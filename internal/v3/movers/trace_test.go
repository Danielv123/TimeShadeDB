package movers

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"timeshadedb/internal/v3/core"
)

// traceRow is one line of a research trace (testdata/README.md).
type traceRow struct {
	t       core.TimeMs
	gone    bool
	x, y    float64
	vx, vy  float64
	heading float64
	hasHead bool
}

func readTrace(t *testing.T, path string) []traceRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		r = gz
	}
	var out []traceRow
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		ms, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || len(fields) < 3 {
			t.Fatalf("bad trace line %q", line)
		}
		row := traceRow{t: core.TimeMs(ms)}
		if fields[2] == "GONE" {
			row.gone = true
		} else {
			parts := strings.Split(strings.Trim(fields[2], "()"), ", ")
			if len(parts) != 5 {
				t.Fatalf("bad trace line %q", line)
			}
			v := make([]float64, 4)
			for i := range v {
				if v[i], err = strconv.ParseFloat(parts[i], 64); err != nil {
					t.Fatalf("bad trace line %q", line)
				}
			}
			row.x, row.y, row.vx, row.vy = v[0], v[1], v[2], v[3]
			if parts[4] != "None" {
				row.heading, _ = strconv.ParseFloat(parts[4], 64)
				row.hasHead = true
			}
		}
		out = append(out, row)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	// The traces hold a few rows with equal times (illisis re-stamped samples at window
	// boundaries); a store keeps the last one, so the expectation does too.
	dedup := out[:0]
	for i, r := range out {
		if i+1 < len(out) && out[i+1].t == r.t {
			continue
		}
		dedup = append(dedup, r)
	}
	return dedup
}

var traceSfc = core.SurfaceKey{Map: "gw-bc41437d.1", Surface: "nauvis"}

func traceMeta(name string) core.MoverMeta {
	if name == "train" {
		return core.MoverMeta{ID: "248495586.660604.17", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 4, Kinds: "LCCl"}
	}
	return core.MoverMeta{ID: "248495586.1000.v77", Kind: core.KindVehicle, Force: "player", Name: "spidertron", Kinds: "S"}
}

// traceSamples converts a trace to samples as an importer would: no ticks, heading
// when the trace has one.
func traceSamples(rows []traceRow, meta core.MoverMeta) []core.MoverSample {
	out := make([]core.MoverSample, 0, len(rows))
	for _, r := range rows {
		s := core.MoverSample{Surface: traceSfc, Meta: meta, T: r.t, Flags: core.FlagNoTick}
		if r.gone {
			s.Flags |= core.FlagGone
		} else {
			s.X, s.Y = r.x, r.y
			if r.hasHead {
				s.Orientation = r.heading
			} else {
				s.Flags |= core.FlagNoHeading
			}
			if r.vx == 0 && r.vy == 0 {
				s.Flags |= core.FlagStop
			}
		}
		out = append(out, s)
	}
	return out
}

// expectedHandoffs counts gones followed by a sample of the journey within
// HandoffWindow, and ends (the rest).
func expectedHandoffs(rows []traceRow) (handoffs, ends int) {
	for i, r := range rows {
		if !r.gone {
			continue
		}
		h := false
		for j := i + 1; j < len(rows) && rows[j].t <= r.t+core.HandoffWindow; j++ {
			if !rows[j].gone && rows[j].t > r.t {
				h = true
				break
			}
		}
		if h {
			handoffs++
		} else {
			ends++
		}
	}
	return
}

// readAllRows reads every closed window of sfc back through the day indexes.
func readAllRows(t *testing.T, m core.MoverStore, sfc core.SurfaceKey) ([]core.MoverRow, [][]core.MoverMeta) {
	t.Helper()
	ctx := context.Background()
	sfcs, err := m.Surfaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rows []core.MoverRow
	var tables [][]core.MoverMeta
	for _, info := range sfcs {
		if info.Key != sfc {
			continue
		}
		for _, d := range info.Days {
			day, err := m.Day(ctx, sfc, d)
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range day.Windows {
				w, err := m.Window(ctx, sfc, ref.Start)
				if err != nil {
					t.Fatal(err)
				}
				rows = append(rows, w.Rows...)
				tables = append(tables, w.Movers)
			}
		}
	}
	return rows, tables
}

type traceCheck struct {
	rows, gones, handoffs, maxErrUnits int
}

func checkTrace(t *testing.T, m core.MoverStore, trace []traceRow) traceCheck {
	t.Helper()
	got, _ := readAllRows(t, m, traceSfc)
	byT := map[core.TimeMs]traceRow{}
	for _, r := range trace {
		byT[r.t] = r // the trace has no duplicate times within a journey
	}
	var c traceCheck
	c.rows = len(got)
	for _, r := range got {
		src, ok := byT[r.T]
		if !ok {
			t.Fatalf("stored row at %v is not in the trace", r.T)
		}
		if src.gone != (r.Flags&core.FlagGone != 0) {
			t.Fatalf("row at %v: gone %v, trace %v", r.T, r.Flags&core.FlagGone != 0, src.gone)
		}
		if src.gone {
			c.gones++
			if r.Flags&core.FlagHandoff != 0 {
				c.handoffs++
			}
			continue
		}
		if r.Flags&core.FlagHandoff != 0 {
			t.Fatalf("handoff flag on a sample at %v", r.T)
		}
		e := int(math.Max(math.Abs(r.X-src.x), math.Abs(r.Y-src.y)) * 32)
		c.maxErrUnits = max(c.maxErrUnits, e) // in 1/32 tile
	}
	return c
}

