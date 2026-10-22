// Package minimaptest writes a small synthetic illisis/minimap export for tests of the
// importer and the import-minimap command. It encodes the files itself (it does not use
// package minimap), so the importer's decoders are tested against an independent
// writer.
package minimaptest

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"timeshadedb/internal/core"
)

// T0 is the base time of the synthetic data: a multiple of 300 s.
const T0 int64 = 1791630000000

// Record is one MMC1 record: Pos nil means a whole block (Col has 1024 colours).
type Record struct {
	T     int64
	Block uint8
	Pos   []uint16
	Col   []uint16
}

// Export describes the written export and what an import must produce from it.
type Export struct {
	Root    string
	DS      core.DatastoreKey
	Surface core.SurfaceKey
	// Final is every z0 area's final image, by v3 tile.
	Final map[core.TileCoord]*core.TileImage
	// Versions is the number of committed versions per z0 tile at a 60 s window.
	Versions map[core.TileCoord]int
	// Journey ids.
	Train, Parked, Player, Vehicle string
	// RetimedTrain are the expected times of the two clamped train samples.
	RetimedTrain [2]int64
	// EndSwitchT is the time of the train sample that is an end switch.
	EndSwitchT int64
	// Intervals is the number of overlay intervals the store must hold.
	Intervals int
}

