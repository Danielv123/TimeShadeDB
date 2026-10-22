package movers

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"timeshadedb/internal/v3/core"
)

// Open windows (docs/v3/FORMAT.md 5.5). Every open 5-minute window of a surface has an
// append-only log, movers/<surface id>/open/<window start>.wal, that holds every row
// accepted into the window: CRC-framed batches in the record format of wal.go, with meta
// numbers local to the file. Append writes a batch to the logs of the windows it touches
// before it changes memory. While the rows of all open windows fit Options.OpenBytes they
// are also kept in memory; beyond it whole windows are evicted, oldest start first, and
// only a small index stays per evicted window (each journey's newest row and the earliest
// time of its samples). Rows accepted into an evicted window go to its log only. A Commit
// reads an evicted window back from its log, one window at a time, so neither a burst nor
// a horizon held back for a source that is catching up makes Append refuse rows: memory
// stays bounded and the backlog waits on disk. A window's log is deleted once meta.json
// records a horizon past the window; Open deletes logs of windows below the horizon and
// replays the others, keeping the newest windows resident up to the budget.

// openWin is one open window of a surface.
type openWin struct {
	start core.TimeMs
	// rows are the resident rows in arrival order, possibly with repeated (journey, t);
	// nil once the window was evicted.
	rows    []row
	spilled bool
	n       int   // rows accepted into the window, resident or not
	bytes   int64 // accounted memory of rows
	// tmin and tmax are the time range of the rows (when n > 0).
	tmin, tmax core.TimeMs
	// last and first index an evicted window: each journey's newest row (by t, then
	// arrival) and the earliest t of its rows that are not gones (the handoff check of the
	// window before). ibytes is their accounted memory.
	last   map[string]*row
	first  map[string]core.TimeMs
	ibytes int64
	log    *winLog // nil without a WAL
}

// rowOverhead is the accounted memory of a row without carriages or rail pieces: the
// struct and the share of its slot in a growing slice. Measured (heap per row, 200,000
// rows): 144 B plain, 164 B with rail pieces, 275 B with 10 carriages.
const rowOverhead = int64(unsafe.Sizeof(row{})) + 32

// rowCost is the accounted memory of r.
func rowCost(r *row) int64 {
	return rowOverhead + 12*int64(len(r.car)) + 12*int64(len(r.rail))
}

// indexEntryCost is the accounted memory of one journey in an evicted window's index
// besides its row copy (rowCost): the map entries.
const indexEntryCost = 64 + 48

// winLog is the append-only log of one open window.
type winLog struct {
	path  string
	f     *os.File // nil while closed (an evicted window); reopened by the next write
	size  int64
	ids   map[walMetaKey]uint64
	next  uint64
	dirty bool
}

func (s *surface) openDir() string { return filepath.Join(s.dir, "open") }

func (s *surface) logPath(start core.TimeMs) string {
	return filepath.Join(s.openDir(), timeName(start)+".wal")
}

// win returns the open window starting at ws, creating it (and its log) when missing.
func (m *Store) win(s *surface, ws core.TimeMs) (*openWin, error) {
	if w := s.open[ws]; w != nil {
		return w, nil
	}
	w := &openWin{start: ws}
	if !m.opts.NoWAL {
		if err := mkdirAll(s.openDir(), m.opts.SyncInterval >= 0); err != nil {
			return nil, err
		}
		w.log = &winLog{path: s.logPath(ws), ids: map[walMetaKey]uint64{}, next: 1}
	}
	s.open[ws] = w
	return w, nil
}

// logBatch is the rows of one batch for one window log.
type logBatch struct {
	log     *winLog
	entries []walEntry
}

// writeLogs writes each batch to its log with one write call. On a failure every log
// written so far is cut back and the meta numbers defined are forgotten, so the batch is
// logged nowhere; a log that cannot be cut back makes the error sticky.
func (m *Store) writeLogs(batches []logBatch) error {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	if m.walErr != nil {
		return m.walErr
	}
	type undo struct {
		l     *winLog
		size  int64
		added []walMetaKey
		next  uint64
	}
	var done []undo
	fail := func(err error) error {
		for _, u := range done {
			for _, k := range u.added {
				delete(u.l.ids, k)
			}
			u.l.next = u.next
			if u.l.f != nil {
				if terr := u.l.f.Truncate(u.size); terr != nil {
					m.walErr = fmt.Errorf("movers: wal %s: %w (and cutting it back: %v)", u.l.path, err, terr)
				}
			}
			u.l.size = u.size
		}
		return err
	}
	for _, b := range batches {
		l := b.log
		if err := m.openLog(l); err != nil {
			return fail(err)
		}
		u := undo{l: l, size: l.size, next: l.next}
		buf := append(m.buf[:0], make([]byte, 8)...)
		buf, frameStart := encodeEntries(buf, 0, b.entries, l.ids, &l.next, &u.added)
		frame(buf[frameStart:])
		m.buf = buf
		done = append(done, u)
		if _, err := l.f.WriteAt(buf, l.size); err != nil {
			return fail(err)
		}
		l.size += int64(len(buf))
		if cap(m.buf) > 4<<20 {
			m.buf = nil
		}
	}
	for _, u := range done {
		u.l.dirty = true
		m.dirty[u.l] = struct{}{}
	}
	if m.opts.SyncInterval == 0 {
		return m.syncLogsLocked()
	}
	return nil
}

