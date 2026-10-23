package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
)

const base core.TimeMs = 1_792_000_000_000 // 2026-10-15

type env struct {
	V      int    `json:"v"`
	Tick   int64  `json:"tick"`
	Src    string `json:"src,omitempty"`
	Map    string `json:"map,omitempty"`
	Inst   int64  `json:"instance,omitempty"`
	RecvMs int64  `json:"recv_ms,omitempty"`
	Seq    *int64 `json:"seq,omitempty"`
	Msgs   []any  `json:"msgs"`
}

func line(t testing.TB, e env) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func chart(v uint16) *core.ChunkPixels {
	var px core.ChunkPixels
	for i := range px {
		px[i] = v
	}
	return &px
}

type fixture struct {
	ter   *memstore.Terrain
	mov   *memstore.Movers
	ovl   *memstore.Overlays
	maps  *Maps
	in    *Ingester
	now   core.TimeMs
	notes *recNotifier
}

type recNotifier struct {
	chunks, samples, maps int
}

func (r *recNotifier) ChunksApplied(u []core.ChunkUpdate)  { r.chunks += len(u) }
func (r *recNotifier) SamplesApplied(s []core.MoverSample) { r.samples += len(s) }
func (r *recNotifier) MapsChanged()                        { r.maps++ }

func newFixture(t *testing.T, opt Options) *fixture {
	t.Helper()
	maps, err := OpenMaps(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ter: memstore.NewTerrain(), mov: memstore.NewMovers(), ovl: memstore.NewOverlays(), maps: maps, now: base + 10*core.Second, notes: &recNotifier{}}
	opt.Clock = func() core.TimeMs { return f.now }
	opt.Notifier = f.notes
	f.in = New(Stores{Terrain: f.ter, Movers: f.mov, Overlays: f.ovl, Maps: maps}, opt)
	return f
}

