package ingest

import (
	"bytes"
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
)

// Stores that count what they are given and keep nothing, so a memory test measures
// ingest alone.
type dropTerrain struct {
	*memstore.Terrain
	n atomic.Int64
}

func (d *dropTerrain) Apply(_ context.Context, u []core.ChunkUpdate) (core.ApplyResult, error) {
	d.n.Add(int64(len(u)))
	return core.ApplyResult{Accepted: len(u)}, nil
}

type dropMovers struct {
	*memstore.Movers
	n atomic.Int64
}

func (d *dropMovers) Append(_ context.Context, s []core.MoverSample) (core.ApplyResult, error) {
	d.n.Add(int64(len(s)))
	return core.ApplyResult{Accepted: len(s)}, nil
}

type dropOverlays struct {
	*memstore.Overlays
	n atomic.Int64
}

func (d *dropOverlays) Apply(_ context.Context, e []core.OverlayEvent) (core.ApplyResult, error) {
	d.n.Add(int64(len(e)))
	return core.ApplyResult{Accepted: len(e)}, nil
}

// peakHeap runs fn and returns how far HeapInuse rose above its level before fn.
func peakHeap(fn func()) int64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	before := int64(ms.HeapInuse)
	var peak atomic.Int64
	peak.Store(before)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			runtime.ReadMemStats(&m)
			if v := int64(m.HeapInuse); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-tk.C:
			}
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	runtime.ReadMemStats(&ms)
	if v := int64(ms.HeapInuse); v > peak.Load() {
		peak.Store(v)
	}
	return peak.Load() - before
}

// fillLine returns an envelope line of about size bytes whose msgs repeat msg.
func fillLine(size int, msg string) string {
	var b strings.Builder
	b.WriteString(`{"v":1,"tick":60,"src":"s","map":"m","recv_ms":1792000000000,"msgs":[`)
	for i := 0; b.Len() < size-len(msg)-4; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(msg)
	}
	b.WriteString("]}\n")
	return b.String()
}

// TestIngestMemoryBounded posts bodies of many tiny messages, each far under the body and
// line caps, and checks that ingest memory stays bounded by its batch limits rather than
// growing with the number of messages in a line. (Before the decode window, the first
// case peaked at over 2 GB.)
func TestIngestMemoryBounded(t *testing.T) {
	const lineSize = 4<<20 - 64
	two := func(msg string) func() string {
		return func() string { return strings.Repeat(fillLine(lineSize, msg), 2) }
	}
	head := `{"v":1,"tick":60,"src":"s","map":"m","recv_ms":1792000000000,"msgs":[`
	cases := []struct {
		name string
		body func() string
		want func(st Stats, chunks, samples, events int64) bool
	}{
		{"empty messages", two(`{}`), nil},
		{"numbers", two(`1`), func(st Stats, _, _, _ int64) bool { return st.BadMessages > 1_000_000 }},
		{"deleted chunks", two(`{"type":"chunk","s":"nauvis","f":"player","x":1,"y":2}`), func(_ Stats, c, _, _ int64) bool { return c > 100_000 }},
		{"move entries", two(`{"type":"move","s":"nauvis","m":[{"j":"a","k":"v","f":"player","name":"car","x":1,"y":1}]}`), func(_ Stats, _, s, _ int64) bool { return s > 50_000 }},
		{"one huge move", func() string {
			return head + `{"type":"move","s":"nauvis","m":[` + strings.Repeat(`{"j":"a","k":"v"},`, 200000) + `{"j":"a","k":"v"}]}]}` + "\n"
		}, func(st Stats, _, s, _ int64) bool { return st.BadMessages == 1 && s == 0 }},
		{"many entries", two(`{"type":"move","s":"nauvis","m":[` + strings.Repeat(`{"j":"a"},`, 5000) + `{"j":"a"}]}`), func(st Stats, _, s, _ int64) bool { return st.BadMessages > 100 && s == 0 }},
		{"large charts", two(`{"type":"chunk","s":"nauvis","f":"player","x":1,"y":2,"d":"` + strings.Repeat("A", 60000) + `"}`), nil},
		{"tags", two(`{"type":"tag","s":"nauvis","f":"player","n":1}`), func(_ Stats, _, _, e int64) bool { return e > 100_000 }},
		{"small lines", func() string { return strings.Repeat(head+`{},{},{},{},{},{},{},{},{},{}]}`+"\n", 200000) }, nil},
	}
	// Measure close to the live heap rather than the garbage the pacer allows.
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			maps, _ := OpenMaps(t.TempDir())
			ter := &dropTerrain{Terrain: memstore.NewTerrain()}
			mov := &dropMovers{Movers: memstore.NewMovers()}
			ovl := &dropOverlays{Overlays: memstore.NewOverlays()}
			in := New(Stores{Terrain: ter, Movers: mov, Overlays: ovl, Maps: maps}, Options{Clock: func() core.TimeMs { return base + core.Second }})
			body := []byte(c.body())
			var resp core.IngestResponse
			var err error
			grew := peakHeap(func() {
				resp, err = in.Ingest(context.Background(), bytes.NewReader(body))
			})
			if err != nil {
				t.Fatal(err)
			}
			st := in.Stats()
			t.Logf("%d bytes: peak heap +%.1f MB; envelopes %d, bad lines %d, bad messages %d, chunks %d, samples %d, overlays %d",
				len(body), float64(grew)/(1<<20), resp.Envelopes, resp.BadLines, st.BadMessages, ter.n.Load(), mov.n.Load(), ovl.n.Load())
			if resp.BadLines != 0 || resp.Envelopes == 0 {
				t.Fatalf("resp %+v", resp)
			}
			if c.want != nil && !c.want(st, ter.n.Load(), mov.n.Load(), ovl.n.Load()) {
				t.Fatalf("unexpected counts: stats %+v", st)
			}
			if grew > 32<<20 {
				t.Fatalf("peak heap grew %.1f MB for a %d byte body", float64(grew)/(1<<20), len(body))
			}
		})
	}
}
