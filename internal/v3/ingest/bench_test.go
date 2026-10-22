package ingest

import (
	"bytes"
	"context"
	"math/rand/v2"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
	"timeshadedb/internal/v3/terrain"
)

// BenchmarkIngestTerrain measures POST-body ingest into the file-backed terrain store
// (WAL with group commit, the server's defaults): bodies of 4,096 chunk updates in
// envelopes of 8 messages (the capture's short default lines, under 3,500 bytes). Charts look like
// game charts: runs of a few colours with noise.
func BenchmarkIngestTerrain(b *testing.B) {
	ter, err := terrain.Open(b.TempDir(), terrain.SplitBudget(128<<20))
	if err != nil {
		b.Fatal(err)
	}
	defer ter.Close()
	maps, _ := OpenMaps(b.TempDir())
	t := base
	in := New(Stores{Terrain: ter, Movers: memstore.NewMovers(), Overlays: memstore.NewOverlays(), Maps: maps}, Options{Clock: func() core.TimeMs { return t }})
	rng := rand.New(rand.NewPCG(5, 6))
	charts := make([]string, 64)
	for i := range charts {
		var px core.ChunkPixels
		c := uint16(rng.Uint32())
		for j := range px {
			if rng.IntN(40) == 0 {
				c = uint16(rng.IntN(200)) * 300
			}
			px[j] = c
		}
		charts[i] = core.EncodeChart(&px)
	}
	const perBody = 4096
	bodies := make([][]byte, 8)
	for k := range bodies {
		var buf bytes.Buffer
		for e := 0; e < perBody/8; e++ {
			msgs := make([]any, 8)
			for m := range msgs {
				n := e*8 + m
				msgs[m] = map[string]any{"type": "chunk", "s": "nauvis", "f": "player", "x": n%300 - 150, "y": (n/300)%40 - 20 + k, "d": charts[rng.IntN(len(charts))]}
			}
			buf.WriteString(line(b, env{V: 1, Tick: int64(k*perBody + e), Src: "s", Map: "m", RecvMs: int64(base) + int64(k*perBody+e)*16, Msgs: msgs}))
		}
		bodies[k] = buf.Bytes()
	}
	b.SetBytes(int64(len(bodies[0])))
	b.ResetTimer()
	for i := range b.N {
		resp, err := in.Ingest(context.Background(), bytes.NewReader(bodies[i%len(bodies)]))
		if err != nil || resp.Chunks.Accepted+resp.Chunks.Late != perBody {
			b.Fatalf("%v %+v", err, resp.Chunks)
		}
		if i%16 == 15 {
			t += core.Minute
			ter.Commit(context.Background(), t-10*core.Second)
		}
	}
	b.ReportMetric(float64(b.N*perBody)/b.Elapsed().Seconds(), "updates/s")
}