// openLog opens l for writing if it is closed. Callers hold m.lmu.
func (m *Store) openLog(l *winLog) error {
	if l.f != nil {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if st.Size() != l.size {
		// A log is reopened with the size it was closed with. A new window finding a file
		// finds one left by a window that was dropped but could not be deleted.
		m.opts.Logf("movers: wal %s has %d bytes, expected %d; cutting it", l.path, st.Size(), l.size)
		if err := f.Truncate(l.size); err != nil {
			f.Close()
			return err
		}
	}
	if l.size == 0 && m.opts.SyncInterval >= 0 {
		if err := syncDir(filepath.Dir(l.path)); err != nil {
			f.Close()
			return err
		}
	}
	l.f = f
	return nil
}

// syncLogsLocked fsyncs every log written since the last sync. Callers hold m.lmu.
func (m *Store) syncLogsLocked() error {
	if m.opts.SyncInterval < 0 {
		clear(m.dirty)
		return nil
	}
	var first error
	for l := range m.dirty {
		if l.f != nil && l.dirty {
			if err := l.f.Sync(); err != nil {
				if first == nil {
					first = err
				}
				m.walErr = fmt.Errorf("movers: wal %s: %w", l.path, err)
				continue
			}
		}
		l.dirty = false
		delete(m.dirty, l)
	}
	return first
}

func (m *Store) syncLogs() error {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	return m.syncLogsLocked()
}

// closeLog fsyncs (if it has unsynced writes) and closes l's file; it stays usable and is
// reopened by the next write. Callers hold m.lmu.
func (m *Store) closeLog(l *winLog) error {
	if l == nil || l.f == nil {
		return nil
	}
	var err error
	if l.dirty && m.opts.SyncInterval >= 0 {
		err = l.f.Sync()
	}
	l.dirty = false
	delete(m.dirty, l)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// removeLog closes and deletes l. Callers hold m.lmu.
func (m *Store) removeLog(l *winLog) error {
	if l == nil {
		return nil
	}
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	delete(m.dirty, l)
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// addRow puts r into w (resident or only indexed) and accounts for it. r has its seq,
// interned meta and body.
func (m *Store) addRow(w *openWin, r row) {
	if w.n == 0 || r.t < w.tmin {
		w.tmin = r.t
	}
	if w.n == 0 || r.t > w.tmax {
		w.tmax = r.t
	}
	w.n++
	m.openRows++
	if !w.spilled {
		c := rowCost(&r)
		w.rows = append(w.rows, r)
		w.bytes += c
		m.resident += c
		return
	}
	w.index(&r, m)
}

// index updates an evicted window's index with r.
func (w *openWin) index(r *row, m *Store) {
	id := r.meta.ID
	cur, ok := w.last[id]
	if !ok || r.t > cur.t || (r.t == cur.t && r.seq >= cur.seq) {
		d := rowCost(r)
		if ok {
			d -= rowCost(cur)
		} else {
			d += indexEntryCost
		}
		w.ibytes += d
		m.resident += d
		c := *r
		w.last[id] = &c
	}
	if !r.gone() {
		if f, ok := w.first[id]; !ok || r.t < f {
			w.first[id] = r.t
		}
	}
}

// evict drops the resident rows of whole windows, oldest start first, until the resident
// memory is within three quarters of Options.OpenBytes (or nothing more can go). Windows
// that end within LiveTrail of their surface's newest row, which the live payload reads,
// go last; windows without a log stay resident. Callers hold m.mu for writing.
func (m *Store) evict() {
	if m.opts.OpenBytes <= 0 || m.resident <= m.opts.OpenBytes {
		return
	}
	type cand struct {
		s      *surface
		w      *openWin
		recent bool
	}
	var cs []cand
	for _, s := range m.sfc {
		for _, w := range s.open {
			if !w.spilled && w.log != nil && len(w.rows) > 0 {
				cs = append(cs, cand{s, w, w.start+core.MoverWindowLen > s.last-core.LiveTrail})
			}
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.recent != b.recent {
			return !a.recent
		}
		if a.w.start != b.w.start {
			return a.w.start < b.w.start
		}
		return a.s.id < b.s.id
	})
	target := m.opts.OpenBytes * 3 / 4
	m.lmu.Lock()
	defer m.lmu.Unlock()
	for _, c := range cs {
		if m.resident <= target {
			break
		}
		w := c.w
		w.last, w.first = map[string]*row{}, map[string]core.TimeMs{}
		w.spilled = true
		rows := w.rows
		w.rows = nil
		m.resident -= w.bytes
		w.bytes = 0
		for i := range rows {
			w.index(&rows[i], m)
		}
		m.stats.Evictions++
		if err := m.closeLog(w.log); err != nil {
			m.walErr = fmt.Errorf("movers: wal %s: %w", w.log.path, err)
		}
	}
}

// windowRows returns the rows of w for a Commit: its resident rows, or for an evicted
// window the rows read back from its log, in arrival order, with interned metas and fresh
// seqs. Callers hold m.mu for writing.
func (m *Store) windowRows(s *surface, w *openWin) ([]row, error) {
	if !w.spilled {
		return w.rows, nil
	}
	rows, err := m.readWinRows(s, w)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].meta = s.intern(rows[i].meta)
		m.seq++
		rows[i].seq = m.seq
	}
	return rows, nil
}

// readWinRows reads an evicted window's rows back from its log, in arrival order (seq is
// the position). It changes nothing but the log's file handle, so readers holding m.mu
// for reading may call it.
func (m *Store) readWinRows(s *surface, w *openWin) ([]row, error) {
	m.lmu.Lock()
	err := m.closeLog(w.log) // flushed and closed, so the read sees every write
	size := w.log.size
	m.lmu.Unlock()
	if err != nil {
		return nil, err
	}
	rows := make([]row, 0, w.n)
	_, _, skipped, err := readLog(w.log.path, size, func(sfc core.SurfaceKey, r row) {
		if sfc != s.key || core.WindowStart(r.t, core.MoverWindowLen) != w.start {
			return
		}
		r.seq = uint64(len(rows) + 1)
		rows = append(rows, r)
	})
	if err != nil {
		return nil, fmt.Errorf("reading evicted window %d: %w", w.start, err)
	}
	if skipped > 0 {
		m.opts.Logf("movers: wal %s: skipped %d frames that did not decode", w.log.path, skipped)
	}
	return rows, nil
}

// readLog reads the frames of a log up to limit bytes (< 0: the whole file) and calls fn
// for every row of every valid frame. It returns the size of the valid prefix (a torn
// tail ends it), the meta numbers defined, the frames skipped because they did not decode
// (their CRC was valid), and I/O errors.
func readLog(path string, limit int64, fn func(core.SurfaceKey, row)) (int64, map[uint64]walMetaJSON, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, 0, err
	}
	defer f.Close()
	var r io.Reader = f
	if limit >= 0 {
		r = io.LimitReader(f, limit)
	}
	br := bufio.NewReaderSize(r, 256<<10)
	metas := map[uint64]walMetaJSON{}
	ptrs := map[uint64]*core.MoverMeta{}
	var off int64
	skipped := 0
	var hdr [8]byte
	var payload []byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			break // end, or a torn header
		}
		n := int(binary.LittleEndian.Uint32(hdr[0:]))
		crc := binary.LittleEndian.Uint32(hdr[4:])
		if n > walMaxFrame {
			break
		}
		if cap(payload) < n {
			payload = make([]byte, n)
		}
		payload = payload[:n]
		if _, err := io.ReadFull(br, payload); err != nil {
			break
		}
		if crc32.Checksum(payload, castagnoli) != crc {
			break
		}
		if err := decodeWALFrame(payload, metas, ptrs, fn); err != nil {
			skipped++
		}
		off += 8 + int64(n)
	}
	return off, metas, skipped, nil
}

