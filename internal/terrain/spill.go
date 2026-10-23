package terrain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"timeshadedb/internal/core"
)

// Spilling open windows (docs/FORMAT.md 5.7). The open windows of a datastore hold the
// newest update per chunk and window. When they would grow past Options.OpenWindowBytes,
// Apply moves whole windows, oldest start first across datastores, out of memory into
// datastores/<id>/open/<window start>.spill instead of refusing the batch; updates that
// arrive later for a spilled window go to a fresh in-memory window as usual. The commit
// that closes a spilled window merges, per tile, the spilled chunks with the in-memory
// ones (greatest T wins, and on equal T the in-memory update, which arrived later), and
// live reads of a tile with spilled windows read its part of the spill file. A spill file
// is a cache of the WAL, which still holds every update of the window: Open deletes the
// spill files and the WAL replay spills again as it fills the budget, so a crash during a
// spill or a commit loses nothing.
//
// A spill file is a sequence of frames, one per tile and spill (a window spilled twice
// has two frames for a tile; the later one wins on equal T), in the WAL frame layout
// (u32 payload length, u32 CRC-32C, one zstd frame). Decompressed: varint tile x, varint
// tile y, uvarint n, then n chunks of u8 block, varint T, u8 flags (bit 0 deleted) and,
// unless deleted, 1,024 RGB565 pixels (u16 LE).

// spillWin is a spilled window of a datastore: where each tile's frames are.
type spillWin struct {
	path   string
	size   int64
	frames map[core.TileCoord][]spillRange
}

type spillRange struct{ off, n int64 }

func (d *dsState) spillPath(ws core.TimeMs) string {
	return filepath.Join(d.dir, "open", strconv.FormatInt(int64(ws), 10)+".spill")
}

