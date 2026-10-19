package benchlib

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGz(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mmc1 builds an MMC1 change file: records (dt, block, pixels); nil pixels with a full
// slice means a whole block.
type mmcRec struct {
	dt    uint32
	block byte
	pos   []uint16
	col   []uint16
}

func mmc1(recs []mmcRec) []byte {
	b := []byte("MMC1")
	for _, r := range recs {
		b = binary.LittleEndian.AppendUint32(b, r.dt)
		b = append(b, r.block)
		if r.pos == nil {
			b = binary.LittleEndian.AppendUint16(b, 0)
			for _, c := range r.col {
				b = binary.LittleEndian.AppendUint16(b, c)
			}
			continue
		}
		b = binary.LittleEndian.AppendUint16(b, uint16(len(r.pos)))
		for i := range r.pos {
			b = binary.LittleEndian.AppendUint16(b, r.pos[i])
			b = binary.LittleEndian.AppendUint16(b, r.col[i])
		}
	}
	return b
}

func TestIllisisStream(t *testing.T) {
	root := t.TempDir()
	writeGz(t, filepath.Join(root, "maps.json.gz"), []byte(`{"maps":{}}`))
	adir := filepath.Join(root, "gw-x~2E1", "nauvis", "player", "0", "0_-1")
	writeGz(t, filepath.Join(adir, "index.json.gz"), []byte(`{"changes":[[60000,3],[180000,1]],"snaps":[]}`))
	full := make([]uint16, 1024)
	for i := range full {
		full[i] = 7
	}
	writeGz(t, filepath.Join(adir, "60000.c.gz"), mmc1([]mmcRec{
		{dt: 0, block: 17, col: full},                             // t 60000, window 1: block 17 charted
		{dt: 10, block: 17, pos: []uint16{5}, col: []uint16{9}},   // t 60010, same window
		{dt: 20, block: 3, pos: []uint16{1}, col: []uint16{0}},    // t 60020: no net change, no row
		{dt: 60000, block: 3, pos: []uint16{2}, col: []uint16{4}}, // t 120000, window 2
	}))
	writeGz(t, filepath.Join(adir, "180000.c.gz"), mmc1([]mmcRec{
		{dt: 5, block: 17, pos: []uint16{5}, col: []uint16{9}}, // t 180005: same colour, no row
	}))
	// A mover-only map still gets an (empty) file, as to_stream.py writes one.
	writeGz(t, filepath.Join(root, "i-1", "nauvis", "m", "1.m.gz"), []byte(`{}`))

	out := t.TempDir()
	files, err := WriteIllisisStream(root, out, StreamOptions{CoalesceMs: 60000})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Map != "gw-x~2E1" || files[0].Rows != 2 || files[1].Rows != 0 {
		t.Fatalf("files %+v", files)
	}
	b, _ := os.ReadFile(files[0].Path)
	ls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(ls) != 2 {
		t.Fatalf("rows %q", ls)
	}
	// Block 17 is chunk (16*0-8+1, 16*-1-8+1) = (-7, -23); block 3 is (-5, -24).
	r0, err := ParseTSVRow("gw-x.1", ls[0])
	if err != nil {
		t.Fatal(err)
	}
	if r0.T != 60010 || r0.CX != -7 || r0.CY != -23 || r0.Surface != "nauvis" || r0.Force != "player" || r0.Pixels[5] != 9 || r0.Pixels[0] != 7 {
		t.Fatalf("row 0 %+v", r0)
	}
	r1, _ := ParseTSVRow("gw-x.1", ls[1])
	if r1.T != 120000 || r1.CX != -5 || r1.CY != -24 || r1.Pixels[2] != 4 || r1.Pixels[1] != 0 {
		t.Fatalf("row 1 t=%d (%d,%d)", r1.T, r1.CX, r1.CY)
	}

	// Without coalescing every changing record is a row.
	files, err = WriteIllisisStream(root, t.TempDir(), StreamOptions{Map: "gw-x~2E1"})
	if err != nil || len(files) != 1 || files[0].Rows != 3 {
		t.Fatalf("uncoalesced %+v %v", files, err)
	}
	if UnnameIllisis("gw-bc41437d~2E1") != "gw-bc41437d.1" || UnnameIllisis("a~4") != "a~4" {
		t.Fatal("unname")
	}
}
