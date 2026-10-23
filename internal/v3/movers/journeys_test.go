package movers

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"slices"
	"testing"

	"timeshadedb/internal/v3/core"
)

func TestJourneysSurfaceChangeAndCarryForward(t *testing.T) {
	js := NewJourneys(0)
	nauvis := core.SurfaceKey{Map: "gw-a", Surface: "nauvis"}
	gleba := core.SurfaceKey{Map: "gw-a", Surface: "gleba"}
	full := core.MoverMeta{ID: "1.5.9", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 3, Kinds: "LCC", Color: "ff0000"}
	bare := core.MoverMeta{ID: "1.5.9", Kind: core.KindTrain, Force: "player", Name: "locomotive"}
	out := js.Move("s1", []core.MoverSample{
		{Surface: nauvis, Meta: full, T: 1000, X: 1},
		{Surface: nauvis, Meta: bare, T: 2000, X: 2},
		{Surface: gleba, Meta: bare, T: 3000, X: 3, Tick: 180},
	}, nil)
	if len(out) != 4 {
		t.Fatalf("out = %+v", out)
	}
	if out[1].Meta != full {
		t.Fatalf("carry forward: %+v", out[1].Meta)
	}
	if g := out[2]; g.Flags&core.FlagGone == 0 || g.Surface != nauvis || g.T != 3000 || g.Meta != full || g.Tick != 180 {
		t.Fatalf("surface change gone = %+v", g)
	}
	if out[3].Surface != gleba || out[3].Meta != full {
		t.Fatalf("sample on the new surface = %+v", out[3])
	}
	// A gone with no meta gets the journey's meta and surface.
	out = js.Move("s1", []core.MoverSample{{Surface: nauvis, Meta: core.MoverMeta{ID: "1.5.9"}, T: 4000, Flags: core.FlagGone}}, nil)
	if len(out) != 1 || out[0].Surface != gleba || out[0].Meta != full {
		t.Fatalf("gone = %+v", out)
	}
	// A second gone (no owner any more) and a gone of an unknown journey are dropped.
	out = js.Move("s1", []core.MoverSample{
		{Surface: nauvis, Meta: core.MoverMeta{ID: "1.5.9"}, T: 4500, Flags: core.FlagGone},
		{Surface: nauvis, Meta: core.MoverMeta{ID: "nope"}, T: 4500, Flags: core.FlagGone},
	}, nil)
	if len(out) != 0 || js.Stats().UnknownGones != 2 {
		t.Fatalf("out = %+v, stats %+v", out, js.Stats())
	}
	// Reappearing on another surface after a gone: no extra gone.
	out = js.Move("s2", []core.MoverSample{{Surface: nauvis, Meta: bare, T: 5000}}, nil)
	if len(out) != 1 || out[0].Flags != 0 || out[0].Meta != full || js.Owner("1.5.9") != "s2" {
		t.Fatalf("resume = %+v", out)
	}
}

