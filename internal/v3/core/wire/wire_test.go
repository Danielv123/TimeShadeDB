package wire

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
)

func testPalette(n int) core.Palette {
	p := core.Palette{Colors: make([]uint16, n)}
	for i := 1; i < n; i++ {
		p.Colors[i] = uint16(i*37 + 1)
	}
	return p
}

func randomImage(r *rand.Rand, colors []uint16) *core.TileImage {
	im := core.NewTileImage()
	for i := range im.Pix {
		if r.Intn(3) > 0 {
			im.Pix[i] = colors[r.Intn(len(colors))]
		}
	}
	return im
}

func TestTileRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{2, 256, 300} {
		pal := testPalette(n)
		im := randomImage(r, pal.Colors)
		b, err := EncodeTile(TileHeader{Z: 0, X: -3, Y: 2, Version: 7, Live: true}, im, NewLUT(pal))
		if err != nil {
			t.Fatal(err)
		}
		h, got, err := DecodeTile(b, &pal)
		if err != nil {
			t.Fatal(err)
		}
		wantFormat := FormatPal8
		if n > 256 {
			wantFormat = FormatPal16
		}
		if h.Format != wantFormat || h.X != -3 || h.Y != 2 || h.Version != 7 || !h.Live || h.PaletteVersion > uint32(n) {
			t.Fatalf("palette %d: header %+v", n, h)
		}
		if !got.Equal(im) {
			t.Fatalf("palette %d: pixels differ", n)
		}
		short := core.Palette{Colors: pal.Colors[:h.PaletteVersion-1]}
		if _, _, err := DecodeTile(b, &short); !errors.Is(err, ErrFormat) {
			t.Fatalf("short palette accepted: %v", err)
		}
	}
	im := randomImage(r, []uint16{1, 0xF8C3, 0xFFFF, 0x1234})
	b, err := EncodeTile(TileHeader{Z: -3, X: 1, Y: -1, Version: 2}, im, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, got, err := DecodeTile(b, nil)
	if err != nil || h.Format != FormatRGB565 || h.Z != -3 || h.PaletteVersion != 0 || !got.Equal(im) {
		t.Fatalf("rgb565 round trip: %+v %v", h, err)
	}
	if _, err := EncodeTile(TileHeader{}, im, NewLUT(testPalette(2))); err == nil {
		t.Fatal("colour outside the palette accepted")
	}
	if _, _, err := DecodeTile(b[:100], nil); !errors.Is(err, ErrFormat) {
		t.Fatalf("truncated tile err = %v", err)
	}
}

func TestDeltaRoundTrip(t *testing.T) {
	pal := testPalette(40)
	var deltas []core.ChunkDelta
	for _, blk := range []uint8{0, 17, 255} {
		var d core.ChunkDelta
		d.Block = blk
		for i := range d.Pixels {
			d.Pixels[i] = pal.Colors[(i+int(blk))%len(pal.Colors)]
		}
		deltas = append(deltas, d)
	}
	b, err := EncodeTileDelta(DeltaHeader{X: 4, Y: -4, From: 3, To: 9}, deltas, NewLUT(pal))
	if err != nil {
		t.Fatal(err)
	}
	h, got, err := DecodeTileDelta(b, &pal)
	if err != nil {
		t.Fatal(err)
	}
	if h.Format != FormatPal8 || h.X != 4 || h.Y != -4 || h.From != 3 || h.To != 9 || h.Count != 3 || !reflect.DeepEqual(got, deltas) {
		t.Fatalf("delta round trip: %+v", h)
	}
	b, err = EncodeTileDelta(DeltaHeader{}, deltas, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, got, err := DecodeTileDelta(b, nil); err != nil || !reflect.DeepEqual(got, deltas) {
		t.Fatalf("rgb565 delta: %v", err)
	}
	if _, err := EncodeTileDelta(DeltaHeader{}, []core.ChunkDelta{deltas[1], deltas[0]}, nil); err == nil {
		t.Fatal("unsorted delta accepted")
	}
}

func TestPaletteRoundTrip(t *testing.T) {
	p := testPalette(235)
	got, err := DecodePalette(EncodePalette(p))
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("palette round trip: %v", err)
	}
	bad := EncodePalette(core.Palette{Colors: []uint16{5}})
	if _, err := DecodePalette(bad); !errors.Is(err, ErrFormat) {
		t.Fatalf("palette without 0 accepted: %v", err)
	}
}

func sampleWindow() *core.MoverWindow {
	return &core.MoverWindow{
		Surface: core.SurfaceKey{Map: "m", Surface: "s"},
		Kind:    core.WindowClosed,
		Start:   1_791_630_000_000,
		Len:     core.MoverWindowLen,
		Movers: []core.MoverMeta{
			{ID: "1.2.3", Kind: core.KindTrain, Force: "player", Name: "locomotive", Carriages: 2, Kinds: "LC", Color: "f20000"},
			{ID: "p:alice", Kind: core.KindPlayer, Force: "player", Name: "alice"},
		},
		Rows: []core.MoverRow{
			{Mover: 0, T: 1_791_630_000_100, Tick: 1000, X: -10.5, Y: 3.25, Orientation: 0.25, Flags: core.FlagStop,
				Carriages: []core.Carriage{{X: -10.5, Y: 3.25, Orientation: 0.25}, {X: -17.5, Y: 3.25, Orientation: 0.25}}},
			{Mover: 0, T: 1_791_630_000_400, Tick: 1018, X: -5, Y: 3.25, Orientation: 0.9999},
			{Mover: 0, T: 1_791_630_001_000, Tick: 1054, Flags: core.FlagGone | core.FlagHandoff},
			{Mover: 0, T: 1_791_630_001_500, Tick: 1084, X: 300, Y: 3, Orientation: 0.0001, Flags: core.FlagEndSwitch},
			{Mover: 1, T: 1_791_629_999_000, X: 1, Y: 2, Flags: core.FlagNoTick | core.FlagNoHeading},
		},
	}
}

