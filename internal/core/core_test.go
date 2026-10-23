package core

import (
	"errors"
	"math/rand"
	"testing"
)

func TestFloorDivAndWindows(t *testing.T) {
	cases := []struct{ a, b, want int64 }{{7, 2, 3}, {-7, 2, -4}, {-8, 2, -4}, {0, 5, 0}, {-1, 60000, -1}}
	for _, c := range cases {
		if got := FloorDiv(c.a, c.b); got != c.want {
			t.Fatalf("FloorDiv(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if got := WindowStart(119_999, Minute); got != 60_000 {
		t.Fatalf("WindowStart = %d", got)
	}
	if got := WindowEnd(120_000, Minute); got != 180_000 {
		t.Fatalf("WindowEnd = %d", got)
	}
	if got := WindowStart(-1, Minute); got != -60_000 {
		t.Fatalf("WindowStart(-1) = %d", got)
	}
	if got := UTCDay(1791630273766); got != "2026-10-10" {
		t.Fatalf("UTCDay = %q", got)
	}
	d, err := ParseUTCDay("2026-10-10")
	if err != nil || UTCDay(d) != "2026-10-10" || d%Day != 0 {
		t.Fatalf("ParseUTCDay = %d, %v", d, err)
	}
}

func TestEscapeNameRoundTrip(t *testing.T) {
	cases := map[string]string{
		"nauvis":        "nauvis",
		"gw-bc41437d.1": "gw-bc41437d~2E1",
		"Platform 1":    "~50latform~201",
		"a~b":           "a~7Eb",
		"é":             "~C3~A9",
	}
	for in, want := range cases {
		got := EscapeName(in)
		if got != want {
			t.Fatalf("EscapeName(%q) = %q, want %q", in, got, want)
		}
		back, err := UnescapeName(got)
		if err != nil || back != in {
			t.Fatalf("UnescapeName(%q) = %q, %v", got, back, err)
		}
	}
	for _, bad := range []string{"", "A", "a.b", "~2e", "~2", "~ZZ", "a b"} {
		if _, err := UnescapeName(bad); !errors.Is(err, ErrBadKey) {
			t.Fatalf("UnescapeName(%q) err = %v, want ErrBadKey", bad, err)
		}
	}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		b := make([]byte, 1+r.Intn(12))
		for j := range b {
			b[j] = byte(r.Intn(256))
		}
		s := string(b)
		back, err := UnescapeName(EscapeName(s))
		if err != nil || back != s {
			t.Fatalf("round trip of %q failed: %q, %v", s, back, err)
		}
	}
}

func TestDatastoreIDs(t *testing.T) {
	k := DatastoreKey{Map: "gw-bc41437d.1", Surface: "nauvis", Force: "player"}
	if got, want := k.ID(), "gw-bc41437d~2E1.nauvis.player"; got != want {
		t.Fatalf("ID = %q, want %q", got, want)
	}
	back, err := ParseDatastoreID(k.ID())
	if err != nil || back != k {
		t.Fatalf("ParseDatastoreID = %+v, %v", back, err)
	}
	if got, want := k.SurfaceKey().ID(), "gw-bc41437d~2E1.nauvis"; got != want {
		t.Fatalf("surface ID = %q, want %q", got, want)
	}
	sk, err := ParseSurfaceID("i-248495586.platform-1")
	if err != nil || sk != (SurfaceKey{Map: "i-248495586", Surface: "platform-1"}) {
		t.Fatalf("ParseSurfaceID = %+v, %v", sk, err)
	}
	for _, bad := range []string{"a.b", "a.b.c.d", "a..c", ".b.c", "a.B.c"} {
		if _, err := ParseDatastoreID(bad); !errors.Is(err, ErrBadKey) {
			t.Fatalf("ParseDatastoreID(%q) err = %v", bad, err)
		}
	}
	if err := (DatastoreKey{Map: "m", Surface: "s"}).Validate(); !errors.Is(err, ErrBadKey) {
		t.Fatalf("empty force accepted: %v", err)
	}
}

func TestChunkTileMapping(t *testing.T) {
	// Gridworld instance (0, 0) covers tiles [-256, 256): chunks -8..7 form z0 tile (0, 0).
	tile, block := ChunkCoord{X: -8, Y: -8}.Tile()
	if tile != (TileCoord{0, 0, 0}) || block != 0 {
		t.Fatalf("chunk -8,-8 -> %v block %d", tile, block)
	}
	tile, block = ChunkCoord{X: 7, Y: 7}.Tile()
	if tile != (TileCoord{0, 0, 0}) || block != 255 {
		t.Fatalf("chunk 7,7 -> %v block %d", tile, block)
	}
	tile, block = ChunkCoord{X: -9, Y: 8}.Tile()
	if tile != (TileCoord{0, -1, 1}) || block != 15 {
		t.Fatalf("chunk -9,8 -> %v block %d", tile, block)
	}
	// illisis: cx = 16*ax - 8 + (b & 15), cy = 16*ay - 8 + (b >> 4).
	for ax := int32(-4); ax <= 3; ax++ {
		for ay := int32(-3); ay <= 2; ay++ {
			for b := 0; b < 256; b++ {
				c := TileCoord{0, ax, ay}.Chunk(uint8(b))
				if c.X != 16*ax-8+int32(b&15) || c.Y != 16*ay-8+int32(b>>4) {
					t.Fatalf("Chunk(%d,%d,%d) = %v", ax, ay, b, c)
				}
				back, bb := c.Tile()
				if back != (TileCoord{0, ax, ay}) || int(bb) != b {
					t.Fatalf("Tile(%v) = %v %d", c, back, bb)
				}
			}
		}
	}
	if x, y := (TileCoord{0, 0, 0}).Origin(); x != -256 || y != -256 {
		t.Fatalf("origin z0 = %d,%d", x, y)
	}
	if x, y := (TileCoord{-2, 1, -1}).Origin(); x != -256+2048 || y != -256-2048 {
		t.Fatalf("origin z-2 = %d,%d", x, y)
	}
	if got := TileAt(-256, 255.9, 0); got != (TileCoord{0, 0, 0}) {
		t.Fatalf("TileAt = %v", got)
	}
	if got := TileAt(-256.1, 256, 0); got != (TileCoord{0, -1, 1}) {
		t.Fatalf("TileAt = %v", got)
	}
	if got := TileAt(793, 567, -1); got != (TileCoord{-1, 1, 0}) {
		t.Fatalf("TileAt z-1 = %v", got)
	}
}

func TestTileTree(t *testing.T) {
	tl := TileCoord{0, -3, 5}
	p := tl.Parent()
	if p != (TileCoord{-1, -2, 2}) {
		t.Fatalf("parent = %v", p)
	}
	found := false
	for _, c := range p.Children() {
		if c == tl {
			found = true
		}
		if c.Parent() != p {
			t.Fatalf("child %v parent %v", c, c.Parent())
		}
	}
	if !found {
		t.Fatal("tile not among its parent's children")
	}
	if a := tl.Ancestor(-3); a != (TileCoord{-3, -1, 0}) {
		t.Fatalf("ancestor = %v", a)
	}
	// A coarse tile covers exactly its children's world area.
	x0, y0 := p.Origin()
	cx, cy := p.Children()[0].Origin()
	if x0 != cx || y0 != cy {
		t.Fatalf("parent origin %d,%d child origin %d,%d", x0, y0, cx, cy)
	}
	if !(TileCoord{0, MaxCoord, -MaxCoord}).Valid() || (TileCoord{0, MaxCoord + 1, 0}).Valid() {
		t.Fatal("z0 bounds wrong")
	}
	if (TileCoord{1, 0, 0}).Valid() || (TileCoord{MinZ - 1, 0, 0}).Valid() || !(TileCoord{MinZ, 1, 1}).Valid() {
		t.Fatal("level bounds wrong")
	}
}

func TestTileImageBlocks(t *testing.T) {
	im := NewTileImage()
	var px ChunkPixels
	for i := range px {
		px[i] = uint16(i + 1)
	}
	im.SetBlock(17, &px) // bx 1, by 1
	if got := im.Pix[(32+0)*TileSize+32]; got != 1 {
		t.Fatalf("first pixel of block 17 = %d", got)
	}
	if got := im.Pix[(32+31)*TileSize+63]; got != 1024 {
		t.Fatalf("last pixel of block 17 = %d", got)
	}
	if im.Block(17) != px {
		t.Fatal("Block did not return what SetBlock wrote")
	}
	if TilePixelIndex(17, 33) != (33)*TileSize+33 {
		t.Fatalf("TilePixelIndex = %d", TilePixelIndex(17, 33))
	}
}

func TestAverage4MatchesIllisis(t *testing.T) {
	// illisis levels.ts: ((sum + 2) >> 2) << shift per channel.
	ref := func(a, b, c, d uint16) uint16 {
		var out uint16
		for _, s := range []struct{ shift, mask uint16 }{{11, 31}, {5, 63}, {0, 31}} {
			sum := int(a>>s.shift&s.mask) + int(b>>s.shift&s.mask) + int(c>>s.shift&s.mask) + int(d>>s.shift&s.mask)
			out |= uint16((sum+2)>>2) << s.shift
		}
		return out
	}
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 100000; i++ {
		a, b, c, d := uint16(r.Intn(65536)), uint16(r.Intn(65536)), uint16(r.Intn(65536)), uint16(r.Intn(65536))
		if Average4(a, b, c, d) != ref(a, b, c, d) {
			t.Fatalf("Average4(%x,%x,%x,%x) = %x, want %x", a, b, c, d, Average4(a, b, c, d), ref(a, b, c, d))
		}
	}
	if Average4(0xFFFF, 0xFFFF, 0xFFFF, 0xFFFF) != 0xFFFF || Average4(0, 0, 0, 0) != 0 {
		t.Fatal("extremes wrong")
	}
}

func TestDownsample(t *testing.T) {
	child := NewTileImage()
	for i := range child.Pix {
		child.Pix[i] = 0xF800 // red
	}
	out := Downsample([4]*TileImage{nil, child, nil, nil})
	if out.Pix[0] != 0 || out.Pix[256] != 0xF800 || out.Pix[255*TileSize+511] != 0xF800 || out.Pix[256*TileSize+256] != 0 {
		t.Fatal("quadrant placement wrong")
	}
}

func TestRGB565(t *testing.T) {
	if RGB565(255, 24, 25) != 0xF8C3 {
		t.Fatalf("enemy red = %x", RGB565(255, 24, 25))
	}
	r, g, b := RGB565ToRGB(0xFFFF)
	if r != 255 || g != 255 || b != 255 {
		t.Fatalf("white = %d,%d,%d", r, g, b)
	}
}

func TestQuantizedSample(t *testing.T) {
	s := MoverSample{X: 1.03, Y: -2.97, Orientation: 1.25, Carriages: []Carriage{{X: 0.5, Y: 0.51, Orientation: -0.25}}}
	q := s.Quantized()
	if q.X != 1.0 || q.Y != -3.0 || q.Orientation != 0.25 {
		t.Fatalf("quantized = %+v", q)
	}
	if q.Carriages[0].Orientation != 0.75 || q.Carriages[0].Y != 0.5 {
		t.Fatalf("carriage = %+v", q.Carriages[0])
	}
	if &q.Carriages[0] == &s.Carriages[0] {
		t.Fatal("carriages share storage")
	}
	g := MoverSample{X: 5, Flags: FlagGone, Carriages: s.Carriages}.Quantized()
	if g.X != 0 || g.Carriages != nil {
		t.Fatalf("gone not cleared: %+v", g)
	}
}

func TestIntervalHelpers(t *testing.T) {
	iv := Interval{Kind: OverlayTag, Key: "1/2", Start: 10, End: 20, X: 1}
	if !iv.Overlaps(19, 30) || iv.Overlaps(20, 30) || iv.Overlaps(0, 10) {
		t.Fatal("Overlaps wrong")
	}
	open := iv
	open.End = 0
	if !open.Overlaps(1000, 2000) || !open.Open() {
		t.Fatal("open interval wrong")
	}
	o := iv
	o.Start, o.End = 99, 0
	if !iv.SameContent(o) {
		t.Fatal("SameContent ignores times")
	}
	o.Text = "x"
	if iv.SameContent(o) {
		t.Fatal("SameContent compares text")
	}
}

func TestChartRoundTrip(t *testing.T) {
	var px ChunkPixels
	for i := range px {
		px[i] = uint16(i * 61)
	}
	d := EncodeChart(&px)
	got, err := DecodeChart(d)
	if err != nil || *got != px {
		t.Fatalf("chart round trip: %v", err)
	}
	if _, err := DecodeChart("not base64!"); err == nil {
		t.Fatal("bad base64 accepted")
	}
	short := EncodeChart(&px)[:20]
	if _, err := DecodeChart(short); err == nil {
		t.Fatal("truncated chart accepted")
	}
}
