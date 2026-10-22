package pyramid

// Cross-checks against the illisis/minimap static export (acceptance criterion:
// "the average matches illisis z1-4 on imported data, bit-exact on the final state").
//
// Set TSDB_TESTDATA to the export root (the directory holding maps.json.gz). The data is
// only read and decoded here; nothing in it is executed. The MMC1/MMS1 decoding below is
// written from the format description in the research notes (testdata.md §1-3, after
// illisis shared/codec.ts and shared/replay.ts); it is a test helper, not the importer
// (internal/import/minimap).

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/memstore"
)

func testdataRoot(t *testing.T) string {
	root := os.Getenv("TSDB_TESTDATA")
	if root == "" {
		t.Skip("TSDB_TESTDATA not set (illisis export root)")
	}
	if _, err := os.Stat(filepath.Join(root, "maps.json.gz")); err != nil {
		t.Skipf("TSDB_TESTDATA: %v", err)
	}
	return root
}

func readGz(t testing.TB, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return b
}

type mmIndex struct {
	Snaps   []int64    `json:"snaps"`
	Changes [][2]int64 `json:"changes"`
}

type mmRecord struct {
	t     int64
	block uint8
	pos   []uint16 // nil: whole block
	col   []uint16
}

func decodeMMC1(t testing.TB, t0 int64, b []byte) []mmRecord {
	if len(b) < 4 || string(b[:4]) != "MMC1" {
		t.Fatal("not MMC1")
	}
	var out []mmRecord
	o := 4
	for o < len(b) {
		if o+7 > len(b) {
			t.Fatal("truncated MMC1 record")
		}
		dt := binary.LittleEndian.Uint32(b[o:])
		r := mmRecord{t: t0 + int64(dt), block: b[o+4]}
		n := int(binary.LittleEndian.Uint16(b[o+5:]))
		o += 7
		if n == 0 {
			r.col = make([]uint16, core.ChunkPixelCount)
			for i := range r.col {
				r.col[i] = binary.LittleEndian.Uint16(b[o+2*i:])
			}
			o += 2 * core.ChunkPixelCount
		} else {
			r.pos, r.col = make([]uint16, n), make([]uint16, n)
			for i := 0; i < n; i++ {
				r.pos[i] = binary.LittleEndian.Uint16(b[o+4*i:])
				r.col[i] = binary.LittleEndian.Uint16(b[o+4*i+2:])
			}
			o += 4 * n
		}
		out = append(out, r)
	}
	return out
}

func applyMM(im *core.TileImage, r mmRecord) {
	if r.pos == nil {
		for p, c := range r.col {
			im.Pix[core.TilePixelIndex(r.block, p)] = c
		}
		return
	}
	for i, p := range r.pos {
		im.Pix[core.TilePixelIndex(r.block, int(p))] = r.col[i]
	}
}

// mmArea is one illisis area directory.
type mmArea struct {
	dir    string
	ax, ay int32
	index  mmIndex
}

func listAreas(t testing.TB, levelDir string) []mmArea {
	ents, err := os.ReadDir(levelDir)
	if err != nil {
		return nil
	}
	var out []mmArea
	for _, e := range ents {
		xs, ys, ok := strings.Cut(e.Name(), "_")
		if !e.IsDir() || !ok {
			continue
		}
		ax, e1 := strconv.Atoi(xs)
		ay, e2 := strconv.Atoi(ys)
		dir := filepath.Join(levelDir, e.Name())
		if e1 != nil || e2 != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "index.json.gz")); err != nil {
			continue
		}
		a := mmArea{dir: dir, ax: int32(ax), ay: int32(ay)}
		if err := json.Unmarshal(readGz(t, filepath.Join(dir, "index.json.gz")), &a.index); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// finalImage is the area's latest state: the newest snapshot plus the change files
// after it (illisis shared/replay.ts).
func finalImage(t testing.TB, a mmArea) *core.TileImage {
	im := core.NewTileImage()
	snap := int64(math.MinInt64)
	if n := len(a.index.Snaps); n > 0 {
		snap = a.index.Snaps[n-1]
		b := readGz(t, filepath.Join(a.dir, fmt.Sprintf("%d.s.gz", snap)))
		if len(b) != 4+2*core.TilePixelCount || string(b[:4]) != "MMS1" {
			t.Fatalf("%s: bad snapshot", a.dir)
		}
		for i := range im.Pix {
			im.Pix[i] = binary.LittleEndian.Uint16(b[4+2*i:])
		}
	}
	for _, c := range a.index.Changes {
		if c[0] <= snap {
			continue
		}
		for _, r := range decodeMMC1(t, c[0], readGz(t, filepath.Join(a.dir, fmt.Sprintf("%d.c.gz", c[0])))) {
			applyMM(im, r)
		}
	}
	return im
}