func (f *fixture) ingest(t *testing.T, body string) core.IngestResponse {
	t.Helper()
	resp, err := f.in.Ingest(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestIngestMapsMessages(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	d := core.EncodeChart(chart(0x1234))
	h := 0.25
	x, y := 10.5, -3.0
	seq := int64(7)
	body := line(t, env{V: 1, Tick: 600, Src: "clusterio:5", Map: "gw-a.1", Inst: 5, RecvMs: int64(base), Seq: &seq, Msgs: []any{
		map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 1, "y": 2, "d": d},
		map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 3, "y": 2},
		map[string]any{"type": "move", "s": "nauvis", "m": []any{
			map[string]any{"j": "5.1.7", "k": "t", "f": "player", "name": "locomotive", "x": 1.5, "y": 2.5, "h": h, "n": 2, "o": "LC", "stop": true,
				"car": [][3]float64{{1.5, 2.5, 0.25}, {1.5, 9.5, 0.25}}},
			map[string]any{"j": "p:bob", "k": "p", "f": "player", "name": "bob", "x": 4, "y": 4, "tick": 590},
		}},
		map[string]any{"type": "tag", "s": "nauvis", "f": "player", "n": 3, "x": x, "y": y, "icon": "item/iron-plate", "text": "hi"},
		map[string]any{"type": "recipe", "s": "nauvis", "f": "player", "u": 99, "x": x, "y": y, "r": "gear", "q": "normal"},
		map[string]any{"type": "map", "info": map[string]any{"key": "gw-a.1", "title": "A", "instances": []any{}}},
		map[string]any{"type": "unknown-future-type", "x": 1},
	}})
	seq2 := int64(8)
	body += line(t, env{V: 1, Tick: 660, Src: "clusterio:5", Map: "gw-a.1", Inst: 5, RecvMs: int64(base + 1000), Seq: &seq2, Msgs: []any{
		map[string]any{"type": "move", "s": "nauvis", "m": []any{map[string]any{"j": "5.1.7", "gone": true}}},
		map[string]any{"type": "tag", "s": "nauvis", "f": "player", "n": 3},
	}})
	resp := f.ingest(t, body)
	if resp.Envelopes != 2 || resp.BadLines != 0 || len(resp.Errors) != 0 {
		t.Fatalf("resp %+v", resp)
	}
	if resp.Chunks.Accepted != 2 || resp.Samples.Accepted != 3 || resp.Overlays.Accepted != 3 {
		t.Fatalf("counts %+v", resp)
	}
	if resp.Seq == nil || *resp.Seq != 8 {
		t.Fatalf("seq %v", resp.Seq)
	}
	if f.notes.chunks != 2 || f.notes.samples != 3 || f.notes.maps != 1 {
		t.Fatalf("notes %+v", f.notes)
	}
	// Terrain: chunk (1,2) holds the chart, (3,2) was deleted, at the anchored time.
	ds := core.DatastoreKey{Map: "gw-a.1", Surface: "nauvis", Force: "player"}
	if err := f.ter.Commit(ctx, base+5*core.Minute); err != nil {
		t.Fatal(err)
	}
	tile, block := core.ChunkCoord{X: 1, Y: 2}.Tile()
	im, _, err := f.ter.ReadTile(ctx, ds, tile, base+5*core.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if px := im.Block(block); px[0] != 0x1234 {
		t.Fatalf("pixel %x", px[0])
	}
	// Movers: tick 600 at recv base is the anchor; the player's tick 590 is 10 ticks
	// (167 ms) earlier; the gone at tick 660 is 1 s later.
	if err := f.mov.Commit(ctx, base+10*core.Minute); err != nil {
		t.Fatal(err)
	}
	w, err := f.mov.Window(ctx, core.SurfaceKey{Map: "gw-a.1", Surface: "nauvis"}, core.WindowStart(base, core.MoverWindowLen))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]core.MoverRow{}
	for _, r := range w.Rows {
		got[w.Movers[r.Mover].ID] = append(got[w.Movers[r.Mover].ID], r)
	}
	tr := got["5.1.7"]
	if len(tr) != 2 || tr[0].T != base || tr[0].Tick != 600 || tr[0].Flags&core.FlagStop == 0 || len(tr[0].Carriages) != 2 || tr[0].Orientation != 0.25 {
		t.Fatalf("train rows %+v", tr)
	}
	if tr[1].Flags&core.FlagGone == 0 || tr[1].T != base+1000 {
		t.Fatalf("gone row %+v", tr[1])
	}
	pl := got["p:bob"]
	if len(pl) != 1 || pl[0].T != base-167 || pl[0].Flags&core.FlagNoHeading == 0 {
		t.Fatalf("player rows %+v", pl)
	}
	// Overlays: the tag was opened and closed, keyed by instance.
	ivs, err := f.ovl.Query(ctx, ds, core.OverlayQuery{Tile: core.TileAt(x, y, 0), T0: 0, T1: base + core.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var tag *core.Interval
	for i := range ivs {
		if ivs[i].Kind == core.OverlayTag {
			tag = &ivs[i]
		}
	}
	if tag == nil || tag.Key != "5/3" || tag.Instance != 5 || tag.End != base+1000 || tag.Icon != "item/iron-plate" {
		t.Fatalf("tag %+v (all %+v)", tag, ivs)
	}
	// Maps: stored in maps.json.
	b, err := os.ReadFile(filepath.Join(filepath.Dir(f.maps.path), "maps.json"))
	if err != nil || !strings.Contains(string(b), `"title": "A"`) {
		t.Fatalf("maps.json %s %v", b, err)
	}
	if again, _ := OpenMaps(filepath.Dir(f.maps.path)); len(again.list) != 1 {
		t.Fatal("maps.json does not reload")
	}
}

// TestIngestRailSamples: a train sample with a speed is a rail path sample (FlagRail) with
// its pieces; one with a piece that is not valid starts a new rail path instead; without
// a speed it is an older capture's sample.
func TestIngestRailSamples(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	train := func(j string, x float64, extra map[string]any) map[string]any {
		m := map[string]any{"j": j, "k": "t", "f": "player", "name": "locomotive", "x": x, "y": 1, "h": 0.25, "n": 1, "o": "L"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	body := line(t, env{V: 1, Tick: 600, Src: "clusterio:5", Map: "gw-a.1", Inst: 5, RecvMs: int64(base), Msgs: []any{
		map[string]any{"type": "move", "s": "nauvis", "m": []any{
			train("5.1.7", 1, map[string]any{"v": 0.4375, "ru": true, "rp": [][4]int{{-3, 0, 12, 1}, {2, 1, 4, 4}}}),
			train("5.1.8", 1, map[string]any{"v": 0.5, "rp": [][4]int{{-3, 0, 12, 9}}}),
			train("5.1.9", 1, nil),
		}},
	}})
	resp := f.ingest(t, body)
	if resp.Samples.Accepted != 3 || len(resp.Errors) != 0 {
		t.Fatalf("resp %+v", resp)
	}
	if err := f.mov.Commit(ctx, base+10*core.Minute); err != nil {
		t.Fatal(err)
	}
	w, err := f.mov.Window(ctx, core.SurfaceKey{Map: "gw-a.1", Surface: "nauvis"}, core.WindowStart(base, core.MoverWindowLen))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]core.MoverRow{}
	for _, r := range w.Rows {
		got[w.Movers[r.Mover].ID] = r
	}
	a := got["5.1.7"]
	want := []core.RailPiece{{X: -3, Y: 0, Dir: 12, Kind: core.RailKindCurveA}, {X: 2, Y: 1, Dir: 4, Kind: core.RailKindRamp}}
	if a.Flags&core.FlagRail == 0 || a.Speed != 0.4375 || !a.RailStart || !reflect.DeepEqual(a.Rail, want) {
		t.Fatalf("rail row %+v", a)
	}
	if b := got["5.1.8"]; b.Flags&core.FlagRail == 0 || !b.RailStart || len(b.Rail) != 0 {
		t.Fatalf("a bad piece starts a new rail path: %+v", b)
	}
	if c := got["5.1.9"]; c.Flags&core.FlagRail != 0 {
		t.Fatalf("an older capture's row: %+v", c)
	}
}

func TestIngestBadLines(t *testing.T) {
	f := newFixture(t, Options{MaxLine: 300})
	good := line(t, env{V: 1, Tick: 1, Src: "s", Map: "m", RecvMs: int64(base), Msgs: []any{}})
	body := "not json\n" +
		line(t, env{V: 2, Tick: 1, Src: "s", Map: "m", RecvMs: int64(base)}) +
		line(t, env{V: 1, Tick: 1, Map: "m", RecvMs: int64(base)}) +
		`{"v":1,"tick":1,"src":"s","map":"m","recv_ms":1,"msgs":[]` + strings.Repeat(" ", 400) + "}\n" +
		"\n" + good +
		line(t, env{V: 1, Tick: 1, Src: "s", Map: "m", RecvMs: int64(base), Msgs: []any{
			map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 1, "y": 1, "d": "!!"},
			map[string]any{"type": "chunk", "s": "", "f": "player", "x": 1, "y": 1},
		}}) +
		strings.TrimSuffix(good, "\n") // last line without a newline
	resp := f.ingest(t, body)
	if resp.BadLines != 4 || resp.Envelopes != 3 {
		t.Fatalf("resp %+v", resp)
	}
	if resp.Chunks.Invalid != 1 || resp.Chunks.Accepted != 0 {
		t.Fatalf("chunks %+v", resp.Chunks)
	}
	if len(resp.Errors) != 6 {
		t.Fatalf("errors %q", resp.Errors)
	}
	st := f.in.Stats()
	if st.BadLines != 4 || st.BadMessages != 2 || st.Envelopes != 3 {
		t.Fatalf("stats %+v", st)
	}
}

func TestIngestClampsFutureTimes(t *testing.T) {
	f := newFixture(t, Options{})
	d := core.EncodeChart(chart(7))
	// recv_ms an hour ahead of the server clock.
	resp := f.ingest(t, line(t, env{V: 1, Tick: 60, Src: "s", Map: "m", RecvMs: int64(f.now + core.Hour), Msgs: []any{
		map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 0, "y": 0, "d": d},
	}}))
	if resp.Chunks.Accepted != 1 || f.in.Stats().Clamped != 1 {
		t.Fatalf("resp %+v stats %+v", resp, f.in.Stats())
	}
	infos, _ := f.ter.Datastores(context.Background())
	if len(infos) != 1 || infos[0].LastT != f.now {
		t.Fatalf("datastores %+v", infos)
	}
}

func TestIngestFlushesInBatches(t *testing.T) {
	f := newFixture(t, Options{FlushChunks: 10})
	var sb strings.Builder
	d := core.EncodeChart(chart(9))
	for i := range 35 {
		sb.WriteString(line(t, env{V: 1, Tick: int64(i), Src: "s", Map: "m", RecvMs: int64(base) + int64(i)*17, Msgs: []any{
			map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": i, "y": 0, "d": d},
		}}))
	}
	resp := f.ingest(t, sb.String())
	if resp.Chunks.Accepted != 35 || f.notes.chunks != 35 {
		t.Fatalf("resp %+v notes %+v", resp, f.notes)
	}
}

// overloadedTerrain refuses every batch.
type overloadedTerrain struct{ *memstore.Terrain }

func (overloadedTerrain) Apply(context.Context, []core.ChunkUpdate) (core.ApplyResult, error) {
	return core.ApplyResult{}, fmt.Errorf("test: %w", core.ErrOverloaded)
}

func TestIngestOverloaded(t *testing.T) {
	maps, _ := OpenMaps(t.TempDir())
	in := New(Stores{Terrain: overloadedTerrain{memstore.NewTerrain()}, Movers: memstore.NewMovers(), Overlays: memstore.NewOverlays(), Maps: maps}, Options{})
	_, err := in.Ingest(context.Background(), strings.NewReader(line(t, env{V: 1, Tick: 1, Src: "s", Map: "m", RecvMs: int64(base), Msgs: []any{
		map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 0, "y": 0},
	}})))
	if !errors.Is(err, core.ErrOverloaded) {
		t.Fatalf("err %v", err)
	}
	if in.Stats().Overloaded != 1 {
		t.Fatal("overload not counted")
	}
}

