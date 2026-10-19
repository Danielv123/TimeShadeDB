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

// train builds rows for one journey with meta 0.
func trainRows(specs ...[6]float64) []srcRow {
	out := make([]srcRow, len(specs))
	for i, s := range specs {
		out[i] = srcRow{t: int64(s[0]), x: s[1], y: s[2], vx: s[3], vy: s[4], flags: uint8(s[5])}
	}
	return out
}

var trainMeta = []core.MoverMeta{{ID: "1.2.3", Kind: core.KindTrain, Carriages: 5}}

func newRep() *MoverReport {
	return &MoverReport{Retimed: map[string]int{}, Source: newCounts(), Imported: newCounts()}
}

func TestFixJourneyAnchoredRun(t *testing.T) {
	b := float64(rowBoundary)
	// Moving at 10 tiles/s; two samples really taken at 1500 and 1800 were stamped 3001.
	rows := trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{3001, 5, 0, 10, 0, b},
		[6]float64{3001, 8, 0, 10, 0, b},
		[6]float64{3300, 23, 0, 10, 0, 0},
	)
	rep := newRep()
	before := trainJumps(rows)
	got := fixJourney(rows, trainMeta, rep)
	if len(got) != 4 || got[1].t != 1500 || got[2].t != 1800 {
		t.Fatalf("re-timed rows: %+v", got)
	}
	if rep.Retimed["t"] != 2 || rep.BoundaryStamped != 2 || rep.DupSamplesDropped != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if before != 1 || trainJumps(got) != 0 {
		t.Fatalf("jumps before %d after %d, want 1 and 0", before, trainJumps(got))
	}
}

func TestFixJourneyForwardAndLimits(t *testing.T) {
	b := float64(rowBoundary)
	// No sample after the run: forward estimate from the previous sample.
	rows := trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{2001, 4, 0, 10, 0, b},
	)
	got := fixJourney(rows, trainMeta, newRep())
	if got[1].t != 1400 {
		t.Fatalf("forward estimate t = %d, want 1400", got[1].t)
	}
	// An estimate more than 5 s before the stamp is not trusted.
	rows = trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{60001, 10, 0, 10, 0, b},
	)
	rep := newRep()
	if got := fixJourney(rows, trainMeta, rep); got[1].t != 60001 || rep.Retimed["t"] != 0 {
		t.Fatalf("far estimate moved the sample: %+v", got)
	}
	// A stopped train is never re-timed; equal times keep the last row.
	rows = trainRows(
		[6]float64{1000, 0, 0, 0, 0, 0},
		[6]float64{3001, 0, 0, 0, 0, b},
		[6]float64{3001, 0.5, 0, 0, 0, b},
	)
	rep = newRep()
	got = fixJourney(rows, trainMeta, rep)
	if len(got) != 2 || got[1].x != 0.5 || rep.DupSamplesDropped != 1 {
		t.Fatalf("stopped/dup rows: %+v rep %+v", got, rep)
	}
}

func TestFixJourneyAfterBoundaryGone(t *testing.T) {
	b := float64(rowBoundary)
	g := float64(rowGone | rowBoundary)
	// A handoff at a window floor: the gone and the next sample were both clamped to
	// 3001; the sample's true time (from the next one) is 2700, the gone moves before it.
	rows := trainRows(
		[6]float64{2000, 0, 0, 10, 0, 0},
		[6]float64{3001, 0, 0, 0, 0, g},
		[6]float64{3001, 10, 0, 10, 0, b},
		[6]float64{3300, 16, 0, 10, 0, 0},
	)
	rep := newRep()
	got := fixJourney(rows, trainMeta, rep)
	if got[1].flags&rowGone == 0 || got[1].t != 2699 || got[2].t != 2700 {
		t.Fatalf("rows: %+v", got)
	}
	if rep.RetimedGones != 1 || rep.Retimed["t"] != 1 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestEndSwitch(t *testing.T) {
	meta := []core.MoverMeta{{ID: "1.2.3", Kind: core.KindTrain, Carriages: 5}}
	rows := trainRows(
		[6]float64{1000, 0, 0, 0, 0, 0},
		[6]float64{3000, 28.5, 0, 0, 0, 0}, // a 5-carriage train is 28 tiles long
		[6]float64{4000, 28.5, 0, 1, 0, 0},
		[6]float64{5000, 80, 0, 1, 0, 0}, // a real jump, not an end switch
	)
	rep := newRep()
	got := fixJourney(rows, meta, rep)
	if rep.EndSwitches != 1 || got[1].flags&rowEndSwitch == 0 || got[3].flags&rowEndSwitch != 0 {
		t.Fatalf("end switches: %d, rows %+v", rep.EndSwitches, got)
	}
	if n := trainJumps(got); n != 1 {
		t.Fatalf("jumps after end-switch flagging = %d, want 1", n)
	}
}

func TestDupPairsResetAtGone(t *testing.T) {
	g := float64(rowGone)
	rows := trainRows(
		[6]float64{1, 0, 0, 0, 0, 0},
		[6]float64{1, 0, 0, 0, 0, 0},
		[6]float64{2, 0, 0, 0, 0, g},
		[6]float64{2, 0, 0, 0, 0, 0},
		[6]float64{2, 0, 0, 0, 0, 0},
	)
	if n := dupPairs(rows); n != 2 {
		t.Fatalf("dupPairs = %d, want 2", n)
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