// Write builds the export under dir and returns its description.
func Write(t testing.TB, dir string) *Export {
	t.Helper()
	e := &Export{
		Root:     dir,
		DS:       core.DatastoreKey{Map: "gw-test.1", Surface: "nauvis", Force: "player"},
		Surface:  core.SurfaceKey{Map: "gw-test.1", Surface: "nauvis"},
		Final:    map[core.TileCoord]*core.TileImage{},
		Versions: map[core.TileCoord]int{{X: 0, Y: 0}: 3, {X: -1, Y: 0}: 1},
		Train:    "1.10.5", Parked: "2.30.9", Player: "p:alice", Vehicle: "1.20.v7",
		RetimedTrain: [2]int64{T0 + 299500, T0 + 299800},
		EndSwitchT:   T0 + 302000,
		Intervals:    3,
	}
	forceDir := filepath.Join(dir, "gw-test~2E1", "nauvis", "player")

	fill := func(c uint16) []uint16 {
		out := make([]uint16, 1024)
		for i := range out {
			out[i] = c
		}
		return out
	}
	seq := func(n int) []uint16 {
		out := make([]uint16, n)
		for i := range out {
			out[i] = uint16(i)
		}
		return out
	}
	// Area 0_0: three change files. Window 1 nets block 0 out (A -> B -> A).
	fileA := []Record{
		{T: T0 + 1000, Block: 0, Col: fill(0x1111)},
		{T: T0 + 2000, Block: 17, Pos: seq(10), Col: fill(0x2222)[:10]},
		{T: T0 + 30000, Block: 17, Pos: []uint16{5}, Col: []uint16{0xF8C3}},
		{T: T0 + 40000, Block: 17, Pos: []uint16{5}, Col: []uint16{0x2222}},
	}
	fileB := []Record{
		{T: T0 + 61000, Block: 0, Pos: []uint16{3}, Col: []uint16{0x3333}},
		{T: T0 + 62000, Block: 0, Pos: []uint16{3}, Col: []uint16{0x1111}},
		{T: T0 + 90000, Block: 17, Pos: []uint16{100}, Col: []uint16{0x4444}},
	}
	fileC := []Record{
		{T: T0 + 300500, Block: 17, Col: fill(0)},
		{T: T0 + 301000, Block: 255, Pos: []uint16{1023}, Col: []uint16{0x5555}},
	}
	a00 := filepath.Join(forceDir, "0", "0_0")
	img := make([]uint16, 512*512)
	for _, r := range append(append([]Record{}, fileA...), fileB...) {
		apply(img, r)
	}
	snapT := T0 + 120000
	writeGz(t, filepath.Join(a00, strconv.FormatInt(snapT, 10)+".s.gz"), snap(img))
	for _, r := range fileC {
		apply(img, r)
	}
	e.Final[core.TileCoord{X: 0, Y: 0}] = &core.TileImage{Pix: img}
	for _, f := range [][]Record{fileA, fileB, fileC} {
		writeGz(t, filepath.Join(a00, strconv.FormatInt(f[0].T, 10)+".c.gz"), change(f[0].T, f))
	}
	writeJSON(t, filepath.Join(a00, "index.json.gz"), map[string]any{
		"snaps":   []int64{snapT},
		"changes": [][2]int64{{fileA[0].T, fileA[3].T}, {fileB[0].T, fileB[2].T}, {fileC[0].T, fileC[1].T}},
	})
	writeJSON(t, filepath.Join(a00, "recipes.json.gz"), [][]any{
		{1, 55, "player", T0 + 1000, nil, 3.5, 4.5, "iron-plate", "normal"},
	})

	// Area -1_0: one whole block.
	am10 := filepath.Join(forceDir, "0", "-1_0")
	fileD := []Record{{T: T0 + 5000, Block: 8, Col: fill(0x6666)}}
	img2 := make([]uint16, 512*512)
	apply(img2, fileD[0])
	e.Final[core.TileCoord{X: -1, Y: 0}] = &core.TileImage{Pix: img2}
	writeGz(t, filepath.Join(am10, strconv.FormatInt(fileD[0].T, 10)+".c.gz"), change(fileD[0].T, fileD))
	writeJSON(t, filepath.Join(am10, "index.json.gz"), map[string]any{"snaps": []int64{}, "changes": [][2]int64{{T0 + 5000, T0 + 5000}}})

	// Level 1: the 2x2 averages of the final z0 state, as snapshots.
	for _, tile := range []core.TileCoord{{Z: -1, X: 0, Y: 0}, {Z: -1, X: -1, Y: 0}} {
		var kids [4]*core.TileImage
		for i, c := range tile.Children() {
			kids[i] = e.Final[c]
		}
		dir1 := filepath.Join(forceDir, "1", strconv.Itoa(int(tile.X))+"_"+strconv.Itoa(int(tile.Y)))
		writeGz(t, filepath.Join(dir1, strconv.FormatInt(T0+600000, 10)+".s.gz"), snap(core.Downsample(kids).Pix))
		writeJSON(t, filepath.Join(dir1, "index.json.gz"), map[string]any{"snaps": []int64{T0 + 600000}, "changes": [][2]int64{}})
	}

	// Tags: two versions of one tag (replaced at T0+5000) and an empty version.
	writeJSON(t, filepath.Join(forceDir, "tags.json.gz"), [][]any{
		{1, 0, "player", T0 + 1000, T0 + 5000, 10, 10, "item/iron-ore", "a"},
		{1, 0, "player", T0 + 5000, nil, 10, 10, "item/iron-ore", "b"},
		{1, 1, "player", T0 + 2000, T0 + 2000, 0, 0, "", ""},
	})

	// Movers. Window 1 (named by its earliest row, T0+1000).
	mdir := filepath.Join(dir, "gw-test~2E1", "nauvis", "m")
	w1 := T0 + 1000
	movers := [][]any{
		{e.Train, "t", "player", "locomotive", 2, "LC", "f20000"},
		{e.Player, "p", "player", "alice"},
		{e.Vehicle, "v", "player", "car"},
	}
	row := func(m int, t int64, xs ...float64) []any {
		out := []any{m, t - w1}
		for _, x := range xs {
			out = append(out, x)
		}
		return out
	}
	rows1 := [][]any{
		row(1, T0+1000, 5, 5, 0, 0),
		row(2, T0+1000, 0, 0, 0, -2),
		row(1, T0+2000, 6, 5, 1, 0),
		row(2, T0+2000, 0, -2, 2, 0),
		row(1, T0+3000), // gone, handoff (sample 0.5 s later)
		row(2, T0+3000, 2, -2, 0, 0),
		row(1, T0+3500, 7, 5, 1, 0),
		row(1, T0+10000), // gone, an end
		row(1, T0+60000, 8, 5, 0, 0),
		row(0, T0+297000, 0, 0, 10, 0, 0.25),
		row(0, T0+298000, 10, 0, 10, 0, 0.25),
		row(0, T0+299000, 20, 0, 10, 0, 0.25),
	}
	writeJSON(t, filepath.Join(mdir, strconv.FormatInt(w1, 10)+".m.gz"), map[string]any{"t": w1, "movers": movers, "s": rows1})
	// Window 2: named by its floor T0+300001; two train samples clamped to it.
	w2 := T0 + 300001
	row2 := func(t int64, xs ...float64) []any {
		out := []any{0, t - w2}
		for _, x := range xs {
			out = append(out, x)
		}
		return out
	}
	rows2 := [][]any{
		row2(w2, 25, 0, 10, 0, 0.25),
		row2(w2, 28, 0, 10, 0, 0.25),
		row2(T0+300300, 33, 0, 10, 0, 0.25),
		row2(T0+300300, 33.25, 0, 10, 0, 0.25), // duplicate time: the later row wins
		row2(T0+300400, 33, 0, 0, 0, 0.25),
		row2(T0+302000, 26, 0, 0, 0, 0.75), // end switch: 7 tiles at rest, 2 carriages
	}
	writeJSON(t, filepath.Join(mdir, strconv.FormatInt(w2, 10)+".m.gz"), map[string]any{"t": w2, "movers": movers[:1], "s": rows2})
	// Keyframe: a train parked since before the data (seeded) and the moving train.
	writeJSON(t, filepath.Join(mdir, strconv.FormatInt(T0+300000, 10)+".k.gz"), map[string]any{"t": T0 + 300000, "movers": [][]any{
		{e.Parked, "t", "player", "locomotive", 3, 100, 200, 0, 0, T0 - 50000, "LCC", "00ff00", 0.5},
		{e.Train, "t", "player", "locomotive", 2, 20, 0, 10, 0, T0 + 299000, "LC", "f20000", 0.25},
	}})

	writeJSON(t, filepath.Join(dir, "maps.json.gz"), map[string]any{
		"updated": T0 + 600000, "window_ms": 300000, "live_ms": 5000,
		"maps": map[string]any{
			"gw-test~2E1": map[string]any{
				"title": "Gridworld test.1", "tileSize": 512,
				"instances": map[string]any{
					"2": map[string]any{"name": "gridworld_1_0", "bounds": []int{256, -256, 768, 256}},
					"1": map[string]any{"name": "gridworld_0_0", "bounds": []int{-256, -256, 256, 256}},
				},
				"surfaces": map[string]any{
					"nauvis": map[string]any{
						"first": T0 + 1000, "live": true, "days": []string{"2026-10-10"},
						"edges":  [][]int{{256, -256, 256, 256}},
						"forces": map[string]any{"player": map[string]any{"tags": true, "areas": [][]string{{"-1_0", "0_0"}, {"-1_0", "0_0"}, {}, {}, {}}}},
					},
				},
			},
		},
	})
	return e
}