func TestIngestResyncEndsJourneys(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	mv := func(j string) map[string]any {
		return map[string]any{"j": j, "k": "v", "f": "player", "name": "car", "x": 1, "y": 1, "h": 0.5}
	}
	f.ingest(t, line(t, env{V: 1, Tick: 60, Src: "s", Map: "m", RecvMs: int64(base), Msgs: []any{
		map[string]any{"type": "move", "s": "nauvis", "m": []any{mv("1.1.v1"), mv("1.1.v2")}},
	}}))
	// A resync lists the surfaces, re-sends only v1 and ends: v2 gets a gone.
	resp := f.ingest(t, line(t, env{V: 1, Tick: 120, Src: "s", Map: "m", RecvMs: int64(base + 1000), Msgs: []any{
		map[string]any{"type": "surfaces", "list": []any{map[string]any{"name": "nauvis", "tile": true}}},
		map[string]any{"type": "move", "s": "nauvis", "m": []any{mv("1.1.v1")}},
		map[string]any{"type": "resynced", "trace": "t", "counts": map[string]int{"movers": 1}},
	}}))
	if resp.Samples.Accepted != 2 {
		t.Fatalf("resp %+v", resp)
	}
	live, err := f.mov.Live(ctx, core.SurfaceKey{Map: "m", Surface: "nauvis"}, base+2000)
	if err != nil {
		t.Fatal(err)
	}
	gone := false
	for _, r := range live.Rows {
		if live.Movers[r.Mover].ID == "1.1.v2" && r.Flags&core.FlagGone != 0 {
			gone = true
		}
	}
	if !gone {
		t.Fatalf("v2 not ended: %+v", live.Rows)
	}
}

