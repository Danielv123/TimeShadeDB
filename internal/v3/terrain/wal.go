package terrain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
// OS at Apply and fsynced by the store's syncer about once per SyncInterval (group
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

// replay decodes every segment in order and calls fn for each frame. A torn or corrupt
// frame ends its segment: the segment is truncated there.
func (w *wal) replay(dec *zstd.Decoder, fn func([]walUpdate)) error {
	for i := range w.segs {
		sg := &w.segs[i]
		f, err := os.OpenFile(w.path(sg.seq), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(f)
		if err != nil {
			f.Close()
			return err
		}
		pos := 0
		for pos+walFrameHeader <= len(data) {
			n := int(binary.LittleEndian.Uint32(data[pos:]))
			sum := binary.LittleEndian.Uint32(data[pos+4:])
			if n > len(data)-pos-walFrameHeader {
				break
			}
			payload := data[pos+walFrameHeader : pos+walFrameHeader+n]
			if crcC(payload) != sum {
				break
			}
			ups, err := decodeWALFrame(dec, payload)
			if err != nil {
				break
			}
			for j := range ups {
				if ups[j].t > sg.maxT {
					sg.maxT = ups[j].t
				}
			}
			fn(ups)
			pos += walFrameHeader + n
		}
		if pos < len(data) {
			if err := f.Truncate(int64(pos)); err != nil {
				f.Close()
				return err
			}
		}
		sg.size = int64(pos)
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

// append writes one frame (already encoded by encodeWALFrame) whose newest update is maxT.
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

// encodeWALFrame builds a frame for the updates (raw is scratch space).
func encodeWALFrame(enc *zstd.Encoder, ups []*core.ChunkUpdate, raw []byte) (frame, rawOut []byte) {
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
	frame = make([]byte, walFrameHeader, walFrameHeader+len(raw)/4+64)
	frame = enc.EncodeAll(raw, frame)
	p := frame[walFrameHeader:]
	binary.LittleEndian.PutUint32(frame[0:], uint32(len(p)))
	binary.LittleEndian.PutUint32(frame[4:], crcC(p))
	return frame, raw
}

func decodeWALFrame(dec *zstd.Decoder, payload []byte) ([]walUpdate, error) {
	raw, err := dec.DecodeAll(payload, nil)
	if err != nil {
		return nil, err
	}
	n, k := binary.Uvarint(raw)
	if k <= 0 || n > uint64(len(raw)) {
		return nil, errPayload
	}
	raw = raw[k:]
	out := make([]walUpdate, 0, n)
	for i := uint64(0); i < n; i++ {
		var u walUpdate
		var vals [3]int64
		for j := range vals {
			v, k := binary.Varint(raw)
			if k <= 0 {
				return nil, errPayload
			}
			vals[j] = v
			raw = raw[k:]
		}
		u.t, u.chunk = core.TimeMs(vals[0]), core.ChunkCoord{X: int32(vals[1]), Y: int32(vals[2])}
		if len(raw) < 1 {
			return nil, errPayload
		}
		flags := raw[0]
		raw = raw[1:]
		if flags&1 != 0 {
			u.deleted = true
		} else {
			if len(raw) < 2*core.ChunkPixelCount {
				return nil, errPayload
			}
			for j := range u.px {
				u.px[j] = binary.LittleEndian.Uint16(raw[2*j:])
			}
			raw = raw[2*core.ChunkPixelCount:]
		}
		out = append(out, u)
	}
	return out, nil
}