// TestJourneysStaleBeforeNewOwner: the old owner's stale sample arrives after its gone
// but before the new owner's first sample. It must not make the old source the owner
// again, or the new owner's samples would be dropped for the rest of the journey.
func TestJourneysStaleBeforeNewOwner(t *testing.T) {
	js := NewJourneys(0)
	sfc := core.SurfaceKey{Map: "gw", Surface: "nauvis"}
	meta := core.MoverMeta{ID: "7.1.2", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 2}
	gone := core.MoverSample{Surface: sfc, Meta: core.MoverMeta{ID: meta.ID}, Flags: core.FlagGone}
	at := func(t core.TimeMs, x float64) core.MoverSample {
		return core.MoverSample{Surface: sfc, Meta: meta, T: t, X: x}
	}
	var out []core.MoverSample
	out = js.Move("a", []core.MoverSample{at(1000, 1)}, out)
	g := gone
	g.T = 2000
	out = js.Move("a", []core.MoverSample{g}, out)
	out = js.Move("a", []core.MoverSample{at(2100, 2)}, out) // stale, from the old owner
	if js.Owner(meta.ID) != "" || js.Stats().Quarantined != 1 {
		t.Fatalf("stale sample claimed the journey: owner %q, stats %+v", js.Owner(meta.ID), js.Stats())
	}
	out = js.Move("b", []core.MoverSample{at(2500, 3), at(3500, 4)}, out)
	if js.Owner(meta.ID) != "b" || len(out) != 4 {
		t.Fatalf("new owner: owner %q, out %+v", js.Owner(meta.ID), out)
	}
	// b hands the journey back to a within the window: allowed, a is not quarantined
	// once b has owned the journey.
	g.T = 4000
	out = js.Move("b", []core.MoverSample{g}, out)
	out = js.Move("a", []core.MoverSample{at(4500, 5)}, out)
	if js.Owner(meta.ID) != "a" || len(out) != 6 {
		t.Fatalf("hand back: owner %q, out %+v", js.Owner(meta.ID), out)
	}
	// The same source resumes its own journey after HandoffWindow.
	g.T = 5000
	out = js.Move("a", []core.MoverSample{g}, out)
	out = js.Move("a", []core.MoverSample{at(5000+core.HandoffWindow, 6)}, out)
	if js.Owner(meta.ID) != "a" || len(out) != 8 || js.Stats().Quarantined != 1 {
		t.Fatalf("resume after the window: owner %q, out %d, stats %+v", js.Owner(meta.ID), len(out), js.Stats())
	}
}

// TestJourneysLateAfterAdopt: the source's last samples and gone arrive after the
// destination's adopt. Those from before the cut are kept; a sample after it is
// dropped and a gone after it is kept just before it; a rolled-back batch leaves the
// previous owner able to send them again.
func TestJourneysLateAfterAdopt(t *testing.T) {
	sfc := core.SurfaceKey{Map: "gw", Surface: "nauvis"}
	meta := core.MoverMeta{ID: "7.1.2", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 22, Kinds: "LC"}
	bare := core.MoverMeta{ID: meta.ID, Kind: core.KindTrain, Force: "player", Name: "locomotive"}
	at := func(t core.TimeMs, x float64) core.MoverSample {
		return core.MoverSample{Surface: sfc, Meta: bare, T: t, X: x, Tick: int64(t) / 10}
	}
	gone := func(t core.TimeMs) core.MoverSample {
		return core.MoverSample{Surface: sfc, Meta: core.MoverMeta{ID: meta.ID}, T: t, Tick: int64(t) / 10, Flags: core.FlagGone}
	}
	js := NewJourneys(0)
	first := at(1000, 1)
	first.Meta = meta
	out := js.Move("a", []core.MoverSample{first}, nil)
	// b adopts at 3000, but its first sample is from 2900: the cut.
	js.Adopt("b", meta.ID, 3000)
	out = js.Move("b", []core.MoverSample{at(2900, 50)}, out)
	js.Mark()
	n := len(out)
	out = js.Move("a", []core.MoverSample{at(2000, 2), at(2950, 3), gone(3100)}, out)
	if len(out) != n+2 {
		t.Fatalf("late rows = %+v", out[n:])
	}
	if s := out[n]; s.T != 2000 || s.Meta != meta {
		t.Fatalf("late sample = %+v (meta carried forward from the source)", s)
	}
	if g := out[n+1]; g.Flags&core.FlagGone == 0 || g.T != 2899 || g.Meta != meta || g.Surface != sfc {
		t.Fatalf("late gone = %+v, want at 2899 with the source's meta", g)
	}
	if g := out[n+1]; g.Flags&core.FlagNoTick == 0 || g.Tick != 0 {
		t.Fatalf("late gone moved before the cut kept its tick: %+v", g)
	}
	if st := js.Stats(); st.LateKept != 2 || st.OwnerDrops != 1 || js.Owner(meta.ID) != "b" {
		t.Fatalf("stats %+v, owner %q", st, js.Owner(meta.ID))
	}
	// Rolled back (the store refused the batch): the retry meets the same state.
	js.Rollback()
	out = js.Move("a", []core.MoverSample{at(2000, 2), gone(2500)}, out[:n])
	if g := out[n+1]; len(out) != n+2 || g.T != 2500 || g.Tick != 250 || g.Flags&core.FlagNoTick != 0 {
		t.Fatalf("after rollback: %+v", out[n:])
	}
	// The gone ends what a may send: a later sample of it is dropped.
	out = js.Move("a", []core.MoverSample{at(2600, 4)}, out)
	if len(out) != n+2 || js.Owner(meta.ID) != "b" {
		t.Fatalf("after the late gone: %+v, owner %q", out[n:], js.Owner(meta.ID))
	}
	// Another source gets nothing from the adopt.
	out = js.Move("c", []core.MoverSample{at(2000, 9)}, out)
	if len(out) != n+2 {
		t.Fatalf("a third source's sample was kept")
	}
}

