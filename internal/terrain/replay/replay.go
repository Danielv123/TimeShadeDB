// Package replay turns the illisis/minimap static export (the v3 test data) into a
// time-ordered stream of terrain chunk updates, for benchmarks and tests of the
// terrain store.
//
// It reads only the z0 change files (MMC1, illisis codec.ts) listed by each area's
// index.json.gz, merges all areas by time and keeps every chunk's full state, so each
// change record becomes one core.ChunkUpdate with the full 32x32 chart, as live capture
// would send it. It is a minimal reader for measurement; the importer (internal/import/minimap) is the
// complete one. The data directory is untrusted input: it is only parsed, and every
// length is checked.
package replay

import (
	"container/heap"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/klauspost/compress/gzip"

	"timeshadedb/internal/core"
)

// Record is one decoded MMC1 change record.
type Record struct {
	T     core.TimeMs
	DS    core.DatastoreKey
	Chunk core.ChunkCoord
	// Pos is nil when the record holds the whole chunk (Col has 1024 entries).
	Pos []uint16
	Col []uint16
}

// Area is one z0 area (= v3 z0 tile) of the export.
type Area struct {
	DS    core.DatastoreKey
	Tile  core.TileCoord
	dir   string
	files []int64 // change file start times, sorted
}

// Filter selects datastores; nil selects all.
type Filter func(core.DatastoreKey) bool

// Scan lists the z0 areas under root (the directory holding maps.json.gz).
func Scan(root string, filter Filter) ([]*Area, error) {
	var areas []*Area
	maps, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, m := range maps {
		if !m.IsDir() {
			continue
		}
		surfaces, _ := os.ReadDir(filepath.Join(root, m.Name()))
		for _, sf := range surfaces {
			if !sf.IsDir() {
				continue
			}
			forces, _ := os.ReadDir(filepath.Join(root, m.Name(), sf.Name()))
			for _, fo := range forces {
				if !fo.IsDir() || fo.Name() == "m" {
					continue
				}
				z0 := filepath.Join(root, m.Name(), sf.Name(), fo.Name(), "0")
				ads, err := os.ReadDir(z0)
				if err != nil {
					continue
				}
				key := core.DatastoreKey{Map: unname(m.Name()), Surface: unname(sf.Name()), Force: unname(fo.Name())}
				if key.Validate() != nil || (filter != nil && !filter(key)) {
					continue
				}
				for _, ad := range ads {
					xs, ys, ok := strings.Cut(ad.Name(), "_")
					if !ad.IsDir() || !ok {
						continue
					}
					x, err1 := strconv.Atoi(xs)
					y, err2 := strconv.Atoi(ys)
					tile := core.TileCoord{X: int32(x), Y: int32(y)}
					if err1 != nil || err2 != nil || !tile.Valid() {
						continue
					}
					a := &Area{DS: key, Tile: tile, dir: filepath.Join(z0, ad.Name())}
					if err := a.readIndex(); err != nil {
						return nil, fmt.Errorf("%s: %w", a.dir, err)
					}
					if len(a.files) > 0 {
						areas = append(areas, a)
					}
				}
			}
		}
	}
	return areas, nil
}

// unname reverses illisis keys.ts name(): "~XX" is one byte of the UTF-8 name.
func unname(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '~' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func readGz(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	b, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, limit)
	}
	return b, nil
}

