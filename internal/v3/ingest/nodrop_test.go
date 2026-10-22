package ingest

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
	"timeshadedb/internal/v3/movers"
)

// These tests cover what keeps valid data from being dropped: retries that are
// idempotent (sources' applied envelopes, journey rules rolled back, anchors that see a
// replay), the per-source horizon holds, caught_up envelopes, and the data clock after a
// gap.

// failOnce refuses the n-th Append once, after a pause so the reader has decoded the
// rest of the body meanwhile (as with a real body larger than two batches).
type failOnce struct {
	*memstore.Movers
	calls atomic.Int64
	n     int64
}

func (f *failOnce) Append(ctx context.Context, s []core.MoverSample) (core.ApplyResult, error) {
	if f.calls.Add(1) == f.n {
		time.Sleep(50 * time.Millisecond)
		return core.ApplyResult{}, fmt.Errorf("test: %w", core.ErrOverloaded)
	}
	return f.Movers.Append(ctx, s)
}

func trainEntry(j string, x float64) map[string]any {
	return map[string]any{"j": j, "k": "t", "f": "player", "name": "locomotive", "x": x, "y": 0, "h": 0.25, "n": 1}
}

func moveMsg(entries ...any) map[string]any {
	return map[string]any{"type": "move", "s": "nauvis", "m": entries}
}

func seq(n int64) *int64 { return &n }

// journeyRows commits everything and returns the stored rows of journey id.
func journeyRows(t *testing.T, mov *memstore.Movers, id string) []core.MoverRow {
	t.Helper()
	ctx := context.Background()
	if err := mov.Commit(ctx, base+10*core.Hour); err != nil {
		t.Fatal(err)
	}
	sfc := core.SurfaceKey{Map: "m", Surface: "nauvis"}
	var out []core.MoverRow
	for ws := core.WindowStart(base-core.Hour, core.MoverWindowLen); ws < base+5*core.Hour; ws += core.MoverWindowLen {
		w, err := mov.Window(ctx, sfc, ws)
		if err != nil || w == nil {
			continue
		}
		for _, r := range w.Rows {
			if w.Movers[r.Mover].ID == id {
				out = append(out, r)
			}
		}
	}
	return out
}