// TestIngestAdoptKeepsSourceTail: a Universal Edges destination's adopt reaches ingest
// before the source's last sample and gone, which were batched later. Both are from
// before the adopt by their sample time and are kept.
func TestIngestAdoptKeepsSourceTail(t *testing.T) {
	f := newFixture(t, Options{})
	mv := func(x float64) map[string]any {
		return map[string]any{"j": "1.1.7", "k": "t", "f": "player", "name": "locomotive", "x": x, "y": 1, "h": 0.25, "n": 2}
	}
	move := func(m map[string]any) map[string]any {
		return map[string]any{"type": "move", "s": "nauvis", "m": []any{m}}
	}
	f.ingest(t, line(t, env{V: 1, Tick: 60, Src: "a", Map: "gw-w", RecvMs: int64(base), Msgs: []any{move(mv(1))}}))
	f.ingest(t, line(t, env{V: 1, Tick: 6000, Src: "b", Map: "gw-w", RecvMs: int64(base + 2000), Msgs: []any{
		map[string]any{"type": "adopt", "j": "1.1.7", "trace": "x"}, move(mv(40)),
	}}))
	resp := f.ingest(t, line(t, env{V: 1, Tick: 150, Src: "a", Map: "gw-w", RecvMs: int64(base + 1500), Msgs: []any{
		move(mv(20)), move(map[string]any{"j": "1.1.7", "gone": true}),
	}}))
	st := f.in.Stats()
	if resp.Samples.Accepted != 2 || st.LateKept != 2 || st.OwnerDrops != 0 {
		t.Fatalf("resp %+v, stats %+v", resp.Samples, st)
	}
	// Later samples of the source are not the journey's any more.
	resp = f.ingest(t, line(t, env{V: 1, Tick: 210, Src: "a", Map: "gw-w", RecvMs: int64(base + 2500), Msgs: []any{move(mv(30))}}))
	if resp.Samples.Accepted != 0 || f.in.Stats().OwnerDrops != 1 {
		t.Fatalf("resp %+v, stats %+v", resp.Samples, f.in.Stats())
	}
}