// clearSpills deletes a datastore's spill files (Open: they are caches of the WAL).
func clearSpills(dir string) error {
	err := os.RemoveAll(filepath.Join(dir, "open"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// spillCand is one open window of a datastore that can be spilled.
type spillCand struct {
	d     *dsState
	ws    core.TimeMs
	bytes int64
}

// spill moves open windows to spill files, oldest start first across the datastores
// (and extra, a datastore being opened), until the open-window bytes are at most target
// or nothing more can go. Windows a commit is closing (below the late threshold) stay.
func (s *Store) spill(target int64, extra *dsState) error {
	s.spillMu.Lock()
	defer s.spillMu.Unlock()
	if s.openBytes.Load() <= target {
		return nil
	}
	s.mu.RLock()
	dss := make([]*dsState, 0, len(s.ds)+1)
	for _, d := range s.ds {
		dss = append(dss, d)
	}
	s.mu.RUnlock()
	if extra != nil {
		dss = append(dss, extra)
	}
	var cands []spillCand
	for _, d := range dss {
		d.mu.RLock()
		byWS := map[core.TimeMs]int64{}
		for _, tl := range d.tiles {
			tl.mu.Lock()
			for ws, win := range tl.open {
				if ws >= d.applyHorizon {
					byWS[ws] += int64(len(win.chunks)) * openChunkBytes
				}
			}
			tl.mu.Unlock()
		}
		d.mu.RUnlock()
		for ws, b := range byWS {
			cands = append(cands, spillCand{d, ws, b})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.ws != b.ws {
			return a.ws < b.ws
		}
		return a.d.id < b.d.id
	})
	for _, c := range cands {
		if s.openBytes.Load() <= target {
			break
		}
		if err := s.spillWindow(c.d, c.ws); err != nil {
			return fmt.Errorf("terrain: spilling %s window %d: %w", c.d.id, c.ws, err)
		}
	}
	return nil
}

// spillWindow writes the in-memory chunks of window ws of every tile of d to the window's
// spill file (a frame per tile) and drops them from memory. It holds d.mu, so no update
// or commit collection runs meanwhile.
func (s *Store) spillWindow(d *dsState, ws core.TimeMs) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ws < d.applyHorizon {
		return nil // a commit took it meanwhile
	}
	tiles := make([]*tileState, 0)
	for _, tl := range d.tiles {
		tl.mu.Lock()
		if tl.open[ws] != nil {
			tiles = append(tiles, tl)
		}
		tl.mu.Unlock()
	}
	if len(tiles) == 0 {
		return nil
	}
	sort.Slice(tiles, func(i, j int) bool {
		a, b := tiles[i].coord, tiles[j].coord
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
	sp := d.spills[ws]
	if sp == nil {
		sp = &spillWin{path: d.spillPath(ws), frames: map[core.TileCoord][]spillRange{}}
	}
	if err := os.MkdirAll(filepath.Dir(sp.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(sp.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(sp.size); err != nil { // a torn earlier spill
		return err
	}
	var raw, out []byte
	off := sp.size
	added := map[core.TileCoord]spillRange{}
	for _, tl := range tiles {
		tl.mu.Lock()
		win := tl.open[ws]
		raw = binary.AppendVarint(raw[:0], int64(tl.coord.X))
		raw = binary.AppendVarint(raw, int64(tl.coord.Y))
		raw = binary.AppendUvarint(raw, uint64(len(win.chunks)))
		blocks := make([]int, 0, len(win.chunks))
		for b := range win.chunks {
			blocks = append(blocks, int(b))
		}
		sort.Ints(blocks)
		for _, b := range blocks {
			oc := win.chunks[uint8(b)]
			raw = append(raw, uint8(b))
			raw = binary.AppendVarint(raw, int64(oc.t))
			if oc.deleted {
				raw = append(raw, 1)
				continue
			}
			raw = append(raw, 0)
			for _, c := range oc.px {
				raw = binary.LittleEndian.AppendUint16(raw, c)
			}
		}
		tl.mu.Unlock()
		out = append(out[:0], make([]byte, walFrameHeader)...)
		out = s.walEnc.EncodeAll(raw, out)
		p := out[walFrameHeader:]
		binary.LittleEndian.PutUint32(out[0:], uint32(len(p)))
		binary.LittleEndian.PutUint32(out[4:], crcC(p))
		if _, err := f.WriteAt(out, off); err != nil {
			return err
		}
		added[tl.coord] = spillRange{off, int64(len(out))}
		off += int64(len(out))
	}
	// Written (to the OS: a crash discards the spill files anyway): drop from memory.
	var freed int64
	for _, tl := range tiles {
		tl.mu.Lock()
		win := tl.open[ws]
		freed += int64(len(win.chunks)) * openChunkBytes
		delete(tl.open, ws)
		if len(tl.open) == 0 {
			tl.open = nil
		}
		if len(sp.frames[tl.coord]) == 0 {
			tl.spilled++
		}
		tl.spillGen++
		sp.frames[tl.coord] = append(sp.frames[tl.coord], added[tl.coord])
		tl.mu.Unlock()
	}
	sp.size = off
	d.spills[ws] = sp
	s.openBytes.Add(-freed)
	s.stats.spills.Add(1)
	s.stats.spilledBytes.Add(uint64(freed))
	return nil
}

// spilledChunk is one chunk read back from a spill file.
type spilledChunk struct {
	block   uint8
	t       core.TimeMs
	deleted bool
	px      core.ChunkPixels
}

// readSpill reads tile tc's frames of a spilled window, in spill order, and calls fn
// for each chunk (a later frame's chunk replaces an earlier one on equal T).
func (s *Store) readSpill(path string, ranges []spillRange, tc core.TileCoord, fn func(*spilledChunk)) error {
	if len(ranges) == 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf, raw []byte
	var c spilledChunk
	for _, r := range ranges {
		if int64(cap(buf)) < r.n {
			buf = make([]byte, r.n)
		}
		buf = buf[:r.n]
		if _, err := f.ReadAt(buf, r.off); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(buf[0:])
		if int64(n)+walFrameHeader != r.n || crcC(buf[walFrameHeader:]) != binary.LittleEndian.Uint32(buf[4:]) {
			return fmt.Errorf("%s: bad frame at %d", path, r.off)
		}
		if raw, err = s.dec.DecodeAll(buf[walFrameHeader:], raw[:0]); err != nil {
			return fmt.Errorf("%s: frame at %d: %w", path, r.off, err)
		}
		p := raw
		x, k1 := binary.Varint(p)
		if k1 <= 0 {
			return errPayload
		}
		p = p[k1:]
		y, k2 := binary.Varint(p)
		if k2 <= 0 {
			return errPayload
		}
		p = p[k2:]
		if int32(x) != tc.X || int32(y) != tc.Y {
			return fmt.Errorf("%s: frame at %d is tile %d,%d, not %v", path, r.off, x, y, tc)
		}
		cnt, k := binary.Uvarint(p)
		if k <= 0 || cnt > core.TileBlocks {
			return errPayload
		}
		p = p[k:]
		for i := uint64(0); i < cnt; i++ {
			if len(p) < 1 {
				return errPayload
			}
			c.block = p[0]
			p = p[1:]
			t, k := binary.Varint(p)
			if k <= 0 || len(p) < k+1 {
				return errPayload
			}
			c.t = core.TimeMs(t)
			c.deleted = p[k]&1 != 0
			p = p[k+1:]
			if c.deleted {
				c.px = core.ChunkPixels{}
			} else {
				if len(p) < 2*core.ChunkPixelCount {
					return errPayload
				}
				for j := range c.px {
					c.px[j] = binary.LittleEndian.Uint16(p[2*j:])
				}
				p = p[2*core.ChunkPixelCount:]
			}
			fn(&c)
		}
	}
	return nil
}

// mergedWindow returns tile tc's content of window ws for a commit: the spilled chunks
// (frames in order, a later one wins on equal T) overlaid by mem, the chunks that arrived
// after the spill, where they are not older. mem may be nil.
func (s *Store) mergedWindow(sp *spillWin, tc core.TileCoord, mem *openWindow) (*openWindow, error) {
	out := &openWindow{chunks: map[uint8]*openChunk{}}
	err := s.readSpill(sp.path, sp.frames[tc], tc, func(c *spilledChunk) {
		if cur := out.chunks[c.block]; cur == nil || c.t >= cur.t {
			out.chunks[c.block] = &openChunk{t: c.t, deleted: c.deleted, px: c.px}
		}
	})
	if err != nil {
		return nil, err
	}
	if mem != nil {
		for b, oc := range mem.chunks {
			if cur := out.chunks[b]; cur == nil || oc.t >= cur.t {
				out.chunks[b] = oc
			}
		}
	}
	return out, nil
}

// liveSpills returns, per block, the newest chunk of tile tl in d's spilled windows
// (nil where none), for a live read, with the tile's spill generation it read them at: a
// read that finds another generation under the tile lock missed a spill and retries. A
// spill file a commit deleted meanwhile is os.ErrNotExist; the caller retries.
func (s *Store) liveSpills(d *dsState, tl *tileState) (*[core.TileBlocks]*openChunk, uint64, error) {
	tl.mu.Lock()
	n, gen := tl.spilled, tl.spillGen
	tl.mu.Unlock()
	if n == 0 {
		return &[core.TileBlocks]*openChunk{}, gen, nil
	}
	type part struct {
		ws     core.TimeMs
		path   string
		ranges []spillRange
	}
	var parts []part
	d.mu.RLock()
	for ws, sp := range d.spills {
		if r := sp.frames[tl.coord]; len(r) > 0 {
			parts = append(parts, part{ws, sp.path, append([]spillRange(nil), r...)})
		}
	}
	d.mu.RUnlock()
	sort.Slice(parts, func(i, j int) bool { return parts[i].ws < parts[j].ws })
	out := &[core.TileBlocks]*openChunk{}
	for _, p := range parts {
		err := s.readSpill(p.path, p.ranges, tl.coord, func(c *spilledChunk) {
			if cur := out[c.block]; cur == nil || c.t >= cur.t {
				out[c.block] = &openChunk{t: c.t, deleted: c.deleted, px: c.px}
			}
		})
		if err != nil {
			return nil, 0, err
		}
	}
	return out, gen, nil
}
