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
//
// Meta numbers are local to the file. At every commit that closes a window the log is
// rewritten (tmp + rename) with only the rows still open, so its size is bounded by the
// open windows. Replay stops at the first torn or corrupt frame and truncates there.

const (
	walRecMeta   = 'M'
	walRecSample = 'S'
	walMaxFrame  = 64 << 20
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
			break
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
			metas[id] = m
			mm := m.M
			ptrs[id] = &mm
		case walRecSample:
			id := r.uvarint()
			m, ok := metas[id]
			if !ok {
				return fmt.Errorf("wal: sample references unknown meta %d", id)
			}
			x := row{meta: ptrs[id]}
			x.t = core.TimeMs(r.varint())
			x.tick = r.varint()
			x.x = int32(r.varint())
			x.y = int32(r.varint())
			x.o = uint16(r.uvarint())
			x.flags = core.MoverFlags(r.byte()) & storedFlags
			nc := int(r.uvarint())
			if nc > 10000 {
				return fmt.Errorf("wal: %d carriages", nc)
			}
			if nc > 0 {
				x.car = make([]carriage, nc)
				for i := range x.car {
					x.car[i] = carriage{x: int32(r.varint()), y: int32(r.varint()), o: uint16(r.uvarint())}
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
	for _, p := range rows {
		fn(p.sfc, p.r)
	}
	return nil
}

// encode appends the records for entries to w.buf, defining metas as needed.
func (w *wal) encode(entries []walEntry) {
	for _, e := range entries {
		k := walMetaKey{e.sfc, *e.r.meta}
		id, ok := w.metaIDs[k]
		if !ok {
			id = w.nextID
			w.nextID++
			w.metaIDs[k] = id
			js, _ := json.Marshal(walMetaJSON{S: e.sfc, M: *e.r.meta})
			w.buf = append(w.buf, walRecMeta)
			w.buf = binary.AppendUvarint(w.buf, id)
			w.buf = binary.AppendUvarint(w.buf, uint64(len(js)))
			w.buf = append(w.buf, js...)
		}
		r := e.r
		w.buf = append(w.buf, walRecSample)
		w.buf = binary.AppendUvarint(w.buf, id)
		w.buf = binary.AppendVarint(w.buf, int64(r.t))
		w.buf = binary.AppendVarint(w.buf, r.tick)
		w.buf = binary.AppendVarint(w.buf, int64(r.x))
		w.buf = binary.AppendVarint(w.buf, int64(r.y))
		w.buf = binary.AppendUvarint(w.buf, uint64(r.o))
		w.buf = append(w.buf, byte(r.flags))
		w.buf = binary.AppendUvarint(w.buf, uint64(len(r.car)))
		for _, c := range r.car {
			w.buf = binary.AppendVarint(w.buf, int64(c.x))
			w.buf = binary.AppendVarint(w.buf, int64(c.y))
			w.buf = binary.AppendUvarint(w.buf, uint64(c.o))
		}
	}
}

// frame fills in the 8-byte frame header at the start of buf and returns buf.
func frame(buf []byte) []byte {
	n := len(buf) - 8
	binary.LittleEndian.PutUint32(buf[0:], uint32(n))
	binary.LittleEndian.PutUint32(buf[4:], crc32.Checksum(buf[8:], castagnoli))
	return buf
}

// append logs entries as one frame.
func (w *wal) append(entries []walEntry) error {
	if len(entries) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.buf = append(w.buf[:0], make([]byte, 8)...)
	w.encode(entries)
	if len(w.buf)-8 > walMaxFrame {
		return fmt.Errorf("wal: batch of %d bytes exceeds the frame limit", len(w.buf)-8)
	}
	if _, err := w.f.Write(frame(w.buf)); err != nil {
		w.err = err
		return err
	}
	w.size += int64(len(w.buf))
	if w.buf = w.buf[:0]; cap(w.buf) > 1<<20 {
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

// rewrite replaces the log with a new one holding only entries.
func (w *wal) rewrite(entries []walEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.metaIDs = map[walMetaKey]uint64{}
	w.nextID = 1
	w.buf = append(w.buf[:0], make([]byte, 8)...)
	var out []byte
	// Split into frames of bounded size.
	for i := 0; i < len(entries); i += 4096 {
		j := min(i+4096, len(entries))
		w.encode(entries[i:j])
		out = append(out, frame(w.buf)...)
		w.buf = append(w.buf[:0], make([]byte, 8)...)
	}
	w.buf = w.buf[:0]
	tmp := w.path + ".tmp"
	if err := writeFileSync(tmp, out, w.interval >= 0); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		w.err = err
		return err
	}
	if err := renameRetry(tmp, w.path); err != nil {
		w.err = err
		return err
	}
	f, err := os.OpenFile(w.path, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		w.err = err
		return err
	}
	w.f, w.size, w.dirty = f, int64(len(out)), false
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