// TestIngestResyncLoggedOnce: the Clusterio instance sends a resync's "resynced" to each
// of its maps; the log has it once.
func TestIngestResyncLoggedOnce(t *testing.T) {
	var logged []string
	f := newFixture(t, Options{Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }})
	done := func(mapKey, trace string, recv int64) string {
		return line(t, env{V: 1, Tick: 60, Src: "s", Map: mapKey, RecvMs: recv, Msgs: []any{
			map[string]any{"type": "resynced", "trace": trace, "counts": map[string]int{"movers": 1}},
		}})
	}
	f.ingest(t, done("gw-w", "r1", int64(base))+done("i-1", "r1", int64(base)))
	f.ingest(t, done("gw-w", "r2", int64(base+1000))+done("i-1", "r2", int64(base+1000)))
	n := 0
	for _, l := range logged {
		if strings.Contains(l, " resynced ") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("resyncs logged %d times, want 2: %q", n, logged)
	}
}

func TestReadLineLimits(t *testing.T) {
	f := newFixture(t, Options{MaxLine: 100})
	body := strings.Repeat("x", 70000) + "\n" + line(t, env{V: 1, Tick: 1, Src: "s", Map: "m", RecvMs: int64(base), Msgs: []any{}})
	resp := f.ingest(t, body)
	if resp.BadLines != 1 || resp.Envelopes != 1 {
		t.Fatalf("resp %+v", resp)
	}
}

func TestDataClock(t *testing.T) {
	dc := &DataClock{}
	dc.Observe(5)
	dc.Observe(3)
	if dc.Now() != 5 {
		t.Fatalf("clock %d", dc.Now())
	}
	dc = &DataClock{} // empty database: the first envelope sets the clock
	maps, _ := OpenMaps(t.TempDir())
	ter := memstore.NewTerrain()
	in := New(Stores{Terrain: ter, Movers: memstore.NewMovers(), Overlays: memstore.NewOverlays(), Maps: maps}, Options{Clock: dc.Now, DataClock: dc})
	// Recorded data far behind the wall clock: the data clock follows it, so nothing is
	// clamped and the commit horizon can trail the data.
	old := base - 30*core.Day
	resp, err := in.Ingest(context.Background(), strings.NewReader(line(t, env{V: 1, Tick: 600, Src: "s", Map: "m", RecvMs: int64(old), Msgs: []any{
		map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 0, "y": 0},
	}})))
	if err != nil || resp.Chunks.Accepted != 1 || dc.Now() != old || in.Stats().Clamped != 0 {
		t.Fatalf("resp %+v err %v clock %d", resp, err, dc.Now())
	}
}