func newRetryIngester(t *testing.T, mov core.MoverStore, now core.TimeMs) (*Ingester, *Sources) {
	t.Helper()
	maps, _ := OpenMaps(t.TempDir())
	srcs, err := OpenSources("", time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := New(Stores{Terrain: memstore.NewTerrain(), Movers: mov, Overlays: memstore.NewOverlays(), Maps: maps},
		Options{FlushSamples: 1, Sources: srcs, Clock: func() core.TimeMs { return now }})
	return in, srcs
}

// TestRetryKeepsGone: a store refuses the second batch of a body once, and the forwarder
// posts the whole body again. The journey rules of the refused batch were rolled back and
// the first envelope (applied) is skipped, so the gone of journey 1.1.1 is stored and
// nothing is counted as an unknown gone or quarantined.
func TestRetryKeepsGone(t *testing.T) {
	mov := &failOnce{Movers: memstore.NewMovers(), n: 2}
	in, _ := newRetryIngester(t, mov, base+10*core.Second)
	var body strings.Builder
	body.WriteString(line(t, env{V: 1, Tick: 60, Src: "s", Map: "m", RecvMs: int64(base), Seq: seq(1), Msgs: []any{moveMsg(trainEntry("1.1.1", 1))}}))
	body.WriteString(line(t, env{V: 1, Tick: 120, Src: "s", Map: "m", RecvMs: int64(base + 1000), Seq: seq(2), Msgs: []any{moveMsg(trainEntry("1.1.2", 1))}}))
	body.WriteString(line(t, env{V: 1, Tick: 180, Src: "s", Map: "m", RecvMs: int64(base + 2000), Seq: seq(3), Msgs: []any{moveMsg(map[string]any{"j": "1.1.1", "gone": true})}}))
	body.WriteString(line(t, env{V: 1, Tick: 240, Src: "s", Map: "m", RecvMs: int64(base + 3000), Seq: seq(4), Msgs: []any{moveMsg(trainEntry("1.1.2", 2))}}))
	if _, err := in.Ingest(context.Background(), strings.NewReader(body.String())); err == nil {
		t.Fatal("the refused batch was not reported")
	}
	resp, err := in.Ingest(context.Background(), strings.NewReader(body.String()))
	if err != nil {
		t.Fatal(err)
	}
	st := in.Stats()
	if resp.Duplicates != 1 || resp.Samples.Accepted != 3 || st.UnknownGones != 0 || st.Quarantined != 0 {
		t.Fatalf("retry: resp %+v, stats %+v", resp, st)
	}
	rows := journeyRows(t, mov.Movers, "1.1.1")
	if len(rows) != 2 || rows[1].Flags&core.FlagGone == 0 {
		t.Fatalf("journey 1.1.1 rows = %+v, want a sample and its gone", rows)
	}
	if rows := journeyRows(t, mov.Movers, "1.1.2"); len(rows) != 2 {
		t.Fatalf("journey 1.1.2 rows = %d, want 2", len(rows))
	}
}

// TestRetryKeepsTimes: a body refused once and posted again stores every sample once, at
// the time the first attempt gave it: the applied envelopes are skipped, and the others
// are timed on the anchor's line (a replay, not a reloaded game).
func TestRetryKeepsTimes(t *testing.T) {
	mov := &failOnce{Movers: memstore.NewMovers(), n: 1 << 40}
	in, _ := newRetryIngester(t, mov, base+10*core.Minute)
	lags := []int64{50, 400, 800, 300, 600}
	mk := func(k int) string {
		return line(t, env{V: 1, Tick: int64(60 * k), Src: "s", Map: "m", RecvMs: int64(base) + int64(k)*1000 + lags[k%len(lags)],
			Seq: seq(int64(k + 1)), Msgs: []any{moveMsg(trainEntry("1.1.1", float64(k)))}})
	}
	var hist, retry strings.Builder
	for k := 0; k < 22; k++ {
		hist.WriteString(mk(k))
	}
	for k := 22; k < 30; k++ {
		retry.WriteString(mk(k))
	}
	if _, err := in.Ingest(context.Background(), strings.NewReader(hist.String())); err != nil {
		t.Fatal(err)
	}
	mov.calls.Store(0)
	mov.n = 3
	if _, err := in.Ingest(context.Background(), strings.NewReader(retry.String())); err == nil {
		t.Fatal("the refused batch was not reported")
	}
	mov.n = 1 << 40
	if _, err := in.Ingest(context.Background(), strings.NewReader(retry.String())); err != nil {
		t.Fatal(err)
	}
	if r := in.anchors.Get("s").Resets(); r != 0 {
		t.Fatalf("anchor resets %d", r)
	}
	rows := journeyRows(t, mov.Movers, "1.1.1")
	if len(rows) != 30 {
		t.Fatalf("%d rows for 30 samples", len(rows))
	}
	for i, r := range rows {
		if r.X != float64(i) {
			t.Fatalf("row %d is sample %v", i, r.X)
		}
	}
}

// TestDuplicateRule: an envelope is a duplicate only when both its seq and its recv_ms
// are at or below its source's last applied ones (a forwarder that restarted with a lower
// seq is not skipped).
func TestDuplicateRule(t *testing.T) {
	s, _ := OpenSources("", time.Hour, nil)
	s.Applied(nil, map[string]seqMark{"a": {seq: 100, recv: base}})
	for _, c := range []struct {
		seq  int64
		recv core.TimeMs
		dup  bool
	}{{100, base, true}, {50, base - 10, true}, {101, base, false}, {50, base + 1, false}} {
		if got := s.Duplicate("a", c.seq, c.recv); got != c.dup {
			t.Fatalf("Duplicate(%d, %d) = %v", c.seq, c.recv, got)
		}
	}
	if s.Duplicate("b", 1, base) {
		t.Fatal("unknown source duplicate")
	}
}

// TestSourcesHold: a source holds each map it feeds at its newest applied time while it
// was heard from within the hold; caught_up raises that to the envelope's receive time
// without touching the clock anchor or the data clock; a reopened table holds every
// source again from the start time.
func TestSourcesHold(t *testing.T) {
	dir := t.TempDir()
	wall := time.UnixMilli(int64(base + core.Hour))
	clock := func() time.Time { return wall }
	srcs, err := OpenSources(dir, 10*time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}
	maps, _ := OpenMaps(t.TempDir())
	mov := memstore.NewMovers()
	dc := &DataClock{}
	in := New(Stores{Terrain: memstore.NewTerrain(), Movers: mov, Overlays: memstore.NewOverlays(), Maps: maps},
		Options{Sources: srcs, Clock: dc.Now, DataClock: dc})
	var body strings.Builder
	for k := 0; k < 5; k++ {
		body.WriteString(line(t, env{V: 1, Tick: int64(60 * (k + 1)), Src: "s", Map: "m", RecvMs: int64(base) + int64(k)*1000, Seq: seq(int64(k + 1)),
			Msgs: []any{moveMsg(trainEntry("1.1.1", float64(k)))}}))
	}
	if _, err := in.Ingest(context.Background(), strings.NewReader(body.String())); err != nil {
		t.Fatal(err)
	}
	h := srcs.Holds()["m"]
	if h < base+3000 || h > base+4000 {
		t.Fatalf("hold after data = base%+d", h-base)
	}
	seen := dc.Seen()
	resp, err := in.Ingest(context.Background(), strings.NewReader(`{"v":1,"tick":0,"src":"s","map":"m","recv_ms":`+fmt.Sprint(int64(base+core.Hour))+`,"seq":6,"caught_up":true,"msgs":[]}`+"\n"))
	if err != nil || resp.Envelopes != 1 {
		t.Fatalf("caught_up: %+v, %v", resp, err)
	}
	if h := srcs.Holds()["m"]; h != base+core.Hour {
		t.Fatalf("hold after caught_up = base%+d", h-base)
	}
	if a := in.anchors.Get("s"); a.Resets() != 0 || dc.Seen() != seen || in.Stats().CaughtUp != 1 {
		t.Fatalf("caught_up touched the anchor (%d resets) or the data clock (%d -> %d)", a.Resets(), seen, dc.Seen())
	}
	// Quiet for longer than the hold: released.
	wall = wall.Add(11 * time.Minute)
	if _, ok := srcs.Holds()["m"]; ok {
		t.Fatal("still held after the hold")
	}
	if err := srcs.Save(); err != nil {
		t.Fatal(err)
	}
	// After a restart every saved source holds again, and its last envelope is known.
	again, err := OpenSources(dir, 10*time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := again.Holds()["m"]; !ok || h != base+core.Hour {
		t.Fatalf("hold after reopen = %v %v", h, ok)
	}
	if !again.Duplicate("s", 6, base+core.Hour) {
		t.Fatal("last envelope forgotten")
	}
}

// TestNewSourceHoldsAtOnce: a source's first envelope holds its map from the moment it is
// read, before its batch is applied, so a commit meanwhile cannot pass its backlog.
func TestNewSourceHoldsAtOnce(t *testing.T) {
	// A source of map messages only (the Clusterio controller) holds nothing.
	ctl, _ := OpenSources("", time.Hour, nil)
	maps0, _ := OpenMaps(t.TempDir())
	in0 := New(Stores{Terrain: memstore.NewTerrain(), Movers: memstore.NewMovers(), Overlays: memstore.NewOverlays(), Maps: maps0},
		Options{Sources: ctl, Clock: func() core.TimeMs { return base + core.Hour }})
	mapMsg := line(t, env{V: 1, Tick: 60, Src: "clusterio:controller", Map: "m", RecvMs: int64(base), Seq: seq(1),
		Msgs: []any{map[string]any{"type": "map", "info": map[string]any{"key": "m", "title": "M"}}}})
	if _, err := in0.Ingest(context.Background(), strings.NewReader(mapMsg)); err != nil {
		t.Fatal(err)
	}
	if h := ctl.Holds(); len(h) != 0 {
		t.Fatalf("map messages hold %v", h)
	}

	srcs, _ := OpenSources("", time.Hour, nil)
	maps, _ := OpenMaps(t.TempDir())
	var held core.TimeMs
	mov := &observeMovers{Movers: memstore.NewMovers(), fn: func() { held = srcs.Holds()["m"] }}
	in := New(Stores{Terrain: memstore.NewTerrain(), Movers: mov, Overlays: memstore.NewOverlays(), Maps: maps},
		Options{Sources: srcs, Clock: func() core.TimeMs { return base + core.Hour }})
	old := line(t, env{V: 1, Tick: 60, Src: "s", Map: "m", RecvMs: int64(base), Seq: seq(1), Msgs: []any{moveMsg(trainEntry("1.1.1", 1))}})
	if _, err := in.Ingest(context.Background(), strings.NewReader(old)); err != nil {
		t.Fatal(err)
	}
	if held != base {
		t.Fatalf("hold while the first batch was applied = base%+d", held-base)
	}
}

type observeMovers struct {
	*memstore.Movers
	fn func()
}

func (o *observeMovers) Append(ctx context.Context, s []core.MoverSample) (core.ApplyResult, error) {
	o.fn()
	return o.Movers.Append(ctx, s)
}

// TestOneRecvCounted: envelopes two seconds apart in ticks with one recv_ms (a forwarder
// that stamped a backlog at its posting time) are counted and reported.
func TestOneRecvCounted(t *testing.T) {
	f := newFixture(t, Options{})
	var body strings.Builder
	for k := 0; k < 60; k++ {
		body.WriteString(line(t, env{V: 1, Tick: int64(130 * k), Src: "s", Map: "m", RecvMs: int64(base),
			Msgs: []any{moveMsg(trainEntry("1.1.1", float64(k)))}}))
	}
	resp := f.ingest(t, body.String())
	if resp.OneRecv != 59 || f.in.Stats().OneRecv != 59 {
		t.Fatalf("one_recv = %d (stats %d), want 59", resp.OneRecv, f.in.Stats().OneRecv)
	}
}

// TestDataClockGap: on the data clock, the first samples after a gap of two hours keep
// their times (they are clamped against their own envelope's recv_ms, not against a data
// clock that moves at most an hour an envelope).
func TestDataClockGap(t *testing.T) {
	maps, _ := OpenMaps(t.TempDir())
	mov := memstore.NewMovers()
	dc := &DataClock{}
	in := New(Stores{Terrain: memstore.NewTerrain(), Movers: mov, Overlays: memstore.NewOverlays(), Maps: maps},
		Options{Clock: dc.Now, DataClock: dc})
	gap := 2 * core.Hour
	var body strings.Builder
	body.WriteString(line(t, env{V: 1, Tick: 60, Src: "s", Map: "m", RecvMs: int64(base), Msgs: []any{moveMsg(trainEntry("1.1.1", 0))}}))
	for k := 0; k < 3; k++ {
		tick := int64(60) + int64(gap)*60/1000 + int64(k*60)
		body.WriteString(line(t, env{V: 1, Tick: tick, Src: "s", Map: "m", RecvMs: int64(base+gap) + int64(k)*1000, Msgs: []any{moveMsg(trainEntry("1.1.1", float64(k+1)))}}))
	}
	if _, err := in.Ingest(context.Background(), strings.NewReader(body.String())); err != nil {
		t.Fatal(err)
	}
	rows := journeyRows(t, mov, "1.1.1")
	if len(rows) != 4 {
		t.Fatalf("%d rows", len(rows))
	}
	for k, r := range rows[1:] {
		if want := base + gap + core.TimeMs(k)*1000; r.T < want-1000 || r.T > want {
			t.Fatalf("sample %d after the gap at base%+d, want about base%+d", k+1, r.T-base, want-base)
		}
	}
}

// TestEndedReleases: an ended envelope (the game exited) releases the source's maps once
// the data before it is applied; data from the source holds them again.
func TestEndedReleases(t *testing.T) {
	srcs, _ := OpenSources("", time.Hour, nil)
	maps, _ := OpenMaps(t.TempDir())
	in := New(Stores{Terrain: memstore.NewTerrain(), Movers: memstore.NewMovers(), Overlays: memstore.NewOverlays(), Maps: maps},
		Options{Sources: srcs, Clock: func() core.TimeMs { return base + core.Hour }})
	data := line(t, env{V: 1, Tick: 60, Src: "s", Map: "m", RecvMs: int64(base), Seq: seq(1), Msgs: []any{moveMsg(trainEntry("1.1.1", 1))}})
	ended := `{"v":1,"tick":60,"src":"s","map":"m","recv_ms":` + fmt.Sprint(int64(base+1000)) + `,"seq":2,"caught_up":true,"ended":true,"msgs":[]}` + "\n"
	if _, err := in.Ingest(context.Background(), strings.NewReader(data+ended)); err != nil {
		t.Fatal(err)
	}
	if h, ok := srcs.Holds()["m"]; ok {
		t.Fatalf("held at base%+d after ended", h-base)
	}
	more := line(t, env{V: 1, Tick: 120, Src: "s", Map: "m", RecvMs: int64(base + 5000), Seq: seq(3), Msgs: []any{moveMsg(trainEntry("1.1.1", 2))}})
	if _, err := in.Ingest(context.Background(), strings.NewReader(more)); err != nil {
		t.Fatal(err)
	}
	if _, ok := srcs.Holds()["m"]; !ok {
		t.Fatal("not held after new data")
	}
}

// TestGoneAfterRestart: the journey rules live in memory, so after a server restart the
// first gone of a journey is one they do not know. It is completed from the mover store's
// last row of the journey (open or closed) instead of being dropped, so the train does
// not stay live for ever; a gone of a journey that already ended stays dropped.
func TestGoneAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mov, err := movers.Open(dir, movers.Options{SyncInterval: -1, MaxAhead: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer mov.Close()
	maps, _ := OpenMaps(t.TempDir())
	newIn := func() *Ingester {
		return New(Stores{Terrain: memstore.NewTerrain(), Movers: mov, Overlays: memstore.NewOverlays(), Maps: maps},
			Options{Clock: func() core.TimeMs { return base + core.Hour }})
	}
	body := line(t, env{V: 1, Tick: 60, Src: "s", Map: "m", RecvMs: int64(base), Msgs: []any{moveMsg(trainEntry("1.1.1", 1), trainEntry("1.1.2", 1))}}) +
		line(t, env{V: 1, Tick: 120, Src: "s", Map: "m", RecvMs: int64(base + 1000), Msgs: []any{moveMsg(map[string]any{"j": "1.1.2", "gone": true})}})
	if _, err := newIn().Ingest(ctx, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	// 1.1.1 in a closed window, then a "restart" (fresh journey rules).
	if err := mov.Commit(ctx, base+core.MoverWindowLen*2); err != nil {
		t.Fatal(err)
	}
	in := newIn()
	gones := line(t, env{V: 1, Tick: 60 * 600, Src: "s", Map: "m", RecvMs: int64(base + 600_000),
		Msgs: []any{moveMsg(map[string]any{"j": "1.1.1", "gone": true}, map[string]any{"j": "1.1.2", "gone": true})}})
	resp, err := in.Ingest(ctx, strings.NewReader(gones))
	if err != nil {
		t.Fatal(err)
	}
	if st := in.Stats(); resp.Samples.Accepted != 1 || st.ResolvedGones != 1 || st.UnknownGones != 1 {
		t.Fatalf("gones after restart: %+v, stats %+v", resp.Samples, st)
	}
	if err := mov.Commit(ctx, base+core.Hour); err != nil {
		t.Fatal(err)
	}
	live, err := mov.Live(ctx, core.SurfaceKey{Map: "m", Surface: "nauvis"}, base+core.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Rows) != 0 {
		t.Fatalf("live after the gones: %+v", live.Rows)
	}
}

// TestUnknownHolds: maps no source on record feeds are held at their newest data from
// the start; caught_up, an ended source of the map or the hold's end release them, and a
// source's own data takes over the hold. A map a known source feeds gets no unknown hold.
func TestUnknownHolds(t *testing.T) {
	dir := t.TempDir()
	wall := time.UnixMilli(int64(base + 2*core.Hour))
	clock := func() time.Time { return wall }
	known, _ := OpenSources(dir, time.Hour, clock)
	known.Seen("k", "known", base)
	if err := known.Save(); err != nil {
		t.Fatal(err)
	}
	srcs, err := OpenSources(dir, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	srcs.HoldUnknown(map[string]core.TimeMs{"known": base + 50, "a": base + 10, "b": base + 20, "c": base + 30})
	h := srcs.Holds()
	if h["known"] != base || h["a"] != base+10 || h["b"] != base+20 || h["c"] != base+30 {
		t.Fatalf("holds at start %v", h)
	}
	srcs.Seen("s", "a", base+500) // data: s holds a from its first data on
	srcs.Seen("t", "b", base+600) // the same for a caught_up envelope
	srcs.Seen("u", "c", base+700)
	srcs.End("u")
	h = srcs.Holds()
	if h["a"] != base+500 || h["b"] != base+600 {
		t.Fatalf("holds after the sources sent %v", h)
	}
	if _, ok := h["c"]; ok {
		t.Fatalf("c held after its source ended: %v", h)
	}
	if by := srcs.HeldBy(); by["a"][UnknownSource] != 0 || len(by["a"]) != 1 {
		t.Fatalf("held by %v", by)
	}

	again, _ := OpenSources("", time.Hour, clock)
	again.HoldUnknown(map[string]core.TimeMs{"x": base})
	wall = wall.Add(61 * time.Minute)
	if h := again.Holds(); len(h) != 0 {
		t.Fatalf("unknown hold after the hold ended: %v", h)
	}
}

// TestHoldPerMap: a source that stopped feeding one of its maps releases that map after
// the hold, while it keeps holding the map it still feeds.
func TestHoldPerMap(t *testing.T) {
	wall := time.UnixMilli(int64(base + core.Hour))
	clock := func() time.Time { return wall }
	srcs, _ := OpenSources("", time.Hour, clock)
	srcs.Seen("s", "m1", base)
	srcs.Seen("s", "m2", base)
	wall = wall.Add(40 * time.Minute)
	srcs.Seen("s", "m2", base+40*core.Minute)
	wall = wall.Add(30 * time.Minute)
	h := srcs.Holds()
	if _, ok := h["m1"]; ok {
		t.Fatalf("m1 held 70 min after its last data: %v", h)
	}
	if _, ok := h["m2"]; !ok {
		t.Fatalf("m2 released 30 min after its last data: %v", h)
	}
}
