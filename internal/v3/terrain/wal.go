package terrain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
)

// The write-ahead log keeps the updates of open windows (FORMAT.md 5 "wal/"). It is a
// directory of numbered segments. A frame is
//
//	u32 payload length | u32 CRC-32C of the payload | payload (one zstd frame)
//
// and the decompressed payload is uvarint n, then n updates of
//
//	varint T | varint cx | varint cy | u8 flags (bit 0 deleted) | 2048 bytes RGB565 LE (unless deleted)
//
// A new segment starts at every commit that moves the horizon; a segment is deleted
// once every update in it is older than the horizon (its windows are committed). So the
// log is bounded by the window length times the ingest rate. Frames are written to the
// OS at Apply (a batch is split into frames of at most walFrameUpdates updates, written
// with one write) and fsynced by the store's syncer about once per SyncInterval (group
// commit).
type wal struct {
	dir    string
	noSync bool

	mu      sync.Mutex
	cur     *os.File
	curSeq  uint64
	curSize int64
	curMaxT core.TimeMs
	dirty   bool
	segs    []walSeg // closed segments, oldest first
}

type walSeg struct {
	seq  uint64
	maxT core.TimeMs
	size int64
}

const walFrameHeader = 8

func segName(seq uint64) string { return fmt.Sprintf("%016d.wal", seq) }

func (w *wal) path(seq uint64) string { return filepath.Join(w.dir, segName(seq)) }

// openWAL lists the existing segments. Call replay, then start.
func openWAL(dir string, noSync bool) (*wal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &wal{dir: dir, noSync: noSync}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".wal") {
			continue
		}
		seq, err := strconv.ParseUint(strings.TrimSuffix(name, ".wal"), 10, 64)
		if err != nil {
			continue
		}
		w.segs = append(w.segs, walSeg{seq: seq, maxT: noHorizon})
	}
	sort.Slice(w.segs, func(i, j int) bool { return w.segs[i].seq < w.segs[j].seq })
	return w, nil
}

// walUpdate is one decoded WAL update.
type walUpdate struct {
	t       core.TimeMs
	chunk   core.ChunkCoord
	deleted bool
	px      core.ChunkPixels
}

// replay decodes every segment in order and calls fn for each frame (the slice is
// reused after fn returns). Frames are read one at a time into reused buffers. A torn
// tail (an incomplete frame, a zero or oversized length, a checksum mismatch) ends its
// segment, which is truncated there and logged. A frame whose checksum is valid but
// that does not decode is not a torn write: replay returns an error and changes
// nothing, rather than dropping acknowledged updates.
func (w *wal) replay(dec *zstd.Decoder, logf func(string, ...any), fn func([]walUpdate)) error {
	var payload, raw []byte
	var ups []walUpdate
	for i := range w.segs {
		sg := &w.segs[i]
		path := w.path(sg.seq)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		size := st.Size()
		var pos int64
		var hdr [walFrameHeader]byte
		reason := ""
		for pos < size {
			if pos+walFrameHeader > size {
				reason = "incomplete frame header"
				break
			}
			if _, err := f.ReadAt(hdr[:], pos); err != nil {
				f.Close()
				return fmt.Errorf("%s: %w", path, err)
			}
			n := int64(binary.LittleEndian.Uint32(hdr[0:]))
			sum := binary.LittleEndian.Uint32(hdr[4:])
			if n == 0 || n > walMaxPayload {
				reason = fmt.Sprintf("bad frame length %d", n)
				break
			}
			if pos+walFrameHeader+n > size {
				reason = "incomplete frame"
				break
			}
			if int64(cap(payload)) < n {
				payload = make([]byte, n)
			}
			payload = payload[:n]
			if _, err := f.ReadAt(payload, pos+walFrameHeader); err != nil {
				f.Close()
				return fmt.Errorf("%s: %w", path, err)
			}
			if crcC(payload) != sum {
				reason = "frame checksum mismatch"
				break
			}
			ups, raw, err = decodeWALFrame(dec, payload, raw, ups)
			if err != nil {
				f.Close()
				return fmt.Errorf("%s: the frame at offset %d has a valid checksum but does not decode: %w (move the segment aside to open without it)", path, pos, err)
			}
			for j := range ups {
				if ups[j].t > sg.maxT {
					sg.maxT = ups[j].t
				}
			}
			fn(ups)
			pos += walFrameHeader + n
		}
		if pos < size {
			logf("terrain: wal %s: %s at offset %d, truncating %d bytes", path, reason, pos, size-pos)
			if err := f.Truncate(pos); err != nil {
				f.Close()
				return err
			}
		}
		sg.size = pos
		f.Close()
	}
	return nil
}

// start opens a fresh segment for appends.
func (w *wal) start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	next := uint64(1)
	if n := len(w.segs); n > 0 {
		next = w.segs[n-1].seq + 1
	}
	return w.openSegLocked(next)
}

func (w *wal) openSegLocked(seq uint64) error {
	f, err := os.OpenFile(w.path(seq), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w.cur, w.curSeq, w.curSize, w.curMaxT, w.dirty = f, seq, 0, noHorizon, false
	return nil
}

// append writes frames (encoded by appendWALFrame) whose newest update is maxT, with
// one write. On failure the segment is cut back, so a batch is logged whole or not at
// all.
func (w *wal) append(frame []byte, maxT core.TimeMs) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return core.ErrClosed
	}
	if _, err := w.cur.WriteAt(frame, w.curSize); err != nil {
		_ = w.cur.Truncate(w.curSize)
		return err
	}
	w.curSize += int64(len(frame))
	if maxT > w.curMaxT {
		w.curMaxT = maxT
	}
	w.dirty = true
	return nil
}