// mmSurface is one (map, surface, force) of the export.
type mmSurface struct {
	dir string
	key core.DatastoreKey
}

func unname(t testing.TB, s string) string {
	u, err := url.PathUnescape(strings.ReplaceAll(s, "~", "%"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func listSurfaces(t testing.TB, root string) []mmSurface {
	var out []mmSurface
	maps, _ := os.ReadDir(root)
	for _, m := range maps {
		if !m.IsDir() || m.Name() == "icons" {
			continue
		}
		surfaces, _ := os.ReadDir(filepath.Join(root, m.Name()))
		for _, s := range surfaces {
			forces, _ := os.ReadDir(filepath.Join(root, m.Name(), s.Name()))
			for _, f := range forces {
				dir := filepath.Join(root, m.Name(), s.Name(), f.Name())
				if _, err := os.Stat(filepath.Join(dir, "0")); err != nil {
					continue
				}
				out = append(out, mmSurface{dir: dir, key: core.DatastoreKey{Map: unname(t, m.Name()), Surface: unname(t, s.Name()), Force: unname(t, f.Name())}})
			}
		}
	}
	return out
}

// compareCoarse compares the pyramid's latest coarse tiles at z = -1..-4 with the final
// illisis z1..z4 areas and returns the number of tiles compared.
func compareCoarse(t *testing.T, p *Pyramid, s mmSurface) int {
	ctx := context.Background()
	n := 0
	for z := 1; z <= 4; z++ {
		areas := listAreas(t, filepath.Join(s.dir, strconv.Itoa(z)))
		ours := map[core.TileCoord]core.TileInfo{}
		for _, ti := range must[[]core.TileInfo](t)(p.ListTiles(ctx, s.key, int8(-z))) {
			ours[ti.Tile] = ti
		}
		for _, a := range areas {
			tile := core.TileCoord{Z: int8(-z), X: a.ax, Y: a.ay}
			want := finalImage(t, a)
			got := core.NewTileImage()
			if ti, ok := ours[tile]; ok {
				got = must[*core.TileImage](t)(p.ReadVersion(ctx, s.key, tile, ti.Version))
				delete(ours, tile)
			}
			if !got.Equal(want) {
				diff := 0
				for i := range got.Pix {
					if got.Pix[i] != want.Pix[i] {
						diff++
					}
				}
				t.Fatalf("%v %v: %d pixels differ from illisis z%d area %d_%d", s.key, tile, diff, z, a.ax, a.ay)
			}
			n++
		}
		for tile := range ours {
			im := must[*core.TileImage](t)(p.ReadVersion(ctx, s.key, tile, ours[tile].Version))
			if !im.IsEmpty() {
				t.Fatalf("%v %v: not in illisis but has pixels", s.key, tile)
			}
		}
	}
	return n
}

// TestIllisisFinalState: feed the final z0 state of every surface into the reference
// terrain, flush the pyramid and compare z-1..-4 with illisis z1..z4, bit for bit.
func TestIllisisFinalState(t *testing.T) {
	root := testdataRoot(t)
	ctx := context.Background()
	total := 0
	for _, s := range listSurfaces(t, root) {
		ter := memstore.NewTerrain()
		var ups []core.ChunkUpdate
		for _, a := range listAreas(t, filepath.Join(s.dir, "0")) {
			im := finalImage(t, a)
			tile := core.TileCoord{Z: 0, X: a.ax, Y: a.ay}
			for b := 0; b < core.TileBlocks; b++ {
				px := im.Block(uint8(b))
				if px != (core.ChunkPixels{}) {
					ups = append(ups, core.ChunkUpdate{DS: s.key, Chunk: tile.Chunk(uint8(b)), T: t0, Pixels: &px})
				}
			}
		}
		must[core.ApplyResult](t)(ter.Apply(ctx, ups))
		if err := ter.Commit(ctx, t0+2*W); err != nil {
			t.Fatal(err)
		}
		p := newPyramid(t, ter, memstore.NewSeriesStore(), Options{})
		must[BuildStats](t)(p.Flush(ctx))
		n := compareCoarse(t, p, s)
		t.Logf("%v: %d chunks, %d coarse areas identical to illisis z1-4", s.key, len(ups), n)
		total += n
	}
	if total == 0 {
		t.Fatal("no coarse areas compared")
	}
	t.Logf("total: %d coarse areas bit-identical", total)
}
