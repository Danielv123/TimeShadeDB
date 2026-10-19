package minimap

import (
	"bytes"
	"compress/gzip"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"timeshadedb/internal/v3/core"
)

func TestChangeRoundTrip(t *testing.T) {
	whole := make([]uint16, BlockPixels)
	for i := range whole {
		whole[i] = uint16(i * 7)
	}
	recs := []Rec{
		{T: 1000, Block: 3, Pos: []uint16{0, 5, 1023}, Col: []uint16{1, 2, 3}},
		{T: 1500, Block: 255, Col: whole},
		{T: 4_000_000, Block: 0, Pos: []uint16{7}, Col: []uint16{0xF8C3}},
	}
	b := EncodeChange(1000, recs)
	got, err := DecodeChange(1000, b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, recs) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, recs)
	}
}

func TestChangeRejectsBadInput(t *testing.T) {
	good := EncodeChange(0, []Rec{{T: 0, Block: 1, Pos: []uint16{1}, Col: []uint16{2}}})
	for name, b := range map[string][]byte{
		"magic":     append([]byte("XXC1"), good[4:]...),
		"short":     good[:3],
		"header":    good[:8],
		"payload":   good[:len(good)-1],
		"whole":     append([]byte("MMC1"), 0, 0, 0, 0, 0, 0, 0, 1, 2),
		"position":  append([]byte("MMC1"), 0, 0, 0, 0, 0, 1, 0, 0x00, 0x04, 1, 0),
		"truncated": append(append([]byte{}, good...), 1, 2, 3),
	} {
		if _, err := DecodeChange(0, b); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", name, err)
		}
	}
	if recs, err := DecodeChange(0, []byte("MMC1")); err != nil || len(recs) != 0 {
		t.Fatalf("empty change file: %v %v", recs, err)
	}
}

func TestSnapRoundTrip(t *testing.T) {
	px := make([]uint16, AreaSize*AreaSize)
	px[0], px[len(px)-1], px[513] = 1, 0xFFFF, 0x1234
	got, err := DecodeSnap(EncodeSnap(px))
	if err != nil || !reflect.DeepEqual(got, px) {
		t.Fatalf("snapshot round trip failed: %v", err)
	}
	if _, err := DecodeSnap(EncodeSnap(px)[:100]); !errors.Is(err, ErrFormat) {
		t.Fatalf("short snapshot err = %v", err)
	}
}

func TestAreaPixelMatchesCoreGrid(t *testing.T) {
	for _, c := range []struct {
		block uint8
		pos   int
	}{{0, 0}, {0, 1023}, {17, 33}, {255, 1023}, {16, 0}} {
		if got, want := areaPixel(c.block, c.pos), core.TilePixelIndex(c.block, c.pos); got != want {
			t.Fatalf("areaPixel(%d, %d) = %d, core says %d", c.block, c.pos, got, want)
		}
	}
	// illisis chunk of block b in area (ax, ay): cx = 16*ax - 8 + (b & 15).
	tile := core.TileCoord{X: -3, Y: 2}
	if got := tile.Chunk(17); got != (core.ChunkCoord{X: 16*-3 - 8 + 1, Y: 16*2 - 8 + 1}) {
		t.Fatalf("chunk of block 17 = %v", got)
	}
}

func TestUnname(t *testing.T) {
	for in, want := range map[string]string{
		"gw-bc41437d~2E1": "gw-bc41437d.1",
		"nauvis":          "nauvis",
		"platform-1":      "platform-1",
		"a~20b~7E":        "a b~",
		"~C3~A9":          "é",
	} {
		got, err := Unname(in)
		if err != nil || got != want {
			t.Errorf("Unname(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "~", "~2", "~ZZ", "x~G1"} {
		if _, err := Unname(bad); err == nil {
			t.Errorf("Unname(%q) accepted", bad)
		}
	}
}

func TestReadGzCapsSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.gz")
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(make([]byte, 1<<20))
	zw.Close()
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGz(p, 1000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	got, err := readGz(p, 1<<20)
	if err != nil || len(got) != 1<<20 {
		t.Fatalf("read %d bytes, %v", len(got), err)
	}
	if err := os.WriteFile(p, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGz(p, 1000); err == nil {
		t.Fatal("bad gzip accepted")
	}
}

func TestHeadingFromVelocity(t *testing.T) {
	for _, c := range []struct{ vx, vy, want float64 }{
		{0, -1, 0}, {1, 0, 0.25}, {0, 1, 0.5}, {-1, 0, 0.75}, {1, -1, 0.125},
	} {
		if got := headingFromVelocity(c.vx, c.vy); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("heading(%v, %v) = %v, want %v", c.vx, c.vy, got, c.want)
		}
	}
}

func TestPickSnaps(t *testing.T) {
	s := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9}
	if got := pickSnaps(s, 0); got != nil {
		t.Fatal(got)
	}
	if got := pickSnaps(s, -1); len(got) != 9 {
		t.Fatal(got)
	}
	got := pickSnaps(s, 3)
	if len(got) != 3 || got[2] != 9 || got[0] != 1 {
		t.Fatalf("pickSnaps(3) = %v", got)
	}
	if got := pickSnaps(s, 1); len(got) != 1 || got[0] != 9 {
		t.Fatalf("pickSnaps(1) = %v", got)
	}
}
