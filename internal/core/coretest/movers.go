package coretest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"timeshadedb/internal/core"
)

// MoverFactory opens a new, empty mover store. The test closes it.
type MoverFactory func(t *testing.T) core.MoverStore

// SFC is the surface the mover contract tests use.
var SFC = core.SurfaceKey{Map: "gw-test.1", Surface: "nauvis"}

// T0 is an hour-aligned base time (2026-10-10T10:00:00Z).
const T0 core.TimeMs = 1_791_630_000_000

// Train is the meta of the test train.
var Train = core.MoverMeta{ID: "248495586.100.7", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 3, Kinds: "LCC", Color: "f20000"}

func sample(meta core.MoverMeta, t core.TimeMs, x, y, o float64) core.MoverSample {
	return core.MoverSample{Surface: SFC, Meta: meta, T: t, Tick: int64(t-T0) * 60 / 1000, X: x, Y: y, Orientation: o}
}

func gone(meta core.MoverMeta, t core.TimeMs) core.MoverSample {
	return core.MoverSample{Surface: SFC, Meta: meta, T: t, Tick: int64(t-T0) * 60 / 1000, Flags: core.FlagGone}
}

func rowOf(s core.MoverSample, mover int) core.MoverRow {
	q := s.Quantized()
	return core.MoverRow{Mover: mover, T: q.T, Tick: q.Tick, X: q.X, Y: q.Y, Orientation: q.Orientation, Flags: q.Flags, Carriages: q.Carriages,
		Speed: q.Speed, Rail: q.Rail, RailStart: q.RailStart}
}

func sameRows(t *testing.T, what string, got, want []core.MoverRow) {
	t.Helper()
	norm := func(rows []core.MoverRow) []core.MoverRow {
		out := make([]core.MoverRow, len(rows))
		for i, r := range rows {
			if len(r.Carriages) == 0 {
				r.Carriages = nil
			}
			if len(r.Rail) == 0 {
				r.Rail = nil
			}
			out[i] = r
		}
		return out
	}
	if !reflect.DeepEqual(norm(got), norm(want)) {
		t.Fatalf("%s rows:\n got  %+v\n want %+v", what, got, want)
	}
}

