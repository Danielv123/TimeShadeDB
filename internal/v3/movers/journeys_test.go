package movers

import (
	"fmt"
	"math"
	"math/rand"
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
	out := js.Resynced("a", 200, nil)
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
	if out := js.Resynced("a", 400, nil); len(out) != 0 {
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
	// AnchorWindow.
	var b Anchor
	b.Observe(1_000_500, 0, 0)
	b.Observe(1_001_100, 60, 0) // obs 1,000,100
	if b.Offset() != 1_000_100 {
		t.Fatalf("offset = %v", b.Offset())
	}
	b.Observe(1_000_100+70_000+300, 70*60, 0) // 70 s later, 300 ms late: the minimum expired
	if b.Offset() != 1_000_400 {
		t.Fatalf("offset after expiry = %v", b.Offset())
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
