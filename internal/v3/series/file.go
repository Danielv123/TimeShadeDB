package series

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"timeshadedb/internal/v3/core"
)

// file is one open series: its two files and its in-memory index. Handles (Series)
// share it; the Store reference-counts it.
type file struct {
	name   string
	path   string // without extension
	noSync bool

	blk, idx *os.File

	// wmu serializes writers (Append, Sync, flush). Readers never take it.
	wmu sync.Mutex
	// mu guards entries and size. Writers hold it only to publish a written record.
	mu      sync.RWMutex
	entries []entry
	size    int64 // end of the last published record in .blk
	written int   // entries already written to .idx
	dirty   bool  // .blk has data that is not fsynced (guarded by wmu)

	// Store bookkeeping, guarded by Store.mu.
	refs  int
	ready chan struct{}
	err   error
	lru   *lruNode
}

// RecoveryReport says what opening a series repaired. Zero means nothing.
type RecoveryReport struct {
	DroppedIndex   int   // index records dropped because they were invalid
	RebuiltIndex   int   // index records rebuilt from .blk
	TruncatedBytes int64 // bytes cut from the end of .blk
}

func (r RecoveryReport) any() bool {
	return r.DroppedIndex != 0 || r.RebuiltIndex != 0 || r.TruncatedBytes != 0
}

// load opens (and with create, creates) the files and runs recovery.
func (f *file) load(create bool) (RecoveryReport, error) {
	var rep RecoveryReport
	blkPath, idxPath := f.path+".blk", f.path+".idx"
	if _, err := os.Stat(blkPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return rep, err
		}
		if !create {
			return rep, fmt.Errorf("%w: series %q", core.ErrNoTile, f.name)
		}
		if err := os.MkdirAll(dirOf(f.path), 0o755); err != nil {
			return rep, err
		}
	}
	blk, err := os.OpenFile(blkPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return rep, err
	}
	idx, err := os.OpenFile(idxPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		blk.Close()
		return rep, err
	}
	f.blk, f.idx = blk, idx
	if err := f.recover(&rep); err != nil {
		blk.Close()
		idx.Close()
		f.blk, f.idx = nil, nil
		return rep, fmt.Errorf("series %q: %w", f.name, err)
	}
	return rep, nil
}

// recover loads the index and repairs torn or corrupt tails (FORMAT.md 5.1 "Recovery").
func (f *file) recover(rep *RecoveryReport) error {
	bst, err := f.blk.Stat()
	if err != nil {
		return err
	}
	blkSize := bst.Size()
	raw, err := io.ReadAll(io.NewSectionReader(f.idx, 0, 1<<62))
	if err != nil {
		return err
	}
	n := len(raw) / IndexRecSize
	entries := make([]entry, n, n+16)
	for i := range entries {
		entries[i] = parseIndex(raw[i*IndexRecSize:])
	}
	// The index must describe contiguous records with non-decreasing effective times.
	// Anything after the first violation is treated as a corrupt tail.
	for i := range entries {
		e := &entries[i]
		var prevEnd int64
		var prevT core.TimeMs
		if i > 0 {
			prevEnd, prevT = entries[i-1].end(), entries[i-1].tEnd()
		}
		if e.off != prevEnd || !validKind(e.kind) || e.tEnd() < prevT {
			n = i
			break
		}
	}
	// Drop index records from the end until the last one points at a valid record.
	for n > 0 {
		e := &entries[n-1]
		if e.end() <= blkSize && f.checkRecord(e) == nil {
			break
		}
		n--
	}
	rep.DroppedIndex = len(raw)/IndexRecSize - n
	entries = entries[:n]
	if n*IndexRecSize != len(raw) {
		if err := f.idx.Truncate(int64(n * IndexRecSize)); err != nil {
			return err
		}
	}
	// Scan .blk after the last indexed record for complete, valid records.
	pos := int64(0)
	if n > 0 {
		pos = entries[n-1].end()
	}
	var hb [HeaderSize]byte
	for pos+HeaderSize <= blkSize {
		if _, err := f.blk.ReadAt(hb[:], pos); err != nil {
			break
		}
		h, err := parseHeader(hb[:])
		if err != nil || pos+HeaderSize+int64(h.plen) > blkSize {
			break
		}
		e := entry{tStart: h.tStart, tLen: h.tLen, off: pos, plen: h.plen, kind: h.kind, flags: h.flags}
		if len(entries) > 0 && e.tEnd() < entries[len(entries)-1].tEnd() {
			break
		}
		if f.checkRecord(&e) != nil {
			break
		}
		entries = append(entries, e)
		rep.RebuiltIndex++
		pos = e.end()
	}
	if pos < blkSize {
		if err := f.blk.Truncate(pos); err != nil {
			return err
		}
		rep.TruncatedBytes = blkSize - pos
	}
	var deltas uint32
	for i := range entries {
		if entries[i].kind == core.RecordDelta {
			deltas++
		}
		entries[i].deltas = deltas
	}
	if cap(entries) > len(entries)+len(entries)/4+16 {
		entries = append(make([]entry, 0, len(entries)+16), entries...)
	}
	f.entries = entries
	f.size = pos
	f.written = n
	if rep.RebuiltIndex > 0 || rep.TruncatedBytes > 0 {
		// Rebuilt index records describe records that were already on disk; write them
		// now so the next open does not scan again.
		if err := f.blk.Sync(); err != nil && !f.noSync {
			return err
		}
		if err := f.writeIndex(); err != nil {
			return err
		}
	}
	return nil
}