func traceFiles(t *testing.T) map[string]string {
	return map[string]string{
		"train":        filepath.Join("testdata", "trace_train_660604.txt.gz"),
		"constructron": filepath.Join("testdata", "trace_constructron.txt.gz"),
	}
}

// TestTraceHandoffsBulk stores whole traces in one commit (an import) and checks that
// every gone followed by the same journey within 10 s is a handoff, every other gone an
// end, and every sample is kept within the 1/16-tile quantization.
func TestTraceHandoffsBulk(t *testing.T) {
	ctx := context.Background()
	for name, path := range traceFiles(t) {
		t.Run(name, func(t *testing.T) {
			trace := readTrace(t, path)
			wantH, wantE := expectedHandoffs(trace)
			m, _ := openTemp(t, Options{SyncInterval: -1, NoWAL: true, MaxBehind: -1})
			defer m.Close()
			res, err := m.Append(ctx, traceSamples(trace, traceMeta(name)))
			if err != nil || res.Accepted != len(trace) {
				t.Fatalf("append = %+v, %v", res, err)
			}
			ok(t, m.Commit(ctx, trace[len(trace)-1].t+core.Hour))
			c := checkTrace(t, m, trace)
			t.Logf("%s: %d rows, %d gones, %d handoffs (want %d, ends %d), max error %d/32 tile", name, c.rows, c.gones, c.handoffs, wantH, wantE, c.maxErrUnits)
			if c.rows != len(trace) || c.handoffs != wantH || c.gones != wantH+wantE || c.maxErrUnits > 1 {
				t.Fatalf("got %+v", c)
			}
			if name == "train" && wantE != 1 {
				t.Fatalf("the train excerpt should hold one real end, found %d", wantE)
			}
		})
	}
}

// TestTraceHandoffsStreaming replays the traces live: one Append per second of data and
// a Commit per second with upTo = clock - grace. With a grace of at least
// HandoffWindow every handoff is marked, also across window boundaries; with no grace
// the gones at the end of a window whose journey resumes in the next one are closed
// before the resume is known (the server's commit grace must cover HandoffWindow).
func TestTraceHandoffsStreaming(t *testing.T) {
	ctx := context.Background()
	for name, path := range traceFiles(t) {
		trace := readTrace(t, path)
		wantH, _ := expectedHandoffs(trace)
		for _, grace := range []core.TimeMs{0, core.HandoffWindow + 5*core.Second} {
			m, _ := openTemp(t, Options{SyncInterval: -1})
			defer m.Close()
			samples := traceSamples(trace, traceMeta(name))
			i := 0
			for clock := core.WindowStart(trace[0].t, core.Second) + core.Second; i < len(samples); clock += core.Second {
				j := i
				for j < len(samples) && samples[j].T < clock {
					j++
				}
				if j > i {
					res, err := m.Append(ctx, samples[i:j])
					if err != nil || res.Accepted != j-i {
						t.Fatalf("append = %+v, %v", res, err)
					}
					i = j
				}
				ok(t, m.Commit(ctx, clock-grace))
				if j == i && i < len(samples) && samples[i].T > clock+core.Hour {
					clock = core.WindowStart(samples[i].T, core.Second) - core.Second // skip long gaps
				}
			}
			ok(t, m.Commit(ctx, trace[len(trace)-1].t+core.Hour))
			c := checkTrace(t, m, trace)
			t.Logf("%s grace %d ms: %d handoffs of %d", name, int64(grace), c.handoffs, wantH)
			if c.rows != len(trace) {
				t.Fatalf("rows = %d, want %d", c.rows, len(trace))
			}
			if grace >= core.HandoffWindow && c.handoffs != wantH {
				t.Fatalf("%s grace %d ms: %d handoffs, want %d", name, int64(grace), c.handoffs, wantH)
			}
			if grace == 0 && c.handoffs > wantH {
				t.Fatalf("%s: more handoffs than expected", name)
			}
		}
	}
}