// sync fsyncs the current segment if it has unsynced frames.
func (w *wal) sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncLocked()
}

func (w *wal) syncLocked() error {
	if w.cur == nil || !w.dirty {
		return nil
	}
	w.dirty = false
	if w.noSync {
		return nil
	}
	return w.cur.Sync()
}

// rotate closes the current segment (if it has frames) and opens the next one, then
// deletes closed segments whose updates are all older than horizon.
func (w *wal) rotate(horizon core.TimeMs) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return core.ErrClosed
	}
	if w.curSize > 0 {
		if err := w.syncLocked(); err != nil {
			return err
		}
		if err := w.cur.Close(); err != nil {
			return err
		}
		w.segs = append(w.segs, walSeg{seq: w.curSeq, maxT: w.curMaxT, size: w.curSize})
		if err := w.openSegLocked(w.curSeq + 1); err != nil {
			w.cur = nil
			return err
		}
	}
	keep := w.segs[:0]
	var firstErr error
	for _, sg := range w.segs {
		if sg.maxT < horizon {
			err := os.Remove(w.path(sg.seq))
			if err == nil || errors.Is(err, os.ErrNotExist) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		keep = append(keep, sg)
	}
	w.segs = keep
	return firstErr
}

// size returns the bytes on disk.
func (w *wal) size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.curSize
	for _, sg := range w.segs {
		n += sg.size
	}
	return n
}

func (w *wal) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return nil
	}
	err := w.syncLocked()
	if e := w.cur.Close(); err == nil {
		err = e
	}
	if w.curSize == 0 {
		os.Remove(w.path(w.curSeq))
	}
	w.cur = nil
	return err
}

// WAL frame limits. Apply splits a batch into frames of at most walFrameUpdates
// updates, so a decoded frame is at most walMaxRaw bytes, well under the decoder's
// memory limit (decoderMaxMemory), and a stored payload is at most walMaxPayload.
const (
	walFrameUpdates = 1024
	walMaxRaw       = binary.MaxVarintLen64 + walFrameUpdates*(3*binary.MaxVarintLen64+1+2*core.ChunkPixelCount)
	walMaxPayload   = 2 * walMaxRaw
)

// appendWALFrame appends a frame for the updates (at most walFrameUpdates) to dst. raw
// is scratch space.
func appendWALFrame(dst []byte, enc *zstd.Encoder, ups []*core.ChunkUpdate, raw []byte) (out, rawOut []byte) {
	raw = binary.AppendUvarint(raw[:0], uint64(len(ups)))
	for _, u := range ups {
		raw = binary.AppendVarint(raw, int64(u.T))
		raw = binary.AppendVarint(raw, int64(u.Chunk.X))
		raw = binary.AppendVarint(raw, int64(u.Chunk.Y))
		if u.Deleted {
			raw = append(raw, 1)
			continue
		}
		raw = append(raw, 0)
		for _, c := range u.Pixels {
			raw = binary.LittleEndian.AppendUint16(raw, c)
		}
	}
	start := len(dst)
	dst = append(dst, make([]byte, walFrameHeader)...)
	dst = enc.EncodeAll(raw, dst)
	p := dst[start+walFrameHeader:]
	binary.LittleEndian.PutUint32(dst[start:], uint32(len(p)))
	binary.LittleEndian.PutUint32(dst[start+4:], crcC(p))
	return dst, raw
}

// decodeWALFrame decodes one payload. raw and out are reused buffers.
func decodeWALFrame(dec *zstd.Decoder, payload, raw []byte, out []walUpdate) ([]walUpdate, []byte, error) {
	raw, err := dec.DecodeAll(payload, raw[:0])
	if err != nil {
		return out[:0], raw, err
	}
	buf := raw
	n, k := binary.Uvarint(buf)
	if k <= 0 || n > uint64(len(buf)) || n > walFrameUpdates {
		return out[:0], raw, errPayload
	}
	buf = buf[k:]
	out = out[:0]
	for i := uint64(0); i < n; i++ {
		var vals [3]int64
		for j := range vals {
			v, k := binary.Varint(buf)
			if k <= 0 {
				return out[:0], raw, errPayload
			}
			vals[j] = v
			buf = buf[k:]
		}
		if len(buf) < 1 {
			return out[:0], raw, errPayload
		}
		out = append(out, walUpdate{})
		u := &out[len(out)-1]
		u.t, u.chunk = core.TimeMs(vals[0]), core.ChunkCoord{X: int32(vals[1]), Y: int32(vals[2])}
		flags := buf[0]
		buf = buf[1:]
		if flags&1 != 0 {
			u.deleted = true
		} else {
			if len(buf) < 2*core.ChunkPixelCount {
				return out[:0], raw, errPayload
			}
			for j := range u.px {
				u.px[j] = binary.LittleEndian.Uint16(buf[2*j:])
			}
			buf = buf[2*core.ChunkPixelCount:]
		}
	}
	return out, raw, nil
}
