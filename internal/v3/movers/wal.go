package movers

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"timeshadedb/internal/v3/core"
)

// The write-ahead log keeps the samples of open windows across a crash
// (docs/v3/FORMAT.md 5.5: "Live samples are kept in memory with a WAL").
//
// File movers/wal/wal.log is a sequence of frames, one per Append batch:
//
//	u32 payload length | u32 CRC-32C of the payload | payload
//
// The payload is a sequence of records:
//
//	'M' uvarint meta number, uvarint length, JSON {"s": SurfaceKey, "m": MoverMeta}
//	'S' uvarint meta number, varint t, varint tick, varint x, varint y, uvarint o,
//	    byte flags, uvarint n, n x (varint cx, varint cy, uvarint co)
//	    [with core.FlagRail: uvarint speed, uvarint pieces << 1 | rail start,
//	     pieces x (varint x, varint y, byte kind << 4 | direction)]
//
// Meta numbers are local to the file and never reused within it. At every commit that
// closes a window the log is rewritten (tmp + rename) with only the rows still open, so
// its size is bounded by the open windows. The rewrite's numbering takes effect only
// once the rename has succeeded, and numbers defined by a failed append are forgotten,
// so the numbering in memory always matches the file being appended to. A batch is
// split into frames of about walFrameTarget payload bytes, written with one write call.
// Replay stops at the first torn frame (bad length or CRC) and truncates there; a frame
// with a valid CRC that does not decode is skipped as a whole (its meta definitions too).

const (
	walRecMeta   = 'M'
	walRecSample = 'S'
	walMaxFrame  = 64 << 20
	// walFrameTarget is the payload size after which a batch continues in a new frame.
	walFrameTarget = 1 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

type walMetaKey struct {
	sfc  core.SurfaceKey
	meta core.MoverMeta
}

type walMetaJSON struct {
	S core.SurfaceKey `json:"s"`
	M core.MoverMeta  `json:"m"`
}

type wal struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	metaIDs  map[walMetaKey]uint64
	nextID   uint64
	buf      []byte
	interval time.Duration // 0: fsync every frame; > 0: group commit; < 0: never
	dirty    bool
	err      error // sticky write error
	skipped  int   // frames with a valid CRC that did not decode at replay
}

// walEntry is one row to log with its surface.
type walEntry struct {
	sfc core.SurfaceKey
	r   *row
}

func openWAL(dir string, interval time.Duration) (*wal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &wal{path: filepath.Join(dir, "wal.log"), interval: interval, metaIDs: map[walMetaKey]uint64{}, nextID: 1}
	return w, nil
}