// TestJourneysAdoptUnknown: after a server restart (the Clusterio controller restarts
// its sidecar) the destination's adopt can come before anything of the journey. The
// first other source with samples from before the cut is taken as the previous owner;
// a gone first gets its meta from the store (Lookup).
func TestJourneysAdoptUnknown(t *testing.T) {
	sfc := core.SurfaceKey{Map: "gw", Surface: "nauvis"}
	meta := core.MoverMeta{ID: "7.1.2", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 3}
	at := func(t core.TimeMs) core.MoverSample { return core.MoverSample{Surface: sfc, Meta: meta, T: t} }
	gone := func(t core.TimeMs) core.MoverSample {
		return core.MoverSample{Surface: sfc, Meta: core.MoverMeta{ID: meta.ID}, T: t, Flags: core.FlagGone}
	}
	js := NewJourneys(0)
	js.Adopt("b", meta.ID, 3000)
	out := js.Move("a", []core.MoverSample{at(2000), at(2900), gone(2950)}, nil)
	out = js.Move("c", []core.MoverSample{at(2500)}, out)
	if len(out) != 3 || out[2].Flags&core.FlagGone == 0 || out[2].Meta != meta || js.Owner(meta.ID) != "b" {
		t.Fatalf("out %+v, owner %q", out, js.Owner(meta.ID))
	}
	if st := js.Stats(); st.LateKept != 3 || st.OwnerDrops != 1 {
		t.Fatalf("stats %+v", st)
	}
	// Only the gone arrives late: its meta from the store.
	js = NewJourneys(0)
	js.Lookup = func(k core.SurfaceKey, id string) (core.MoverMeta, bool) { return meta, k == sfc && id == meta.ID }
	js.Adopt("b", meta.ID, 3000)
	out = js.Move("a", []core.MoverSample{gone(3500)}, nil)
	if len(out) != 1 || out[0].T != 2999 || out[0].Meta != meta {
		t.Fatalf("gone = %+v", out)
	}
	// The previous owner's gone came before the adopt: nothing late is taken.
	js = NewJourneys(0)
	js.Move("a", []core.MoverSample{at(1000), gone(2000)}, nil)
	js.Adopt("b", meta.ID, 3000)
	if out = js.Move("a", []core.MoverSample{at(2100)}, nil); len(out) != 0 {
		t.Fatalf("a sample after its own gone was kept: %+v", out)
	}
}

