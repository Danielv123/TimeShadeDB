package movers

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/coretest"
)

// trainBatch makes one batch of samples: n trains moving on circles, sampled every
// 200 ms, with ticks, starting at t.
func trainBatch(n int, t core.TimeMs) []core.MoverSample {
	out := make([]core.MoverSample, n)
	for i := range out {
		a := float64(t)/20000 + float64(i)
		out[i] = core.MoverSample{
			Surface:     coretest.SFC,
			Meta:        core.MoverMeta{ID: fmt.Sprintf("1.1.%d", i), Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 4, Kinds: "LCCL"},
			T:           t + core.TimeMs(i%200),
			Tick:        int64(t) * 60 / 1000,
			X:           float64(i*50) + 300*math.Cos(a),
			Y:           300 * math.Sin(a),
			Orientation: math.Mod(a/(2*math.Pi)+0.25, 1),
		}
	}
	return out
}

func benchAppend(b *testing.B, opts Options, batch int) {
	ctx := context.Background()
	m, err := Open(b.TempDir(), opts)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	t := coretest.T0
	b.ReportAllocs()
	b.ResetTimer()
	samples := 0
	for i := 0; i < b.N; i++ {
		s := trainBatch(batch, t)
		if _, err := m.Append(ctx, s); err != nil {
			b.Fatal(err)
		}
		samples += len(s)
		t += 200
		if i%50 == 0 { // every 10 s of data; a window closes every 5 minutes
			if err := m.Commit(ctx, t-15*core.Second); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(samples)/b.Elapsed().Seconds(), "samples/s")
}

// BenchmarkAppendWAL: 500 trains per batch, WAL with 1 s group commit, commits every
// 5 minutes of data (including zstd of the closed windows).
func BenchmarkAppendWAL(b *testing.B) {
	benchAppend(b, Options{SyncInterval: time.Second}, 500)
}

// BenchmarkAppendWALFsyncEach fsyncs the WAL on every batch.
func BenchmarkAppendWALFsyncEach(b *testing.B) {
	benchAppend(b, Options{}, 500)
}

// BenchmarkAppendNoWAL measures the in-memory path alone.
func BenchmarkAppendNoWAL(b *testing.B) {
	benchAppend(b, Options{SyncInterval: -1, NoWAL: true}, 500)
}

// BenchmarkLive reads the live payload of 500 trains with 60 s of trail (2.5 rows/s
// each).
func BenchmarkLive(b *testing.B) {
	ctx := context.Background()
	m, err := Open(b.TempDir(), Options{SyncInterval: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	t := coretest.T0
	for i := 0; i < 5*60*5; i++ {
		m.Append(ctx, trainBatch(500, t))
		t += 200
	}
	m.Commit(ctx, t-30*core.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Live(ctx, coretest.SFC, t); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWindowRead reads one closed window of 500 trains (5 minutes at 5 Hz).
func BenchmarkWindowRead(b *testing.B) {
	ctx := context.Background()
	m, err := Open(b.TempDir(), Options{SyncInterval: -1, NoWAL: true})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	t := coretest.T0
	for i := 0; i < 5*60*5; i++ {
		m.Append(ctx, trainBatch(500, t))
		t += 200
	}
	m.Commit(ctx, t+core.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Window(ctx, coretest.SFC, coretest.T0); err != nil {
			b.Fatal(err)
		}
	}
}