func (a *Area) readIndex() error {
	raw, err := readGz(filepath.Join(a.dir, "index.json.gz"), 64<<20)
	if err != nil {
		return err
	}
	var idx struct {
		Changes [][2]int64 `json:"changes"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return err
	}
	for _, c := range idx.Changes {
		a.files = append(a.files, c[0])
	}
	sort.Slice(a.files, func(i, j int) bool { return a.files[i] < a.files[j] })
	return nil
}

// decodeMMC1 parses one change file (illisis codec.ts:47-106): "MMC1", then records of
// u32 dt, u8 block, u16 n, then 1024 colours (n = 0) or n (pos, colour) pairs.
func (a *Area) decodeMMC1(t0 int64, b []byte) ([]Record, error) {
	if len(b) < 4 || string(b[:4]) != "MMC1" {
		return nil, fmt.Errorf("not MMC1")
	}
	var out []Record
	o := 4
	for o < len(b) {
		if len(b)-o < 7 {
			return nil, fmt.Errorf("truncated record header at %d", o)
		}
		dt := binary.LittleEndian.Uint32(b[o:])
		block := b[o+4]
		n := int(binary.LittleEndian.Uint16(b[o+5:]))
		o += 7
		r := Record{T: core.TimeMs(t0 + int64(dt)), DS: a.DS, Chunk: a.Tile.Chunk(block)}
		if n == 0 {
			if len(b)-o < 2048 {
				return nil, fmt.Errorf("truncated whole record at %d", o)
			}
			r.Col = make([]uint16, core.ChunkPixelCount)
			for i := range r.Col {
				r.Col[i] = binary.LittleEndian.Uint16(b[o+2*i:])
			}
			o += 2048
		} else {
			if len(b)-o < 4*n {
				return nil, fmt.Errorf("truncated record at %d", o)
			}
			r.Pos = make([]uint16, n)
			r.Col = make([]uint16, n)
			for i := 0; i < n; i++ {
				r.Pos[i] = binary.LittleEndian.Uint16(b[o+4*i:]) & (core.ChunkPixelCount - 1)
				r.Col[i] = binary.LittleEndian.Uint16(b[o+4*i+2:])
			}
			o += 4 * n
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
}

// areaIter walks one area's records in time order, one change file at a time.
type areaIter struct {
	a    *Area
	ord  int
	file int
	recs []Record
	i    int
}

func (it *areaIter) cur() *Record { return &it.recs[it.i] }

// advance moves to the next record, loading files as needed. It returns false at the end.
func (it *areaIter) advance() (bool, error) {
	it.i++
	for it.i >= len(it.recs) {
		if it.file >= len(it.a.files) {
			return false, nil
		}
		t0 := it.a.files[it.file]
		it.file++
		raw, err := readGz(filepath.Join(it.a.dir, strconv.FormatInt(t0, 10)+".c.gz"), 256<<20)
		if err != nil {
			return false, err
		}
		if it.recs, err = it.a.decodeMMC1(t0, raw); err != nil {
			return false, fmt.Errorf("%s/%d.c.gz: %w", it.a.dir, t0, err)
		}
		it.i = 0
	}
	return true, nil
}

type iterHeap []*areaIter

func (h iterHeap) Len() int { return len(h) }
func (h iterHeap) Less(i, j int) bool {
	a, b := h[i].cur().T, h[j].cur().T
	if a != b {
		return a < b
	}
	return h[i].ord < h[j].ord
}
func (h iterHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *iterHeap) Push(x any)   { *h = append(*h, x.(*areaIter)) }
func (h *iterHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Stream merges areas by time and produces full-chunk updates.
type Stream struct {
	h     iterHeap
	state map[chunkKey]*core.ChunkPixels
	// Records counts the change records read so far.
	Records int
}

type chunkKey struct {
	ds    core.DatastoreKey
	chunk core.ChunkCoord
}

// NewStream starts a merged stream over the areas.
func NewStream(areas []*Area) (*Stream, error) {
	s := &Stream{state: map[chunkKey]*core.ChunkPixels{}}
	for i, a := range areas {
		it := &areaIter{a: a, ord: i, i: -1}
		ok, err := it.advance()
		if err != nil {
			return nil, err
		}
		if ok {
			s.h = append(s.h, it)
		}
	}
	heap.Init(&s.h)
	return s, nil
}

// Next fills u with the next update (its Pixels points at a buffer owned by the
// stream, valid until the next call to Next) and returns false at the end.
func (s *Stream) Next(u *core.ChunkUpdate, buf *core.ChunkPixels) (bool, error) {
	if len(s.h) == 0 {
		return false, nil
	}
	it := s.h[0]
	r := it.cur()
	k := chunkKey{r.DS, r.Chunk}
	st := s.state[k]
	if st == nil {
		st = &core.ChunkPixels{}
		s.state[k] = st
	}
	if r.Pos == nil {
		copy(st[:], r.Col)
	} else {
		for i, p := range r.Pos {
			st[p] = r.Col[i]
		}
	}
	*buf = *st
	*u = core.ChunkUpdate{DS: r.DS, Chunk: r.Chunk, T: r.T, Pixels: buf}
	s.Records++
	ok, err := it.advance()
	if err != nil {
		return false, err
	}
	if ok {
		heap.Fix(&s.h, 0)
	} else {
		heap.Pop(&s.h)
	}
	return true, nil
}

// Chunks returns the number of distinct chunks seen so far.
func (s *Stream) Chunks() int { return len(s.state) }

// FinalState returns the current full state of every chunk seen (for checks against
// the store's latest content).
func (s *Stream) FinalState() map[core.DatastoreKey]map[core.ChunkCoord]*core.ChunkPixels {
	out := map[core.DatastoreKey]map[core.ChunkCoord]*core.ChunkPixels{}
	for k, px := range s.state {
		m := out[k.ds]
		if m == nil {
			m = map[core.ChunkCoord]*core.ChunkPixels{}
			out[k.ds] = m
		}
		m[k.chunk] = px
	}
	return out
}