// replayLogs replays the open-window logs of every surface directory under m.dir, creating
// surfaces that have no meta.json yet. Logs of windows below a surface's horizon were
// committed before a crash and are deleted. Windows load oldest first and evict as the
// budget fills, so the newest stay resident.
func (m *Store) replayLogs() error {
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	type logFile struct {
		key   core.SurfaceKey
		start core.TimeMs
		path  string
	}
	var files []logFile
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		key, err := core.ParseSurfaceID(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(m.dir, e.Name(), "open")
		les, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, le := range les {
			name, ok := strings.CutSuffix(le.Name(), ".wal")
			if !ok || le.IsDir() {
				continue
			}
			start, err := strconv.ParseInt(name, 10, 64)
			if err != nil || timeName(core.TimeMs(start)) != name || core.TimeMs(start)%core.MoverWindowLen != 0 {
				continue
			}
			files = append(files, logFile{key, core.TimeMs(start), filepath.Join(dir, le.Name())})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].start != files[j].start {
			return files[i].start < files[j].start
		}
		return files[i].key.ID() < files[j].key.ID()
	})
	redo := map[*surface]map[string]bool{}
	for _, lf := range files {
		s := m.sfc[lf.key]
		if s == nil {
			s = m.newSurface(lf.key)
			m.sfc[lf.key] = s
		}
		if s.hasHorizon && lf.start+core.MoverWindowLen <= s.horizon {
			// committed before a crash that came before the log was deleted
			if err := os.Remove(lf.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		w := &openWin{start: lf.start, log: &winLog{path: lf.path, ids: map[walMetaKey]uint64{}, next: 1}}
		s.open[lf.start] = w
		size, metas, skipped, err := readLog(lf.path, -1, func(sfc core.SurfaceKey, r row) {
			if sfc != s.key || core.WindowStart(r.t, core.MoverWindowLen) != lf.start {
				return
			}
			m.replayRow(s, w, r, redo)
		})
		if err != nil {
			return err
		}
		if st, err := os.Stat(lf.path); err == nil && st.Size() > size {
			m.opts.Logf("movers: wal %s: torn tail at offset %d, truncating %d bytes", lf.path, size, st.Size()-size)
			if err := os.Truncate(lf.path, size); err != nil {
				return err
			}
		}
		w.log.size = size
		for id, mj := range metas {
			w.log.ids[walMetaKey{mj.S, mj.M}] = id
			w.log.next = max(w.log.next, id+1)
		}
		if skipped > 0 {
			m.stats.WALSkipped += int64(skipped)
			m.opts.Logf("movers: wal %s: skipped %d frames that did not decode", lf.path, skipped)
		}
		m.evict()
	}
	for s, ids := range redo {
		if len(ids) > 0 {
			m.rebody(s, ids)
		}
	}
	return nil
}

// replayRow adds a row read back from a log at Open to window w of s.
func (m *Store) replayRow(s *surface, w *openWin, r row, redo map[*surface]map[string]bool) {
	r.meta = s.intern(r.meta)
	m.seq++
	r.seq = m.seq
	s.openBody(&r, redo)
	s.observe(r.t)
	m.addRow(w, r)
}

// migrateWAL moves the rows of a single-file WAL (movers/wal/wal.log, written by stores
// before the per-window logs) into the window logs, then deletes it. Rows below a
// surface's horizon were committed already. A crash before the delete replays the file
// again; the repeated rows are equal (journey, t) duplicates, which Commit removes.
func (m *Store) migrateWAL() error {
	dir := filepath.Join(m.dir, "wal")
	path := filepath.Join(dir, "wal.log")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	type pend struct {
		s *surface
		r row
	}
	var rows []pend
	_, _, skipped, err := readLog(path, -1, func(key core.SurfaceKey, r row) {
		s := m.sfc[key]
		if s == nil {
			if key.Validate() != nil {
				return
			}
			s = m.newSurface(key)
			m.sfc[key] = s
		}
		if s.hasHorizon && r.t < s.horizon {
			return
		}
		rows = append(rows, pend{s, r})
	})
	if err != nil {
		return err
	}
	if skipped > 0 {
		m.stats.WALSkipped += int64(skipped)
		m.opts.Logf("movers: wal %s: skipped %d frames that did not decode", path, skipped)
	}
	redo := map[*surface]map[string]bool{}
	for len(rows) > 0 {
		n := min(len(rows), 16384)
		chunk := rows[:n]
		rows = rows[n:]
		byLog := map[*winLog]int{}
		var batches []logBatch
		wins := make([]*openWin, len(chunk))
		for i := range chunk {
			p := &chunk[i]
			w, err := m.win(p.s, core.WindowStart(p.r.t, core.MoverWindowLen))
			if err != nil {
				return err
			}
			wins[i] = w
			k, ok := byLog[w.log]
			if !ok {
				k = len(batches)
				byLog[w.log] = k
				batches = append(batches, logBatch{log: w.log})
			}
			batches[k].entries = append(batches[k].entries, walEntry{sfc: p.s.key, r: &p.r})
		}
		if err := m.writeLogs(batches); err != nil {
			return err
		}
		for i := range chunk {
			m.replayRow(chunk[i].s, wins[i], chunk[i].r, redo)
		}
		m.evict()
	}
	for s, ids := range redo {
		if len(ids) > 0 {
			m.rebody(s, ids)
		}
	}
	if err := m.syncLogs(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	os.Remove(dir) // fails while it holds anything else, which is fine
	m.opts.Logf("movers: moved the open rows of %s into the window logs", path)
	return nil
}
