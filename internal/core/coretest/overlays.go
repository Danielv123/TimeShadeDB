package coretest

import (
	"context"
	"errors"
	"testing"

	"timeshadedb/internal/core"
)

// OverlayFactory opens a new, empty overlay store. The test closes it.
type OverlayFactory func(t *testing.T) core.OverlayStore

// OverlayStore runs the overlay contract tests.
func OverlayStore(t *testing.T, open OverlayFactory) {
	ctx := context.Background()
	tile := core.TileAt(793, 567, 0)
	tag := core.Interval{Kind: core.OverlayTag, Key: "248495586/12", Instance: 248495586, X: 793, Y: 567, Icon: "item/iron-ore", Text: "21M"}
	recipe := core.Interval{Kind: core.OverlayRecipe, Key: "248495586/90001", Instance: 248495586, X: 800.5, Y: 570.5, Recipe: "iron-plate", Quality: "normal"}
	rail := core.Interval{Kind: core.OverlayRail, Key: "248495586/90002", Instance: 248495586, X: 801, Y: 571, Name: "straight-rail", Direction: 4}

	t.Run("Lifecycle", func(t *testing.T) {
		o := open(t)
		defer o.Close()
		if v := get(o.Version(ctx, DS, tile))(t); v != 0 {
			t.Fatalf("empty tile version = %d", v)
		}
		changed := tag
		changed.Text = "12M"
		res := get(o.Apply(ctx, []core.OverlayEvent{
			{DS: DS, T: 100, Interval: tag},
			{DS: DS, T: 150, Interval: tag}, // same content: no-op
			{DS: DS, T: 200, Interval: changed},
			{DS: DS, T: 120, Interval: recipe},
			{DS: DS, T: 300, Close: true, Interval: core.Interval{Kind: core.OverlayRecipe, Key: recipe.Key}},
			{DS: DS, T: 130, Interval: rail},
		}))(t)
		if res != (core.ApplyResult{Accepted: 6}) {
			t.Fatalf("apply = %+v", res)
		}
		all := get(o.Query(ctx, DS, core.OverlayQuery{Tile: tile, T0: 0, T1: 1000}))(t)
		if len(all) != 4 {
			t.Fatalf("query all = %+v", all)
		}
		if all[0].Kind != core.OverlayRail || all[1].Kind != core.OverlayRecipe || all[2].Text != "21M" || all[3].Text != "12M" {
			t.Fatalf("order = %+v", all)
		}
		if all[2].Start != 100 || all[2].End != 200 || all[3].Start != 200 || !all[3].Open() || all[1].End != 300 {
			t.Fatalf("interval times = %+v", all)
		}
		at := get(o.Query(ctx, DS, core.OverlayQuery{Tile: tile, T0: 199, Kinds: []core.IntervalKind{core.OverlayTag}}))(t)
		if len(at) != 1 || at[0].Text != "21M" {
			t.Fatalf("point query at 199 = %+v", at)
		}
		at = get(o.Query(ctx, DS, core.OverlayQuery{Tile: tile, T0: 300, Kinds: []core.IntervalKind{core.OverlayRecipe}}))(t)
		if len(at) != 0 {
			t.Fatalf("closed recipe still visible at its end: %+v", at)
		}
		if got := get(o.Query(ctx, DS, core.OverlayQuery{Tile: core.TileCoord{X: 9, Y: 9}, T0: 0, T1: 1000}))(t); len(got) != 0 {
			t.Fatalf("other tile = %+v", got)
		}
		v1 := get(o.Version(ctx, DS, tile))(t)
		if v1 == 0 {
			t.Fatal("version did not change")
		}
		get(o.Apply(ctx, []core.OverlayEvent{{DS: DS, T: 400, Interval: changed}}))(t)
		if v2 := get(o.Version(ctx, DS, tile))(t); v2 != v1 {
			t.Fatalf("no-op event changed the version: %d -> %d", v1, v2)
		}
	})

	t.Run("LateInvalidErrors", func(t *testing.T) {
		o := open(t)
		defer o.Close()
		res := get(o.Apply(ctx, []core.OverlayEvent{
			{DS: DS, T: 100, Interval: tag},
			{DS: DS, T: 50, Interval: tag},
			{DS: DS, T: 60, Close: true, Interval: tag},
			{DS: DS, T: 100, Interval: core.Interval{Kind: "bogus", Key: "1/1"}},
			{DS: DS, T: 100, Interval: core.Interval{Kind: core.OverlayTag}},
			{DS: core.DatastoreKey{}, T: 100, Interval: tag},
			{DS: DS, T: 70, Close: true, Interval: core.Interval{Kind: core.OverlayTag, Key: "never-opened"}},
		}))(t)
		if res != (core.ApplyResult{Accepted: 2, Late: 2, Invalid: 3}) {
			t.Fatalf("apply = %+v", res)
		}
		if _, err := o.Query(ctx, DS, core.OverlayQuery{Tile: core.TileCoord{Z: -1}}); !errors.Is(err, core.ErrBadTile) {
			t.Fatalf("coarse tile err = %v", err)
		}
		other := core.DatastoreKey{Map: "a", Surface: "b", Force: "c"}
		if got, err := o.Query(ctx, other, core.OverlayQuery{Tile: tile}); err != nil || len(got) != 0 {
			t.Fatalf("unknown datastore = %v, %v", got, err)
		}
	})
}