// replay calls fn for every row of every valid frame, then truncates a torn tail and
// opens the file for appending.
func (w *wal) replay(fn func(sfc core.SurfaceKey, r row)) error {
	b, err := os.ReadFile(w.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	metas := map[uint64]walMetaJSON{}
	ptrs := map[uint64]*core.MoverMeta{}
	off := 0
	for off+8 <= len(b) {
		n := int(binary.LittleEndian.Uint32(b[off:]))
		crc := binary.LittleEndian.Uint32(b[off+4:])
		if n > walMaxFrame || off+8+n > len(b) {
			break
		}
		payload := b[off+8 : off+8+n]
		if crc32.Checksum(payload, castagnoli) != crc {
			break
		}
		if err := decodeWALFrame(payload, metas, ptrs, fn); err != nil {
			w.skipped++
		}
		off += 8 + n
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(off)); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Seek(int64(off), io.SeekStart); err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, int64(off)
	// Meta numbers continue after the replayed ones, so new frames can reference them.
	for id, m := range metas {
		w.metaIDs[walMetaKey{m.S, m.M}] = id
		w.nextID = max(w.nextID, id+1)
	}
	return nil
}

func decodeWALFrame(p []byte, metas map[uint64]walMetaJSON, ptrs map[uint64]*core.MoverMeta, fn func(core.SurfaceKey, row)) error {
	r := &colReader{b: p}
	type pending struct {
		sfc core.SurfaceKey
		r   row
	}
	var rows []pending
	// Meta definitions take effect only when the whole frame decodes.
	newMetas := map[uint64]walMetaJSON{}
	newPtrs := map[uint64]*core.MoverMeta{}
	lookup := func(id uint64) (walMetaJSON, *core.MoverMeta, bool) {
		if m, ok := newMetas[id]; ok {
			return m, newPtrs[id], true
		}
		m, ok := metas[id]
		return m, ptrs[id], ok
	}
	for r.off < len(r.b) && r.err == nil {
		switch r.byte() {
		case walRecMeta:
			id := r.uvarint()
			n := int(r.uvarint())
			if r.err != nil || n > len(r.b)-r.off {
				return fmt.Errorf("wal: bad meta record")
			}
			var m walMetaJSON
			if err := json.Unmarshal(r.b[r.off:r.off+n], &m); err != nil {
				return err
			}
			r.off += n
			newMetas[id] = m
			mm := m.M
			newPtrs[id] = &mm
		case walRecSample:
			id := r.uvarint()
			m, p, ok := lookup(id)
			if !ok {
				return fmt.Errorf("wal: sample references unknown meta %d", id)
			}
			x := row{meta: p}
			x.t = core.TimeMs(r.varint())
			x.tick = r.varint()
			x.x = int32(r.varint())
			x.y = int32(r.varint())
			x.o = uint16(r.uvarint())
			x.flags = core.MoverFlags(r.byte()) & storedFlags
			nc := int(r.uvarint())
			if nc > MaxCarriages || nc > len(r.b)-r.off {
				return fmt.Errorf("wal: %d carriages", nc)
			}
			if nc > 0 {
				x.car = make([]carriage, nc)
				for i := range x.car {
					x.car[i] = carriage{x: int32(r.varint()), y: int32(r.varint()), o: uint16(r.uvarint())}
				}
			}
			if x.flags&core.FlagRail != 0 {
				x.speed = uint32(r.uvarint())
				head := r.uvarint()
				np := head >> 1
				if np > core.MaxRailPieces || int(np) > len(r.b)-r.off {
					return fmt.Errorf("wal: %d rail pieces", np)
				}
				x.rstart = head&1 != 0
				if np > 0 {
					x.rail = make([]core.RailPiece, np)
					for i := range x.rail {
						px, py := int32(r.varint()), int32(r.varint())
						kd := r.byte()
						x.rail[i] = core.RailPiece{X: px, Y: py, Dir: kd & 15, Kind: kd >> 4}
					}
				}
			}
			rows = append(rows, pending{m.S, x})
		default:
			return fmt.Errorf("wal: unknown record")
		}
	}
	if r.err != nil {
		return r.err
	}
	for id, m := range newMetas {
		metas[id], ptrs[id] = m, newPtrs[id]
	}
	for _, p := range rows {
		fn(p.sfc, p.r)
	}
	return nil
}

// encodeEntries appends the records of entries to buf, defining metas as needed in ids
// (numbering from *next) and recording every key it defines in *added. buf ends with an
// open frame that starts at frameStart (an 8-byte header placeholder and the payload so
// far); once that payload passes walFrameTarget the frame is closed and a new one
// started. It returns buf and the start of the last, still open, frame.
func encodeEntries(buf []byte, frameStart int, entries []walEntry, ids map[walMetaKey]uint64, next *uint64, added *[]walMetaKey) ([]byte, int) {
	for _, e := range entries {
		if len(buf)-frameStart-8 >= walFrameTarget {
			frame(buf[frameStart:])
			frameStart = len(buf)
			buf = append(buf, make([]byte, 8)...)
		}
		k := walMetaKey{e.sfc, *e.r.meta}
		id, ok := ids[k]
		if !ok {
			id = *next
			*next++
			ids[k] = id
			if added != nil {
				*added = append(*added, k)
			}
			js, _ := json.Marshal(walMetaJSON{S: e.sfc, M: *e.r.meta})
			buf = append(buf, walRecMeta)
			buf = binary.AppendUvarint(buf, id)
			buf = binary.AppendUvarint(buf, uint64(len(js)))
			buf = append(buf, js...)
		}
		r := e.r
		buf = append(buf, walRecSample)
		buf = binary.AppendUvarint(buf, id)
		buf = binary.AppendVarint(buf, int64(r.t))
		buf = binary.AppendVarint(buf, r.tick)
		buf = binary.AppendVarint(buf, int64(r.x))
		buf = binary.AppendVarint(buf, int64(r.y))
		buf = binary.AppendUvarint(buf, uint64(r.o))
		buf = append(buf, byte(r.flags))
		buf = binary.AppendUvarint(buf, uint64(len(r.car)))
		for _, c := range r.car {
			buf = binary.AppendVarint(buf, int64(c.x))
			buf = binary.AppendVarint(buf, int64(c.y))
			buf = binary.AppendUvarint(buf, uint64(c.o))
		}
		if r.flags&core.FlagRail != 0 {
			buf = binary.AppendUvarint(buf, uint64(r.speed))
			head := uint64(len(r.rail)) << 1
			if r.rstart {
				head |= 1
			}
			buf = binary.AppendUvarint(buf, head)
			for _, p := range r.rail {
				buf = binary.AppendVarint(buf, int64(p.X))
				buf = binary.AppendVarint(buf, int64(p.Y))
				buf = append(buf, p.Kind<<4|p.Dir&15)
			}
		}
	}
	return buf, frameStart
}

// frame fills in the 8-byte frame header at the start of buf and returns buf.
func frame(buf []byte) []byte {
	n := len(buf) - 8
	binary.LittleEndian.PutUint32(buf[0:], uint32(n))
	binary.LittleEndian.PutUint32(buf[4:], crc32.Checksum(buf[8:], castagnoli))
	return buf
}

// check returns the sticky write error, so callers can test it before changing memory.
func (w *wal) check() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// append logs entries with one write call (one frame per walFrameTarget bytes). If the
// write fails, the meta numbers it defined are forgotten again.
func (w *wal) append(entries []walEntry) error {
	if len(entries) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	next := w.nextID
	var added []walMetaKey
	buf := append(w.buf[:0], make([]byte, 8)...)
	buf, frameStart := encodeEntries(buf, 0, entries, w.metaIDs, &next, &added)
	frame(buf[frameStart:])
	w.buf = buf
	if _, err := w.f.Write(buf); err != nil {
		for _, k := range added {
			delete(w.metaIDs, k)
		}
		w.err = err
		return err
	}
	w.nextID = next
	w.size += int64(len(buf))
	if w.buf = w.buf[:0]; cap(w.buf) > 4<<20 {
		w.buf = nil
	}
	w.dirty = true
	if w.interval == 0 {
		return w.syncLocked()
	}
	return nil
}

func (w *wal) syncLocked() error {
	if !w.dirty || w.interval < 0 {
		return nil
	}
	w.dirty = false
	if err := w.f.Sync(); err != nil {
		w.err = err
		return err
	}
	return nil
}

func (w *wal) sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	return w.syncLocked()
}