func apply(px []uint16, r Record) {
	at := func(p int) int {
		return ((int(r.Block>>4)*32)+(p>>5))*512 + int(r.Block&15)*32 + (p & 31)
	}
	if r.Pos == nil {
		for p, c := range r.Col {
			px[at(p)] = c
		}
		return
	}
	for i, p := range r.Pos {
		px[at(int(p))] = r.Col[i]
	}
}

func change(t0 int64, recs []Record) []byte {
	var b bytes.Buffer
	b.WriteString("MMC1")
	for _, r := range recs {
		var hdr [7]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(r.T-t0))
		hdr[4] = r.Block
		if r.Pos != nil {
			binary.LittleEndian.PutUint16(hdr[5:], uint16(len(r.Pos)))
		}
		b.Write(hdr[:])
		for i, c := range r.Col {
			if r.Pos != nil {
				binary.Write(&b, binary.LittleEndian, r.Pos[i])
			}
			binary.Write(&b, binary.LittleEndian, c)
		}
	}
	return b.Bytes()
}

func snap(px []uint16) []byte {
	var b bytes.Buffer
	b.WriteString("MMS1")
	binary.Write(&b, binary.LittleEndian, px)
	return b.Bytes()
}

func writeGz(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t testing.TB, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeGz(t, path, b)
}