// TestTraceJourneysTwoSources replays the traces as a gridworld crossing between two
// instances: the source alternates at every gone, and after each crossing the old
// source sends one stale sample (as illisis saw from instances still reporting a train
// that left). Journeys drops the stale samples by the owner rule, fills in the meta of
// the gones, and the store marks every crossing as a handoff of one journey id.
func TestTraceJourneysTwoSources(t *testing.T) {
	ctx := context.Background()
	for name, path := range traceFiles(t) {
		t.Run(name, func(t *testing.T) {
			trace := readTrace(t, path)
			wantH, wantE := expectedHandoffs(trace)
			meta := traceMeta(name)
			samples := traceSamples(trace, meta)
			js := NewJourneys(0)
			srcs := [2]string{"clusterio:1", "clusterio:2"}
			cur, stale := 0, 0
			var out []core.MoverSample
			for k, s := range samples {
				if s.Flags&core.FlagGone != 0 {
					s.Meta = core.MoverMeta{ID: meta.ID} // gones carry no meta on the wire
					out = js.Move(srcs[cur], []core.MoverSample{s}, out)
					cur = 1 - cur
					continue
				}
				if name == "train" {
					s.Meta.Kinds, s.Meta.Color = "", "" // omitted: carried forward
					if k == 0 {
						s.Meta.Kinds = meta.Kinds
					}
				}
				out = js.Move(srcs[cur], []core.MoverSample{s}, out)
				if k > 0 && samples[k-1].Flags&core.FlagGone != 0 {
					st := s
					st.T += 100
					st.X += 50
					n := len(out)
					out = js.Move(srcs[1-cur], []core.MoverSample{st}, out)
					if len(out) != n {
						t.Fatalf("stale sample from the old owner was kept")
					}
					stale++
				}
			}
			if got := js.Stats().OwnerDrops; got != int64(stale) {
				t.Fatalf("owner drops = %d, want %d", got, stale)
			}
			if len(out) != len(samples) {
				t.Fatalf("journeys emitted %d samples, want %d", len(out), len(samples))
			}
			m, _ := openTemp(t, Options{SyncInterval: -1, NoWAL: true, MaxBehind: -1})
			defer m.Close()
			if _, err := m.Append(ctx, out); err != nil {
				t.Fatal(err)
			}
			ok(t, m.Commit(ctx, trace[len(trace)-1].t+core.Hour))
			c := checkTrace(t, m, trace)
			if c.handoffs != wantH || c.gones != wantH+wantE {
				t.Fatalf("got %+v, want %d handoffs %d ends", c, wantH, wantE)
			}
			_, tables := readAllRows(t, m, traceSfc)
			for _, tab := range tables {
				if len(tab) != 1 || tab[0] != meta {
					t.Fatalf("window mover table = %+v, want only %+v", tab, meta)
				}
			}
			t.Logf("%s: %d stale samples dropped, %d handoffs, one journey id and meta in %d windows", name, stale, c.handoffs, len(tables))
		})
	}
}

// TestTraceAdopt: with Universal Edges the destination adopts the journey before the
// source's gone arrives; the source's gone is then dropped by the owner rule and the
// journey continues without a gone row.
func TestTraceAdopt(t *testing.T) {
	trace := readTrace(t, filepath.Join("testdata", "trace_constructron.txt.gz"))
	meta := traceMeta("constructron")
	samples := traceSamples(trace, meta)
	js := NewJourneys(0)
	srcs := [2]string{"clusterio:1", "clusterio:2"}
	cur := 0
	var out []core.MoverSample
	gones := 0
	for _, s := range samples {
		if s.Flags&core.FlagGone != 0 {
			if prev := js.Adopt(srcs[1-cur], meta.ID); prev != srcs[cur] {
				t.Fatalf("adopt: previous owner %q, want %q", prev, srcs[cur])
			}
			out = js.Move(srcs[cur], []core.MoverSample{s}, out)
			cur = 1 - cur
			gones++
			continue
		}
		out = js.Move(srcs[cur], []core.MoverSample{s}, out)
	}
	if len(out) != len(samples)-gones || js.Stats().OwnerDrops != int64(gones) {
		t.Fatalf("out %d of %d, owner drops %d", len(out), len(samples), js.Stats().OwnerDrops)
	}
	for _, s := range out {
		if s.Flags&core.FlagGone != 0 {
			t.Fatal("a gone survived adoption")
		}
	}
}

// TestTraceHandoffsFull runs the bulk check on the complete research traces when
// TSDB_TRACES points at research/out.
func TestTraceHandoffsFull(t *testing.T) {
	dir := os.Getenv("TSDB_TRACES")
	if dir == "" {
		t.Skip("TSDB_TRACES not set")
	}
	ctx := context.Background()
	for _, name := range []string{"train", "constructron"} {
		file := map[string]string{"train": "trace_train_660604.txt", "constructron": "trace_constructron.txt"}[name]
		trace := readTrace(t, filepath.Join(dir, file))
		wantH, wantE := expectedHandoffs(trace)
		m, _ := openTemp(t, Options{SyncInterval: -1, NoWAL: true, MaxBehind: -1})
		if _, err := m.Append(ctx, traceSamples(trace, traceMeta(name))); err != nil {
			t.Fatal(err)
		}
		ok(t, m.Commit(ctx, trace[len(trace)-1].t+core.Hour))
		c := checkTrace(t, m, trace)
		t.Logf("%s full: %d rows, %d gones, %d handoffs (want %d, ends %d)", name, c.rows, c.gones, c.handoffs, wantH, wantE)
		if c.rows != len(trace) || c.handoffs != wantH || c.gones != wantH+wantE {
			t.Fatalf("got %+v", c)
		}
		m.Close()
	}
}