// rewrite replaces the log with a new one holding only entries. The new meta numbering
// replaces the old one only after the rename succeeded: on a failure before that the old
// log stays in use with its numbering (and later appends stay consistent with it); a
// failure after closing the old log is sticky.
func (w *wal) rewrite(entries []walEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	ids := map[walMetaKey]uint64{}
	next := uint64(1)
	var out []byte
	if len(entries) > 0 {
		var frameStart int
		out, frameStart = encodeEntries(make([]byte, 8, 8+len(entries)*24), 0, entries, ids, &next, nil)
		frame(out[frameStart:])
	}
	sync := w.interval >= 0
	tmp := w.path + ".tmp"
	if err := writeFileSync(tmp, out, sync); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		w.err = err
		os.Remove(tmp)
		return err
	}
	if err := renameRetry(tmp, w.path); err != nil {
		w.err = err
		return err
	}
	if sync {
		if err := syncDir(filepath.Dir(w.path)); err != nil {
			w.err = err
			return err
		}
	}
	f, err := os.OpenFile(w.path, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		w.err = err
		return err
	}
	w.f, w.size, w.dirty = f, int64(len(out)), false
	w.metaIDs, w.nextID = ids, next
	return nil
}

func (w *wal) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.syncLocked()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.f = nil
	return err
}

// colReader reads varints and bytes from a byte slice with a sticky error.
type colReader struct {
	b   []byte
	off int
	err error
}

func (c *colReader) uvarint() uint64 {
	if c.err != nil {
		return 0
	}
	v, n := binary.Uvarint(c.b[c.off:])
	if n <= 0 {
		c.err = errors.New("wal: bad varint")
		return 0
	}
	c.off += n
	return v
}

func (c *colReader) varint() int64 {
	if c.err != nil {
		return 0
	}
	v, n := binary.Varint(c.b[c.off:])
	if n <= 0 {
		c.err = errors.New("wal: bad varint")
		return 0
	}
	c.off += n
	return v
}

func (c *colReader) byte() byte {
	if c.err != nil {
		return 0
	}
	if c.off >= len(c.b) {
		c.err = errors.New("wal: record too short")
		return 0
	}
	v := c.b[c.off]
	c.off++
	return v
}