// TestJourneysAfterGone: the gone arrives before the adopt, and the next owner's first
// samples are a few ms earlier (its clock anchored on its own). They are moved just
// after the gone; a larger overlap is left as it is, and so is the old owner resuming.
func TestJourneysAfterGone(t *testing.T) {
	sfc := core.SurfaceKey{Map: "gw", Surface: "nauvis"}
	meta := core.MoverMeta{ID: "7.1.2", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 3}
	at := func(t core.TimeMs) core.MoverSample { return core.MoverSample{Surface: sfc, Meta: meta, T: t} }
	gone := func(t core.TimeMs) core.MoverSample {
		return core.MoverSample{Surface: sfc, Meta: core.MoverMeta{ID: meta.ID}, T: t, Flags: core.FlagGone}
	}
	times := func(out []core.MoverSample) []core.TimeMs {
		var ts []core.TimeMs
		for _, s := range out {
			ts = append(ts, s.T)
		}
		return ts
	}
	js := NewJourneys(0)
	out := js.Move("a", []core.MoverSample{at(1000), gone(3000)}, nil)
	js.Adopt("b", meta.ID, 3010)
	out = js.Move("b", []core.MoverSample{at(2995), at(2998), at(3100), at(3050)}, out)
	if got := times(out); !slices.Equal(got, []core.TimeMs{1000, 3000, 3001, 3002, 3100, 3050}) {
		t.Fatalf("times %v", got)
	}
	if st := js.Stats(); st.AfterGone != 2 {
		t.Fatalf("stats %+v", st)
	}
	// Without an adopt (a gridworld crossing), and with an overlap past goneSlack.
	js = NewJourneys(0)
	out = js.Move("a", []core.MoverSample{at(1000), gone(3000)}, nil)
	out = js.Move("b", []core.MoverSample{at(2999)}, out)
	if got := times(out); !slices.Equal(got, []core.TimeMs{1000, 3000, 3001}) {
		t.Fatalf("no adopt: times %v", got)
	}
	js = NewJourneys(0)
	out = js.Move("a", []core.MoverSample{at(1000), gone(3000)}, nil)
	out = js.Move("b", []core.MoverSample{at(2000), at(2999)}, out)
	if got := times(out); !slices.Equal(got, []core.TimeMs{1000, 3000, 2000, 2999}) {
		t.Fatalf("large overlap: times %v", got)
	}
	// The old owner resuming after the handoff window keeps its times.
	js = NewJourneys(0)
	out = js.Move("a", []core.MoverSample{at(1000), gone(3000), at(3000 + core.HandoffWindow)}, nil)
	if got := times(out); !slices.Equal(got, []core.TimeMs{1000, 3000, 3000 + core.HandoffWindow}) {
		t.Fatalf("resume: times %v", got)
	}
}

func TestJourneysResyncAndStopped(t *testing.T) {
	js := NewJourneys(0)
	sfc := core.SurfaceKey{Map: "m", Surface: "s"}
	mk := func(id string, k core.MoverKind) core.MoverSample {
		return core.MoverSample{Surface: sfc, Meta: core.MoverMeta{ID: id, Kind: k, Force: "player", Name: id}, T: 100}
	}
	js.Move("a", []core.MoverSample{mk("t1", core.KindTrain), mk("t2", core.KindTrain), mk("p:bob", core.KindPlayer)}, nil)
	js.Move("b", []core.MoverSample{mk("t3", core.KindTrain), mk("p:eve", core.KindPlayer)}, nil)
	js.Begin("a")
	js.Move("a", []core.MoverSample{mk("t1", core.KindTrain)}, nil)
	out := js.Resynced("a", 200, Region{}, nil)
	if len(out) != 2 {
		t.Fatalf("resynced gones = %+v", out)
	}
	ids := map[string]bool{}
	for _, g := range out {
		if g.Flags&core.FlagGone == 0 || g.T != 200 || g.Surface != sfc {
			t.Fatalf("gone = %+v", g)
		}
		ids[g.Meta.ID] = true
	}
	if !ids["t2"] || !ids["p:bob"] {
		t.Fatalf("resync ended %v", ids)
	}
	out = js.Stopped("b", 300, nil)
	if len(out) != 1 || out[0].Meta.ID != "p:eve" || js.Owner("t3") != "b" {
		t.Fatalf("stopped = %+v", out)
	}
	if out := js.Resynced("a", 400, Region{}, nil); len(out) != 0 {
		t.Fatalf("resynced without begin = %+v", out)
	}
}