// MoverStore runs the mover contract tests.
func MoverStore(t *testing.T, open MoverFactory) {
	ctx := context.Background()
	W := core.MoverWindowLen

	t.Run("WindowsAndQuantization", func(t *testing.T) {
		m := open(t)
		defer m.Close()
		a := sample(Train, T0+1000, 10.03, 20, 0.25)
		a.Flags = core.FlagStop
		a.Carriages = []core.Carriage{{X: 10.03, Y: 20, Orientation: 0.25}, {X: 10.03, Y: 27, Orientation: 0.25}, {X: 10, Y: 34.01, Orientation: 0.2501}}
		b := sample(Train, T0+2000, 14, 20, 0.25)
		c := sample(Train, T0+W+1000, 30, 20, 0.25)
		res := get(m.Append(ctx, []core.MoverSample{b, a, c}))(t)
		if res != (core.ApplyResult{Accepted: 3}) {
			t.Fatalf("append = %+v", res)
		}
		ok(t, m.Commit(ctx, T0+W))
		w := get(m.Window(ctx, SFC, T0))(t)
		if w.Kind != core.WindowClosed || w.Start != T0 || w.Len != W || w.Surface != SFC {
			t.Fatalf("window header = %+v", w)
		}
		if len(w.Movers) != 1 || w.Movers[0] != Train {
			t.Fatalf("mover table = %+v", w.Movers)
		}
		sameRows(t, "window", w.Rows, []core.MoverRow{rowOf(a, 0), rowOf(b, 0)})
		if w.Rows[0].X != 10.0 || w.Rows[0].Carriages[2].Y != 34.0 {
			t.Fatalf("quantization: %+v", w.Rows[0])
		}
		if _, err := m.Window(ctx, SFC, T0+W); !errors.Is(err, core.ErrNoVersion) {
			t.Fatalf("open window served: %v", err)
		}
		day := get(m.Day(ctx, SFC, core.UTCDay(T0)))(t)
		want := core.DayIndex{Date: core.UTCDay(T0), Windows: []core.WindowRef{{Start: T0, First: T0 + 1000, Last: T0 + 2000, Rows: 2}}, Keyframes: []core.TimeMs{}}
		if !reflect.DeepEqual(day, want) {
			t.Fatalf("day = %+v, want %+v", day, want)
		}
		sfcs := get(m.Surfaces(ctx))(t)
		if len(sfcs) != 1 || sfcs[0].Key != SFC || sfcs[0].ID != SFC.ID() || sfcs[0].Horizon != T0+W ||
			sfcs[0].FirstT != T0+1000 || sfcs[0].LastT != T0+W+1000 || !reflect.DeepEqual(sfcs[0].Days, []string{core.UTCDay(T0)}) {
			t.Fatalf("surfaces = %+v", sfcs)
		}
	})

	t.Run("RailRows", func(t *testing.T) {
		m := open(t)
		defer m.Close()
		a := sample(Train, T0+1000, 10, 20, 0.25)
		a.Flags = core.FlagRail
		a.Speed, a.RailStart = 0.5, true
		a.Rail = []core.RailPiece{{X: -3, Y: 21, Dir: 12, Kind: core.RailKindCurveA}, {X: 2, Y: 19, Dir: 4, Kind: core.RailKindRamp}}
		b := sample(Train, T0+2000, 40, 20, 0.25)
		b.Flags = core.FlagRail | core.FlagStop
		c := sample(Train, T0+3000, 40, 20, 0.25) // an older capture's row
		ok(t, func() error { _, err := m.Append(ctx, []core.MoverSample{a, b, c}); return err }())
		ok(t, m.Commit(ctx, T0+W))
		w := get(m.Window(ctx, SFC, T0))(t)
		sameRows(t, "window", w.Rows, []core.MoverRow{rowOf(a, 0), rowOf(b, 0), rowOf(c, 0)})
		if w.Rows[0].Speed != 0.5 || !w.Rows[0].RailStart || len(w.Rows[0].Rail) != 2 || w.Rows[2].Flags&core.FlagRail != 0 {
			t.Fatalf("rail rows: %+v", w.Rows)
		}
	})

	t.Run("DedupeLateInvalid", func(t *testing.T) {
		m := open(t)
		defer m.Close()
		get(m.Append(ctx, []core.MoverSample{sample(Train, T0+1000, 1, 1, 0), sample(Train, T0+1000, 2, 2, 0)}))(t)
		ok(t, m.Commit(ctx, T0+W))
		w := get(m.Window(ctx, SFC, T0))(t)
		if len(w.Rows) != 1 || w.Rows[0].X != 2 {
			t.Fatalf("duplicate (id, t) not replaced by the later sample: %+v", w.Rows)
		}
		bad := sample(Train, T0+W+1, 0, 0, 0)
		bad.Meta.Kind = "x"
		noID := sample(Train, T0+W+1, 0, 0, 0)
		noID.Meta.ID = ""
		noSfc := sample(Train, T0+W+1, 0, 0, 0)
		noSfc.Surface = core.SurfaceKey{Map: "m"}
		res := get(m.Append(ctx, []core.MoverSample{sample(Train, T0+W-1, 0, 0, 0), bad, noID, noSfc, sample(Train, T0+W, 0, 0, 0)}))(t)
		if res != (core.ApplyResult{Accepted: 1, Late: 1, Invalid: 3}) {
			t.Fatalf("append = %+v", res)
		}
	})

	t.Run("HandoffAndMetaChange", func(t *testing.T) {
		m := open(t)
		defer m.Close()
		other := Train
		other.ID = "248495586.200.9"
		reversed := Train
		reversed.Name, reversed.Kinds = "cargo-wagon", "CCl"
		get(m.Append(ctx, []core.MoverSample{
			sample(Train, T0+1000, 250, 0, 0.25),
			gone(Train, T0+5000),
			sample(Train, T0+8000, 260, 0, 0.25), // within 10 s: handoff
			sample(reversed, T0+9000, 255, 0, 0.75),
			sample(other, T0+1000, 0, 0, 0),
			gone(other, T0+5000),
			sample(other, T0+20000, 0, 0, 0), // 15 s later: an end, then a new start
		}))(t)
		ok(t, m.Commit(ctx, T0+W))
		w := get(m.Window(ctx, SFC, T0))(t)
		if len(w.Movers) != 3 || w.Movers[0] != Train || w.Movers[1] != reversed || w.Movers[2] != other {
			t.Fatalf("mover table = %+v", w.Movers)
		}
		var flags []core.MoverFlags
		for _, r := range w.Rows {
			flags = append(flags, r.Flags)
		}
		want := []core.MoverFlags{0, core.FlagGone | core.FlagHandoff, 0, 0, 0, core.FlagGone, 0}
		if !reflect.DeepEqual(flags, want) {
			t.Fatalf("flags = %v, want %v", flags, want)
		}
		if w.Rows[1].X != 0 || w.Rows[1].Carriages != nil {
			t.Fatalf("gone row carries a position: %+v", w.Rows[1])
		}
	})

	t.Run("KeyframesAndDays", func(t *testing.T) {
		m := open(t)
		defer m.Close()
		b := Train
		b.ID = "248495586.300.11"
		a1 := sample(Train, T0+1000, 5, 5, 0.5)
		a1.Carriages = []core.Carriage{{X: 5, Y: 5, Orientation: 0.5}}
		get(m.Append(ctx, []core.MoverSample{a1, sample(b, T0+2000, 1, 1, 0), gone(b, T0+3000)}))(t)
		ok(t, m.Commit(ctx, T0+core.Hour))
		if _, err := m.Keyframe(ctx, SFC, T0); !errors.Is(err, core.ErrNoVersion) {
			t.Fatalf("keyframe before the first sample: %v", err)
		}
		k := get(m.Keyframe(ctx, SFC, T0+core.Hour))(t)
		if k.Kind != core.WindowKeyframe || k.Start != T0+core.Hour || k.Len != 0 || len(k.Movers) != 1 || k.Movers[0] != Train {
			t.Fatalf("keyframe = %+v", k)
		}
		sameRows(t, "keyframe", k.Rows, []core.MoverRow{rowOf(a1, 0)})
		ok(t, m.Commit(ctx, T0+3*core.Hour))
		day := get(m.Day(ctx, SFC, core.UTCDay(T0)))(t)
		if !reflect.DeepEqual(day.Keyframes, []core.TimeMs{T0 + core.Hour, T0 + 2*core.Hour, T0 + 3*core.Hour}) || day.Final {
			t.Fatalf("day = %+v", day)
		}
		k3 := get(m.Keyframe(ctx, SFC, T0+3*core.Hour))(t)
		sameRows(t, "keyframe 3", k3.Rows, []core.MoverRow{rowOf(a1, 0)})
		dayStart, _ := core.ParseUTCDay(core.UTCDay(T0))
		ok(t, m.Commit(ctx, dayStart+core.Day))
		if day := get(m.Day(ctx, SFC, core.UTCDay(T0)))(t); !day.Final {
			t.Fatalf("day not final after the horizon passed midnight: %+v", day)
		}
		if _, err := m.Day(ctx, SFC, "2001-01-01"); !errors.Is(err, core.ErrNoVersion) {
			t.Fatalf("empty day err = %v", err)
		}
	})

	t.Run("Live", func(t *testing.T) {
		m := open(t)
		defer m.Close()
		now := T0 + 10*core.Minute
		a, b, c := Train, Train, Train
		b.ID, c.ID = "b", "c"
		old := sample(a, now-70_000, 1, 1, 0)
		recent := sample(a, now-30_000, 2, 2, 0)
		parked := sample(b, now-120_000, 3, 3, 0)
		parked.Flags = core.FlagStop
		future := sample(a, now+1000, 9, 9, 0)
		get(m.Append(ctx, []core.MoverSample{old, recent, parked, sample(c, now-300_000, 4, 4, 0), gone(c, now-200_000), future}))(t)
		ok(t, m.Commit(ctx, now))
		l := get(m.Live(ctx, SFC, now))(t)
		if l.Kind != core.WindowLive || l.Start != now || l.Len != core.LiveTrail {
			t.Fatalf("live header = %+v", l)
		}
		if len(l.Movers) != 2 || l.Movers[0].ID != Train.ID || l.Movers[1].ID != "b" {
			t.Fatalf("live movers = %+v", l.Movers)
		}
		sameRows(t, "live", l.Rows, []core.MoverRow{rowOf(recent, 0), rowOf(parked, 1)})
	})

	t.Run("Errors", func(t *testing.T) {
		m := open(t)
		defer m.Close()
		other := core.SurfaceKey{Map: "x", Surface: "y"}
		if _, err := m.Window(ctx, other, T0); !errors.Is(err, core.ErrUnknownDatastore) {
			t.Fatalf("unknown surface err = %v", err)
		}
		if _, err := m.Live(ctx, other, T0); !errors.Is(err, core.ErrUnknownDatastore) {
			t.Fatalf("unknown surface live err = %v", err)
		}
	})
}
