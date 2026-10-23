package ingest

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"math/rand/v2"
	"testing"

	"timeshadedb/internal/core"
)

func TestDecodeChartMatchesCore(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range 200 {
		var px core.ChunkPixels
		for j := range px {
			if rng.IntN(4) == 0 || i%7 == 0 {
				px[j] = uint16(rng.Uint32())
			} else if j > 0 {
				px[j] = px[j-1]
			}
		}
		d := core.EncodeChart(&px)
		var got core.ChunkPixels
		if err := decodeChart(d, &got); err != nil || got != px {
			t.Fatalf("chart %d: %v", i, err)
		}
	}
	short := func(n int) string {
		var b bytes.Buffer
		zw := zlib.NewWriter(&b)
		zw.Write(make([]byte, n))
		zw.Close()
		return base64.StdEncoding.EncodeToString(b.Bytes())
	}
	good := core.EncodeChart(&core.ChunkPixels{})
	raw, _ := base64.StdEncoding.DecodeString(good)
	raw[len(raw)-1] ^= 0xff // break the adler32 checksum
	for name, d := range map[string]string{
		"not base64": "!!!", "not zlib": base64.StdEncoding.EncodeToString([]byte("hello world")),
		"short": short(2047), "long": short(2049), "empty": short(0), "checksum": base64.StdEncoding.EncodeToString(raw),
	} {
		var px core.ChunkPixels
		if err := decodeChart(d, &px); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := core.DecodeChart(d); err == nil {
			t.Errorf("%s: core accepted", name)
		}
	}
	// The pooled decoder still works after errors.
	var px core.ChunkPixels
	if err := decodeChart(good, &px); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkDecodeChart(b *testing.B) {
	var px core.ChunkPixels
	for i := range px {
		px[i] = uint16(i / 37)
	}
	d := core.EncodeChart(&px)
	b.RunParallel(func(pb *testing.PB) {
		var out core.ChunkPixels
		for pb.Next() {
			if err := decodeChart(d, &out); err != nil {
				b.Fatal(err)
			}
		}
	})
}