func TestJourneysBounded(t *testing.T) {
	js := NewJourneys(1000)
	sfc := core.SurfaceKey{Map: "m", Surface: "s"}
	for i := 0; i < 100_000; i++ {
		js.Move("a", []core.MoverSample{{Surface: sfc, Meta: core.MoverMeta{ID: fmt.Sprint(i), Kind: core.KindVehicle}, T: core.TimeMs(i)}}, nil)
	}
	if js.Len() != 1000 || js.Stats().Evicted != 99_000 {
		t.Fatalf("len %d, stats %+v", js.Len(), js.Stats())
	}
	// The most recent journeys are kept.
	if js.Owner("99999") != "a" || js.Owner("0") != "" {
		t.Fatal("wrong journeys evicted")
	}
}

// resyncStore is a mover store holding, on map mapKey's nauvis, journeys from before a
// server restart, and fresh journey rules wired to it as ingest wires them.
func resyncStore(t *testing.T, mapKey string) (*Store, *Journeys, core.SurfaceKey) {
	t.Helper()
	ctx := context.Background()
	m, _ := openTemp(t, Options{SyncInterval: -1, MaxAhead: -1})
	t.Cleanup(func() { m.Close() })
	sfc := core.SurfaceKey{Map: mapKey, Surface: "nauvis"}
	const t0 core.TimeMs = 1_792_000_000_000
	train := func(id string, x, y float64, tt core.TimeMs) core.MoverSample {
		return core.MoverSample{Surface: sfc, Meta: core.MoverMeta{ID: id, Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 22},
			T: tt, X: x, Y: y, Flags: core.FlagStop}
	}
	gone := func(id string, tt core.TimeMs) core.MoverSample {
		return core.MoverSample{Surface: sfc, Meta: core.MoverMeta{ID: id, Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 22},
			T: tt, Flags: core.FlagGone}
	}
	// Closed rows (committed below), then open ones.
	appendAll(t, m, []core.MoverSample{
		train("a.1.1", 179.5, -99, t0), // in tile a, never sent again: ended
		train("a.1.2", 10, 10, t0),     // in tile a, re-sent: stays
		train("b.1.3", 300, 0, t0),     // parked in tile b: stays
		train("b.1.4", 200, 0, t0),     // in tile a, sampled by b since the restart: stays
		train("c.1.5", 100, 0, t0),     // in tile a, adopted by b without a sample: stays
		train("a.1.6", 50, 50, t0),     // in tile a, gone in an open window: not live
		train("a.1.7", 600, 0, t0),     // in tile b, adopted by a without a sample: ended
		train("a.1.8", 60, 60, t0),     // in tile a, ended by a gone: not live
		gone("a.1.8", t0+1000),
	})
	ok(t, m.Commit(ctx, t0+2*core.MoverWindowLen))
	appendAll(t, m, []core.MoverSample{
		gone("a.1.6", t0+2*core.MoverWindowLen+1000),
		{Surface: sfc, Meta: core.MoverMeta{ID: "p:bob", Kind: core.KindPlayer, Force: "player", Name: "bob"},
			T: t0 + 2*core.MoverWindowLen + 2000, X: -20.25, Y: 30}, // in tile a, open rows only: ended
	})
	js := NewJourneys(0)
	js.Lookup = m.LastMeta
	js.Live = func(keep func(core.SurfaceKey, string, float64, float64) bool) []LiveJourney {
		return m.LiveJourneys(mapKey, keep)
	}
	return m, js, sfc
}

