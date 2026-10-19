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