// checkRecord reads the record e points at and verifies both checksums.
func (f *file) checkRecord(e *entry) error {
	buf := make([]byte, HeaderSize+int(e.plen))
	if _, err := f.blk.ReadAt(buf, e.off); err != nil {
		return err
	}
	h, err := parseHeader(buf)
	if err != nil {
		return err
	}
	if !e.matches(h) {
		return fmt.Errorf("%w: index does not match header", ErrCorrupt)
	}
	if crc(buf[HeaderSize:]) != h.pcrc {
		return fmt.Errorf("%w: payload checksum", ErrCorrupt)
	}
	return nil
}

// writeIndex writes the index records not yet in .idx. Caller holds wmu (or owns f).
func (f *file) writeIndex() error {
	f.mu.RLock()
	pending := f.entries[f.written:len(f.entries):len(f.entries)]
	start := f.written
	f.mu.RUnlock()
	if len(pending) == 0 {
		return nil
	}
	buf := make([]byte, len(pending)*IndexRecSize)
	for i := range pending {
		putIndex(buf[i*IndexRecSize:], &pending[i])
	}
	if _, err := f.idx.WriteAt(buf, int64(start*IndexRecSize)); err != nil {
		return err
	}
	f.written = start + len(pending)
	return nil
}

// append writes one record and publishes it.
func (f *file) append(rec core.Record) (uint64, error) {
	if !validKind(rec.Kind) {
		return 0, fmt.Errorf("series: unknown record kind %d", rec.Kind)
	}
	if len(rec.Payload) > MaxPayload {
		return 0, fmt.Errorf("series: payload of %d bytes is too large", len(rec.Payload))
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if f.blk == nil {
		return 0, core.ErrClosed
	}
	e := entry{tStart: rec.TStart, tLen: rec.TLen, plen: uint32(len(rec.Payload)), kind: rec.Kind, flags: rec.Flags}
	// Only writers change entries, and we hold wmu, so reading without mu is safe.
	n := len(f.entries)
	if n > 0 && e.tEnd() < f.entries[n-1].tEnd() {
		return 0, fmt.Errorf("%w: TEnd %d before %d", core.ErrOutOfOrder, e.tEnd(), f.entries[n-1].tEnd())
	}
	e.off = f.size
	e.deltas = 0
	if n > 0 {
		e.deltas = f.entries[n-1].deltas
	}
	if e.kind == core.RecordDelta {
		e.deltas++
	}
	buf := make([]byte, HeaderSize+len(rec.Payload))
	copy(buf[HeaderSize:], rec.Payload)
	putHeader(buf, header{kind: e.kind, flags: e.flags, tStart: e.tStart, tLen: e.tLen, plen: e.plen, pcrc: crc(rec.Payload)})
	if _, err := f.blk.WriteAt(buf, e.off); err != nil {
		// Cut a partial write so the next append starts at a record boundary.
		_ = f.blk.Truncate(e.off)
		return 0, err
	}
	f.mu.Lock()
	f.entries = append(f.entries, e)
	f.size = e.end()
	f.mu.Unlock()
	f.dirty = true
	return uint64(n), nil
}

// sync makes every appended record durable, then writes their index records.
func (f *file) sync() error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	return f.syncLocked()
}

func (f *file) syncLocked() error {
	if f.blk == nil {
		return nil
	}
	if f.dirty {
		if !f.noSync {
			if err := f.blk.Sync(); err != nil {
				return err
			}
		}
		f.dirty = false
	}
	return f.writeIndex()
}

// close flushes and closes the files.
func (f *file) close() error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if f.blk == nil {
		return nil
	}
	err := f.syncLocked()
	if e := f.blk.Close(); err == nil {
		err = e
	}
	if e := f.idx.Close(); err == nil {
		err = e
	}
	f.blk, f.idx = nil, nil
	f.mu.Lock()
	f.entries = nil
	f.mu.Unlock()
	return err
}

// readRange reads records first..last (inclusive) with one contiguous read.
func (f *file) readRange(first, last uint64) ([]core.Record, error) {
	f.mu.RLock()
	n := uint64(len(f.entries))
	if first > last || last >= n {
		f.mu.RUnlock()
		return nil, fmt.Errorf("%w: records %d-%d of %d", core.ErrNoVersion, first, last, n)
	}
	ents := make([]entry, last-first+1)
	copy(ents, f.entries[first:last+1])
	blk := f.blk
	f.mu.RUnlock()
	if blk == nil {
		return nil, core.ErrClosed
	}
	base := ents[0].off
	buf := make([]byte, ents[len(ents)-1].end()-base)
	if _, err := blk.ReadAt(buf, base); err != nil {
		return nil, fmt.Errorf("series %q: read: %w", f.name, err)
	}
	out := make([]core.Record, len(ents))
	for i := range ents {
		e := &ents[i]
		o := e.off - base
		h, err := parseHeader(buf[o:])
		if err != nil || !e.matches(h) {
			return nil, fmt.Errorf("series %q record %d: %w", f.name, first+uint64(i), ErrCorrupt)
		}
		p := buf[o+HeaderSize : o+HeaderSize+int64(e.plen) : o+HeaderSize+int64(e.plen)]
		if crc(p) != h.pcrc {
			return nil, fmt.Errorf("series %q record %d: %w: payload checksum", f.name, first+uint64(i), ErrCorrupt)
		}
		out[i] = core.Record{RecordMeta: e.meta(first + uint64(i)), Payload: p}
	}
	return out, nil
}

// endAt returns the number of records with TEnd <= t.
func endAt(entries []entry, t core.TimeMs) int {
	return sort.Search(len(entries), func(i int) bool { return entries[i].tEnd() > t })
}

// baseAtOrBefore returns the newest base record at or before seq, or 0.
func baseAtOrBefore(entries []entry, seq int) int {
	for i := seq; i >= 0; i-- {
		if entries[i].kind.IsBase() {
			return i
		}
	}
	return 0
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