// TestJourneysResyncForgotten: after a server restart the journey rules know none of the
// journeys the store has live. A resync of a gridworld instance ends those in its tile
// that it did not re-send, and leaves those in other tiles, re-sent ones, ones another
// source sampled or adopted since the restart, and ones that are not live. A rolled back
// batch ends the same journeys when retried, and once stored they are no longer live.
func TestJourneysResyncForgotten(t *testing.T) {
	m, js, sfc := resyncStore(t, "gw-w")
	const tr core.TimeMs = 1_792_000_000_000 + time15m
	tileA := core.Quad{-256, -256, 256, 256}
	js.Move("b", []core.MoverSample{{Surface: sfc, Meta: core.MoverMeta{ID: "b.1.4", Kind: core.KindTrain}, T: tr - 5000, X: 210, Y: 0}}, nil)
	js.Adopt("b", "c.1.5", tr-4000)
	js.Adopt("a", "a.1.7", tr-4000)
	js.Begin("a")
	resent := js.Move("a", []core.MoverSample{{Surface: sfc, Meta: core.MoverMeta{ID: "a.1.2", Kind: core.KindTrain}, T: tr - 1000, X: 10, Y: 10}}, nil)
	js.Mark()
	out := js.Resynced("a", tr, Region{Bounds: &tileA}, slices.Clone(resent))[len(resent):]
	want := []string{"a.1.1", "a.1.7", "p:bob"}
	var got []string
	for _, g := range out {
		got = append(got, g.Meta.ID)
		if g.Flags != core.FlagGone|core.FlagNoTick || g.T != tr || g.Surface != sfc || g.Meta.Name == "" {
			t.Fatalf("gone = %+v", g)
		}
		if g.Meta.ID == "a.1.1" && g.Meta.Carriages != 22 {
			t.Fatalf("gone meta = %+v", g.Meta)
		}
	}
	if !slices.Equal(got, want) || js.Stats().ResyncForgotten != 3 {
		t.Fatalf("ended %v, want %v (stats %+v)", got, want, js.Stats())
	}
	// The store refused the batch: the retry ends the same journeys.
	js.Rollback()
	if js.Stats().ResyncForgotten != 0 || js.Owner("a.1.7") != "a" {
		t.Fatalf("after rollback: owner %q, stats %+v", js.Owner("a.1.7"), js.Stats())
	}
	js.Mark()
	again := js.Resynced("a", tr, Region{Bounds: &tileA}, slices.Clone(resent))
	js.Commit()
	if !slices.EqualFunc(again[len(resent):], out, func(a, b core.MoverSample) bool { return reflect.DeepEqual(a, b) }) {
		t.Fatalf("retry ended %+v, want %+v", again, out)
	}
	appendAll(t, m, again)
	if got := m.LiveJourneys("gw-w", func(core.SurfaceKey, string, float64, float64) bool { return true }); len(got) != 4 {
		t.Fatalf("live after the gones: %+v", got)
	}
	// The next resync has nothing left to end but the journeys it owns.
	js.Begin("a")
	js.Move("a", []core.MoverSample{{Surface: sfc, Meta: core.MoverMeta{ID: "a.1.2", Kind: core.KindTrain}, T: tr + 1000, X: 10, Y: 10}}, nil)
	if out := js.Resynced("a", tr+2000, Region{Bounds: &tileA}, nil); len(out) != 0 {
		t.Fatalf("second resync ended %+v", out)
	}
}

// TestJourneysResyncForgottenSingleSource: on the map of a single source (a Clusterio
// instance's own map, a standalone game) a resync ends every forgotten journey it did
// not re-send, wherever it is; without a region only the journeys it owns.
func TestJourneysResyncForgottenSingleSource(t *testing.T) {
	_, js, sfc := resyncStore(t, "i-1")
	const tr core.TimeMs = 1_792_000_000_000 + time15m
	js.Begin("a")
	js.Resynced("a", tr, Region{}, nil)
	if js.Stats().ResyncForgotten != 0 {
		t.Fatalf("no region: stats %+v", js.Stats())
	}
	js.Begin("a")
	js.Move("a", []core.MoverSample{{Surface: sfc, Meta: core.MoverMeta{ID: "a.1.2", Kind: core.KindTrain}, T: tr - 1000, X: 10, Y: 10}}, nil)
	var got []string
	for _, g := range js.Resynced("a", tr, Region{All: true}, nil) {
		got = append(got, g.Meta.ID)
	}
	if want := []string{"a.1.1", "a.1.7", "b.1.3", "b.1.4", "c.1.5", "p:bob"}; !slices.Equal(got, want) {
		t.Fatalf("ended %v, want %v", got, want)
	}
}

