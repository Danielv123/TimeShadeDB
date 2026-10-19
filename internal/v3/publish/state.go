package publish

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// The publisher's durable state, one directory per target:
//
//   - target.json: {"version": 1, "target": "<Target.ID()>"}. A state is never used for
//     another target.
//   - done.log: append-only, one line per stored object: key, hash, size and class
//     ("i" immutable, "m" mutable), tab-separated. The last line for a key wins. A torn
//     last line (a crash during the append) is cut off at open. It is fsynced every 256
//     records and at the end of each phase; a record lost in a crash only means the
//     object is stored again, which is harmless.
//   - queue.json: the objects of the round in progress, written (atomically) before the
//     round uploads anything and removed when it ends. A restarted publisher finishes
//     this queue, in its order, before it plans a new round.
//
// done.log is rewritten without its superseded lines at open when it has grown to more
// than twice its live records.

const (
	stateVersion = 1
	syncEvery    = 256
)

// record is what the state knows about one stored object.
type record struct {
	Hash      string
	Size      int64
	Immutable bool
}

// State is the durable record of what a target holds. It is safe for concurrent use.
type State struct {
	dir string

	mu       sync.Mutex
	done     map[string]record
	log      *os.File
	w        *bufio.Writer
	unsynced int
	lines    int
}

type stateMeta struct {
	Version int    `json:"version"`
	Target  string `json:"target"`
}

// OpenState opens (or creates) the state in dir for targetID. reset forgets what the
// state recorded, so every object is stored again.
func OpenState(dir, targetID string, reset bool) (*State, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	metaPath := filepath.Join(dir, "target.json")
	if b, err := os.ReadFile(metaPath); err == nil {
		var m stateMeta
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("publish state %s: %w", metaPath, err)
		}
		if m.Target != targetID && !reset {
			return nil, fmt.Errorf("publish state %s belongs to %q, not %q (use another state directory, or reset it)", dir, m.Target, targetID)
		}
		if m.Version != stateVersion && !reset {
			return nil, fmt.Errorf("publish state %s has version %d, want %d", dir, m.Version, stateVersion)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if reset {
		for _, f := range []string{"done.log", "queue.json"} {
			if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	mb, _ := json.Marshal(stateMeta{Version: stateVersion, Target: targetID})
	if err := writeFileAtomic(metaPath, mb); err != nil {
		return nil, err
	}
	s := &State{dir: dir, done: map[string]record{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	if s.lines > 2*len(s.done)+1024 {
		if err := s.compact(); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "done.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.log = f
	s.w = bufio.NewWriterSize(f, 64<<10)
	return s, nil
}

// load reads done.log and cuts off a torn last line.
func (s *State) load() error {
	p := filepath.Join(s.dir, "done.log")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	good := bytes.LastIndexByte(b, '\n') + 1
	for _, line := range strings.Split(string(b[:good]), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue // not ours; skipped
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue
		}
		s.done[f[0]] = record{Hash: f[1], Size: n, Immutable: f[3] == "i"}
		s.lines++
	}
	if good < len(b) {
		if err := os.Truncate(p, int64(good)); err != nil {
			return err
		}
	}
	return nil
}

func formatRecord(key string, r record) string {
	c := "m"
	if r.Immutable {
		c = "i"
	}
	return key + "\t" + r.Hash + "\t" + strconv.FormatInt(r.Size, 10) + "\t" + c + "\n"
}

// compact rewrites done.log with one line per key.
func (s *State) compact() error {
	var buf bytes.Buffer
	for k, r := range s.done {
		buf.WriteString(formatRecord(k, r))
	}
	if err := writeFileAtomic(filepath.Join(s.dir, "done.log"), buf.Bytes()); err != nil {
		return err
	}
	s.lines = len(s.done)
	return nil
}

// Lookup returns the record of key.
func (s *State) Lookup(key string) (record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.done[key]
	return r, ok
}

// Len is the number of objects recorded.
func (s *State) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.done)
}

// Record notes that key is stored with r.
func (s *State) Record(key string, r record) error {
	if strings.ContainsAny(key, "\t\n") {
		return fmt.Errorf("publish state: bad key %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done[key] = r
	s.lines++
	if _, err := s.w.WriteString(formatRecord(key, r)); err != nil {
		return err
	}
	s.unsynced++
	if s.unsynced >= syncEvery {
		return s.syncLocked()
	}
	return nil
}

// Sync makes every record durable.
func (s *State) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncLocked()
}

func (s *State) syncLocked() error {
	if s.unsynced == 0 {
		return nil
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	s.unsynced = 0
	return s.log.Sync()
}

// SaveQueue writes the queue of the round in progress; nil or empty removes it.
func (s *State) SaveQueue(items []Item) error {
	p := filepath.Join(s.dir, "queue.json")
	if len(items) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, b)
}

// LoadQueue returns the queue a stopped round left, or nil.
func (s *State) LoadQueue() ([]Item, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, "queue.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Item
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("publish queue: %w", err)
	}
	return items, nil
}

// Close syncs and closes the log.
func (s *State) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log == nil {
		return nil
	}
	err := s.syncLocked()
	err = errors.Join(err, s.log.Close())
	s.log = nil
	return err
}

// writeFileAtomic writes b to p through a synced temporary file and a rename.
func writeFileAtomic(p string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