// TestDataClockBounded: one envelope with a far-future recv_ms moves the data clock by
// at most MaxDataJump, and never past the wall clock plus MaxAhead.
func TestDataClockBounded(t *testing.T) {
	dc := &DataClock{}
	if dc.Advance(1000, 5000, 10, 100) || dc.Seen() != 1000 || dc.Now() != 0 {
		t.Fatalf("first advance: seen %d now %d", dc.Seen(), dc.Now())
	}
	if !dc.Advance(9000, 1_000_000, 10, 100) || dc.Seen() != 1100 {
		t.Fatalf("jump not bounded: %d", dc.Seen())
	}
	if !dc.Advance(1_000_000, 1150, 10, 1000) || dc.Seen() != 1160 {
		t.Fatalf("wall clock bound: %d", dc.Seen())
	}
	if dc.Advance(500, 1150, 10, 1000) || dc.Seen() != 1160 {
		t.Fatalf("went backwards: %d", dc.Seen())
	}
	dc.Publish(dc.Seen())
	if dc.Now() != 1160 {
		t.Fatalf("published %d", dc.Now())
	}

	dc = &DataClock{}
	old := base - 30*core.Day
	dc.Observe(old)
	maps, _ := OpenMaps(t.TempDir())
	in := New(Stores{Terrain: memstore.NewTerrain(), Movers: memstore.NewMovers(), Overlays: memstore.NewOverlays(), Maps: maps}, Options{Clock: dc.Now, DataClock: dc})
	body := line(t, env{V: 1, Tick: 600, Src: "s", Map: "m", RecvMs: int64(base + 365*core.Day), Msgs: []any{}}) +
		line(t, env{V: 1, Tick: 660, Src: "s", Map: "m", RecvMs: int64(old + core.Second), Msgs: []any{}})
	if _, err := in.Ingest(context.Background(), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if dc.Now() != old+core.Hour || in.Stats().Clamped != 1 {
		t.Fatalf("clock %d (old+%d), clamped %d", dc.Now(), dc.Now()-old, in.Stats().Clamped)
	}
}

// clockMovers records the published data clock at each Append.
type clockMovers struct {
	*memstore.Movers
	dc  *DataClock
	bad []string
}

func (m *clockMovers) Append(ctx context.Context, s []core.MoverSample) (core.ApplyResult, error) {
	now := m.dc.Now()
	for _, x := range s {
		if x.T < now {
			m.bad = append(m.bad, fmt.Sprintf("sample at %d applied after the clock reached %d", x.T, now))
			break
		}
	}
	return m.Movers.Append(ctx, s)
}

// TestDataClockWaitsForApply: the data clock the commit loop reads (Now) must not pass
// rows that were read but not yet applied, or a commit could close their window first.
func TestDataClockWaitsForApply(t *testing.T) {
	dc := &DataClock{}
	maps, _ := OpenMaps(t.TempDir())
	mov := &clockMovers{Movers: memstore.NewMovers(), dc: dc}
	in := New(Stores{Terrain: memstore.NewTerrain(), Movers: mov, Overlays: memstore.NewOverlays(), Maps: maps},
		Options{Clock: dc.Now, DataClock: dc, FlushSamples: 4})
	old := base - core.Day
	var body strings.Builder
	for i := range 200 {
		body.WriteString(line(t, env{V: 1, Tick: int64(i * 60), Src: "s", Map: "m", RecvMs: int64(old) + int64(i)*1000, Msgs: []any{
			map[string]any{"type": "move", "s": "nauvis", "m": []any{map[string]any{"j": "1", "k": "t", "x": float64(i), "y": 0, "h": 0.25}}},
		}}))
	}
	resp, err := in.Ingest(context.Background(), strings.NewReader(body.String()))
	if err != nil || resp.Samples.Accepted != 200 {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if len(mov.bad) > 0 {
		t.Fatalf("%d batches applied behind the clock, first: %s", len(mov.bad), mov.bad[0])
	}
	if dc.Now() != old+199*core.Second {
		t.Fatalf("clock %d after the body, want %d", dc.Now(), old+199*core.Second)
	}
}

// TestIngestBytesMatchesReader: a body applied in place (IngestBytes) gives the same
// result as the same body read from a reader.
func TestIngestBytesMatchesReader(t *testing.T) {
	d := core.EncodeChart(chart(0x1234))
	h := 0.5
	body := "not json\r\n" +
		line(t, env{V: 1, Tick: 600, Src: "s", Map: "m", RecvMs: int64(base), Msgs: []any{
			map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 1, "y": 2, "d": d},
			map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 3, "y": 2},
			map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 4, "y": 2, "d": "!!"},
			map[string]any{"type": "move", "s": "nauvis", "m": []any{
				map[string]any{"j": "5.1.7", "k": "t", "f": "player", "x": 1.5, "y": 2.5, "h": h},
			}},
		}}) +
		`{"v":1,"tick":1,"src":"s","map":"m","recv_ms":1,"msgs":[]` + strings.Repeat(" ", 400) + "}\n" +
		"\n\n" +
		strings.TrimSuffix(line(t, env{V: 1, Tick: 660, Src: "s", Map: "m", RecvMs: int64(base + 1000), Msgs: []any{
			map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": 1, "y": 2, "d": d},
		}}), "\n") + "\r"
	a := newFixture(t, Options{MaxLine: 300 + len(d)*3})
	b := newFixture(t, Options{MaxLine: 300 + len(d)*3})
	ra := a.ingest(t, body)
	rb, err := b.in.IngestBytes(context.Background(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(ra)
	jb, _ := json.Marshal(rb)
	if string(ja) != string(jb) {
		t.Fatalf("reader %s\nbytes  %s", ja, jb)
	}
	if ra.Chunks.Accepted != 3 || ra.Samples.Accepted != 1 || ra.BadLines != 2 {
		t.Fatalf("resp %s", ja)
	}
	if *a.notes != *b.notes || a.in.Stats() != b.in.Stats() {
		t.Fatalf("notes %+v / %+v, stats %+v / %+v", *a.notes, *b.notes, a.in.Stats(), b.in.Stats())
	}
}