// appendAll stores samples, all of which must be accepted.
func appendAll(t *testing.T, m *Store, samples []core.MoverSample) {
	t.Helper()
	res, err := m.Append(context.Background(), samples)
	if err != nil || res.Accepted != len(samples) {
		t.Fatalf("append: %+v, %v", res, err)
	}
}

// time15m is 15 minutes after the first samples of resyncStore: the resync.
const time15m = 15 * core.Minute

func TestAnchorJitter(t *testing.T) {
	// A source at 60 UPS whose envelopes arrive 20-400 ms late; the true offset is
	// 1,791,630,000,000 (map time of tick 0).
	const truth = 1_791_630_000_000.0
	rng := rand.New(rand.NewSource(1))
	var a Anchor
	worst := 0.0
	for tick := int64(0); tick < 60*600; tick += 30 { // 10 minutes, an envelope per 0.5 s
		lag := 20 + rng.Float64()*380
		if rng.Intn(50) == 0 {
			lag += 3000 // occasional stall, under the reset threshold
		}
		recv := core.TimeMs(truth + float64(tick)*1000/60 + lag)
		a.Observe(recv, tick, 0)
		if tick > 60*60 { // after a minute of observations
			worst = math.Max(worst, math.Abs(float64(a.T(tick))-(truth+float64(tick)*1000/60)))
		}
	}
	if worst > 40 || a.Resets() != 0 {
		t.Fatalf("worst error %.1f ms, resets %d", worst, a.Resets())
	}
	t.Logf("worst anchor error after 60 s: %.1f ms (arrival lag 20-400 ms, 2%% stalls of +3 s)", worst)
}

// simulateAnchor feeds an anchor envelopes from a server running at ups (game speed 1,
// so 60 ticks per nominal second) every period of real time for dur, arriving 20-400 ms
// late with 2% stalls of +3 s, and returns the worst |T(tick) - true time| after the
// first minute and the number of resets.
func simulateAnchor(ups float64, period, dur core.TimeMs, seed int64) (worst float64, resets int) {
	const truth = 1_791_630_000_000.0
	rng := rand.New(rand.NewSource(seed))
	var a Anchor
	for real := core.TimeMs(0); real < dur; real += period {
		tick := int64(float64(real) * ups / 1000)
		tickTime := truth + float64(tick)*1000/ups
		lag := 20 + rng.Float64()*380
		if rng.Intn(50) == 0 {
			lag += 3000
		}
		a.Observe(core.TimeMs(tickTime+lag), tick, 0)
		if real > core.Minute {
			// The samples of the envelope are a few ticks older than its tick.
			for _, back := range []int64{0, int64(ups / 2)} {
				err := math.Abs(float64(a.T(tick-back)) - (truth + float64(tick-back)*1000/ups))
				worst = math.Max(worst, err)
			}
		}
	}
	return worst, a.Resets()
}

// TestAnchorSlowServer: a busy server runs below 60 UPS; the anchor fits the effective
// rate instead of drifting (PLAN.md §4.1 "re-fits when the offset drifts").
func TestAnchorSlowServer(t *testing.T) {
	for _, ups := range []float64{60, 59.5, 59, 55, 50, 30, 10, 3} {
		for _, period := range []core.TimeMs{core.Second, core.Second / 4} {
			worst, resets := 0.0, 0
			for seed := int64(0); seed < 5; seed++ {
				w, r := simulateAnchor(ups, period, 10*core.Minute, seed*1000+int64(ups*10)+int64(period))
				worst, resets = math.Max(worst, w), resets+r
			}
			t.Logf("%5.1f UPS, envelope every %4d ms: worst error %5.1f ms over 5 runs, %d resets", ups, period, worst, resets)
			if worst > 150 || resets != 0 {
				t.Errorf("%.1f UPS every %d ms: worst error %.1f ms, %d resets", ups, period, worst, resets)
			}
		}
	}
}

