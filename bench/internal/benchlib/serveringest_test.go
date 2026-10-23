package benchlib

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	"timeshadedb/internal/core"
	"timeshadedb/internal/ingest"
	"timeshadedb/internal/movers"
	"timeshadedb/internal/overlays"
	"timeshadedb/internal/terrain"
)

// serverIngest is the ingest side of serve (stores sized from a 256 MiB budget as
// cmd/timeshadedb does, data clock, commit loop every second) without HTTP, for
// profiling the server's share of the synthetic ingest benchmark.
type serverIngest struct {
	ter  *terrain.Store
	mov  *movers.Store
	ovl  *overlays.Store
	in   *ingest.Ingester
	dc   *ingest.DataClock
	stop chan struct{}
	done chan struct{}
	kick chan struct{}
}

// ingest posts one body and, like the server's commit loop, asks for a commit when the
// mover open rows are a quarter full.
func (s *serverIngest) ingest(body []byte) (core.IngestResponse, error) {
	resp, err := s.in.Ingest(context.Background(), bytes.NewReader(body))
	if s.mov.Pressure() >= 0.25 {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	return resp, err
}

func newServerIngest(tb testing.TB) *serverIngest {
	tb.Helper()
	db := tb.TempDir()
	const budget = 256 << 20
	dc := &ingest.DataClock{}
	topt := terrain.SplitBudget(budget / 2)
	topt.Workers = min(runtime.GOMAXPROCS(0), 8)
	ter, err := terrain.Open(db, topt)
	if err != nil {
		tb.Fatal(err)
	}
	mov, err := movers.Open(db, movers.Options{SyncInterval: time.Second, OpenBytes: budget / 8, Clock: dc.Seen})
	if err != nil {
		tb.Fatal(err)
	}
	ovl, err := overlays.Open(db, overlays.Options{CacheBytes: budget / 32})
	if err != nil {
		tb.Fatal(err)
	}
	maps, err := ingest.OpenMaps(db)
	if err != nil {
		tb.Fatal(err)
	}
	s := &serverIngest{ter: ter, mov: mov, ovl: ovl, dc: dc, stop: make(chan struct{}), done: make(chan struct{}), kick: make(chan struct{}, 1)}
	s.in = ingest.New(ingest.Stores{Terrain: ter, Movers: mov, Overlays: ovl, Maps: maps}, ingest.Options{Clock: dc.Now, DataClock: dc})
	go func() {
		defer close(s.done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-s.kick:
			case <-t.C:
			}
			now := dc.Now()
			_ = ter.Commit(context.Background(), now-2*core.Minute)
			_ = mov.Commit(context.Background(), now-2*core.Minute)
		}
	}()
	tb.Cleanup(func() {
		close(s.stop)
		<-s.done
		ovl.Close()
		mov.Close()
		ter.Close()
	})
	return s
}

// BenchmarkServerIngestSynth posts the synthetic chunk workload's bodies (4,096 updates
// each, as ingestbench sends them) straight to the ingester.
func BenchmarkServerIngestSynth(b *testing.B) {
	const bodiesN = 240
	synth := NewSynth(DefaultSynth())
	enc := &V3{Src: "standalone:bench"}
	var bodies []Body
	rows := 0
	for len(bodies) < bodiesN {
		bs, err := enc.EncodeChunks(synth.NextChunks(4096))
		if err != nil {
			b.Fatal(err)
		}
		for _, x := range bs {
			rows += x.Rows
		}
		bodies = append(bodies, bs...)
	}
	s := newServerIngest(b)
	b.ResetTimer()
	n := 0
	for i := 0; i < b.N; i++ {
		for _, body := range bodies {
			resp, err := s.ingest(body.Data)
			if err != nil || resp.Chunks.Accepted != body.Rows {
				b.Fatalf("%v %+v", err, resp.Chunks)
			}
			n += body.Rows
		}
		// Later rounds replay the same bodies: shift the generator instead.
		if i+1 < b.N {
			b.StopTimer()
			bodies = bodies[:0]
			for len(bodies) < bodiesN {
				bs, _ := enc.EncodeChunks(synth.NextChunks(4096))
				bodies = append(bodies, bs...)
			}
			b.StartTimer()
		}
	}
	b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "updates/s")
}

// BenchmarkServerIngestMovers posts the synthetic mover workload (16,384 samples per
// body).
func BenchmarkServerIngestMovers(b *testing.B) {
	const bodiesN = 240
	cfg := DefaultSynth()
	cfg.Chunks = 1
	synth := NewSynth(cfg)
	enc := &V3{Src: "standalone:bench"}
	gen := func() []Body {
		var bodies []Body
		for len(bodies) < bodiesN {
			bs, err := enc.EncodeMovers(synth.NextMovers(16384))
			if err != nil {
				b.Fatal(err)
			}
			bodies = append(bodies, bs...)
		}
		return bodies
	}
	bodies := gen()
	s := newServerIngest(b)
	b.ResetTimer()
	n := 0
	for i := 0; i < b.N; i++ {
		for _, body := range bodies {
			resp, err := s.ingest(body.Data)
			if err != nil || resp.Samples.Accepted != body.Rows {
				b.Fatalf("%v %+v", err, resp.Samples)
			}
			n += body.Rows
		}
		if i+1 < b.N {
			b.StopTimer()
			bodies = gen()
			b.StartTimer()
		}
	}
	b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "samples/s")
}

func BenchmarkClientSynthChunks(b *testing.B) {
	synth := NewSynth(DefaultSynth())
	enc := &V3{Src: "standalone:bench"}
	synth.NextChunks(8000)
	var gen, encd time.Duration
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		rows := synth.NextChunks(4096)
		t1 := time.Now()
		if _, err := enc.EncodeChunks(rows); err != nil {
			b.Fatal(err)
		}
		gen += t1.Sub(t0)
		encd += time.Since(t1)
	}
	b.ReportMetric(float64(gen.Milliseconds())/float64(b.N), "gen-ms/body")
	b.ReportMetric(float64(encd.Milliseconds())/float64(b.N), "enc-ms/body")
}