func TestMoverWindowRoundTrip(t *testing.T) {
	w := sampleWindow()
	b, err := EncodeMoverWindow(w)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeMoverWindow(b, w.Surface)
	if err != nil {
		t.Fatal(err)
	}
	want := *w
	want.Rows = append([]core.MoverRow(nil), w.Rows...)
	want.Rows[1].Orientation = float64(core.QuantizeOrientation(0.9999)) / core.OrientUnits
	want.Rows[3].Orientation = float64(core.QuantizeOrientation(0.0001)) / core.OrientUnits
	if !reflect.DeepEqual(got, &want) {
		t.Fatalf("round trip:\n got  %+v\n want %+v", got, &want)
	}
	// Same bytes on re-encode.
	b2, err := EncodeMoverWindow(got)
	if err != nil || string(b2) != string(b) {
		t.Fatalf("re-encode differs (%v)", err)
	}
	for _, cut := range []int{0, 10, 40, len(b) - 1} {
		if _, err := DecodeMoverWindow(b[:cut], w.Surface); err == nil {
			t.Fatalf("truncated at %d accepted", cut)
		}
	}
	bad := sampleWindow()
	bad.Rows[0], bad.Rows[1] = bad.Rows[1], bad.Rows[0]
	if _, err := EncodeMoverWindow(bad); err == nil {
		t.Fatal("unsorted rows accepted")
	}
	empty := &core.MoverWindow{Kind: core.WindowKeyframe, Start: 5}
	b, err = EncodeMoverWindow(empty)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeMoverWindow(b, core.SurfaceKey{}); err != nil || len(got.Rows) != 0 || len(got.Movers) != 0 {
		t.Fatalf("empty window: %+v %v", got, err)
	}
}

// TestMoverWindowRail: rail path rows (core.FlagRail) round trip through the two extra
// columns; a window without them keeps the eight-column layout older readers know.
func TestMoverWindowRail(t *testing.T) {
	w := sampleWindow()
	plain, err := EncodeMoverWindow(w)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(plain[6:])&MoverHasRail != 0 {
		t.Fatal("a window without rail rows announces rail columns")
	}
	w.Rows[0].Flags |= core.FlagRail
	w.Rows[0].Speed = 0.25
	w.Rows[0].RailStart = true
	w.Rows[0].Rail = []core.RailPiece{{X: -1067, Y: -764, Dir: 0, Kind: core.RailKindCurveA}, {X: -1069, Y: -769, Dir: 15, Kind: core.RailKindCurveB}}
	w.Rows[1].Flags |= core.FlagRail
	w.Rows[1].Speed = 1.25
	b, err := EncodeMoverWindow(w)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(b[6:])&MoverHasRail == 0 {
		t.Fatal("rail columns not announced")
	}
	got, err := DecodeMoverWindow(b, w.Surface)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		g, x := got.Rows[i], w.Rows[i]
		if g.Flags != x.Flags || g.Speed != x.Speed || g.RailStart != x.RailStart || !reflect.DeepEqual(g.Rail, x.Rail) {
			t.Fatalf("row %d:\n got  %+v\n want %+v", i, g, x)
		}
	}
	b2, err := EncodeMoverWindow(got)
	if err != nil || string(b2) != string(b) {
		t.Fatalf("re-encode differs (%v)", err)
	}
	// a rail flag on a gone row is dropped, and a rail row without the columns is refused
	for i := range b {
		if i == 6 {
			b[i] &^= byte(MoverHasRail)
		}
	}
	if _, err := DecodeMoverWindow(b, w.Surface); err == nil {
		t.Fatal("rail rows without rail columns accepted")
	}
}

func TestMoverWindowSizeOnTrainTrack(t *testing.T) {
	// A train sampled every ~300 ms at 40 tiles/s: compressed like a stored window, the
	// columnar body must stay under the 6 B/sample target (PLAN.md §5).
	w := &core.MoverWindow{Kind: core.WindowClosed, Start: 0, Len: core.MoverWindowLen,
		Movers: []core.MoverMeta{{ID: "t", Kind: core.KindTrain, Force: "player", Name: "locomotive"}}}
	r := rand.New(rand.NewSource(3))
	x, tick := 0.0, int64(0)
	for i := 0; i < 1000; i++ {
		dt := 250 + r.Intn(100)
		tick += int64(dt * 60 / 1000)
		x += 40 * float64(dt) / 1000
		w.Rows = append(w.Rows, core.MoverRow{T: core.TimeMs(i * 300), Tick: tick, X: x, Y: 10, Orientation: 0.25})
	}
	b, err := EncodeMoverWindow(w)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		t.Fatal(err)
	}
	z := enc.EncodeAll(b, nil)
	t.Logf("%.2f B/sample raw, %.2f B/sample zstd", float64(len(b))/1000, float64(len(z))/1000)
	if per := float64(len(z)) / 1000; per > 6 {
		t.Fatalf("%.2f B/sample after zstd", per)
	}
}