// TestAnchorRateChange: the UPS steps between 60 and 40 halfway (a factory grows, or a
// load spike ends); the anchor follows without a reset.
func TestAnchorRateChange(t *testing.T) {
	const truth = 1_791_630_000_000.0
	for _, c := range []struct{ from, to float64 }{{60, 40}, {40, 60}} {
		rng := rand.New(rand.NewSource(3))
		var a Anchor
		tick, tickTime := int64(0), truth
		settled, transient := 0.0, 0.0
		for real := core.TimeMs(0); real < 10*core.Minute; real += 500 {
			ups := c.from
			if real >= 5*core.Minute {
				ups = c.to
			}
			tick += int64(ups / 2)
			tickTime += 500
			a.Observe(core.TimeMs(tickTime+20+rng.Float64()*380), tick, 0)
			err := math.Abs(float64(a.T(tick)) - tickTime)
			switch {
			case real >= 5*core.Minute && real <= 6*core.Minute:
				transient = math.Max(transient, err)
			case real > core.Minute:
				settled = math.Max(settled, err)
			}
		}
		t.Logf("%.0f -> %.0f UPS: worst error %.1f ms outside the minute after the change, %.0f ms within it; UPS %.2f",
			c.from, c.to, settled, transient, a.UPS())
		if settled > 60 || transient > 3000 || a.Resets() != 0 || math.Abs(a.UPS()-c.to) > 0.5 {
			t.Fatalf("%.0f -> %.0f: worst error %.1f ms settled, %.1f ms transient, resets %d, UPS %.2f",
				c.from, c.to, settled, transient, a.Resets(), a.UPS())
		}
	}
}

func TestAnchorResets(t *testing.T) {
	var a Anchor
	a.Observe(10_000, 0, 0)
	a.Observe(11_000, 60, 0)
	if got := a.T(60); got != 11_000 {
		t.Fatalf("T(60) = %d", got)
	}
	// Paused for 20 s: the tick did not advance; obs jumps by 20 s -> reset.
	a.Observe(31_000, 60, 0)
	if got := a.T(60); got != 31_000 || a.Resets() != 1 {
		t.Fatalf("after pause T(60) = %d, resets %d", got, a.Resets())
	}
	// Save reloaded: tick goes backwards -> reset.
	a.Observe(40_000, 30, 0)
	if got := a.T(30); got != 40_000 || a.Resets() != 2 {
		t.Fatalf("after reload T(30) = %d, resets %d", got, a.Resets())
	}
	// Speed 2: 120 ticks per second from tick 30; the clock continues.
	a.Observe(41_000, 90, 2)
	if got := a.T(90); got != 41_000 {
		t.Fatalf("speed change T(90) = %d", got)
	}
	a.Observe(42_000, 210, 2)
	if got, want := a.T(330), core.TimeMs(43_000); got != want {
		t.Fatalf("at speed 2 T(330) = %d, want %d", got, want)
	}
	// A lower-latency observation lowers the offset at once; old minima expire after
	// AnchorWindow (and leave the rate fit after AnchorRateWindow).
	var b Anchor
	b.Observe(1_000_500, 0, 0)
	b.Observe(1_001_100, 60, 0) // obs 1,000,100
	if b.Offset() != 1_000_100 {
		t.Fatalf("offset = %v", b.Offset())
	}
	for s := int64(2); s <= 200; s++ { // then every envelope exactly 300 ms late
		b.Observe(core.TimeMs(1_000_100+s*1000+300), s*60, 0)
	}
	if got := b.T(200 * 60); got != 1_000_100+200_000+300 {
		t.Fatalf("T after expiry = %d, want %d", got, 1_000_100+200_000+300)
	}
}

func TestAnchorsBounded(t *testing.T) {
	as := NewAnchors(10)
	for i := 0; i < 100; i++ {
		as.Observe(fmt.Sprint("src", i), 1000, 0, 0)
	}
	n := 0
	as.Offsets(func(string, float64) { n++ })
	if n != 10 || as.Get("src99") == nil || as.Get("src0") != nil {
		t.Fatalf("anchors kept %d", n)
	}
}
