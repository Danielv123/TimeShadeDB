package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"

	"timeshadedb/internal/v3/core"
)

// Maps is the map metadata of a database: <db>/maps.json, a JSON array of core.MapInfo
// (docs/v3/FORMAT.md 5). The importer writes it; "map" messages (PROTOCOL.md 3) replace
// the entry of their key; the catalog reads it. It is safe for concurrent use.
type Maps struct {
	path string

	mu   sync.RWMutex
	list []core.MapInfo // sorted by Key
	gen  uint64
}

// MaxMaps bounds the number of maps kept, so a hostile forwarder cannot grow maps.json
// without limit.
const MaxMaps = 1024

// OpenMaps loads <db>/maps.json; a missing file is an empty list.
func OpenMaps(db string) (*Maps, error) {
	m := &Maps{path: filepath.Join(db, "maps.json")}
	b, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m.list); err != nil {
		return nil, fmt.Errorf("maps.json: %w", err)
	}
	sort.Slice(m.list, func(i, j int) bool { return m.list[i].Key < m.list[j].Key })
	return m, nil
}

// List returns a copy of the maps, sorted by key, and a generation counter that changes
// whenever the list does.
func (m *Maps) List() ([]core.MapInfo, uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]core.MapInfo(nil), m.list...), m.gen
}

// Get returns the map with that raw key.
func (m *Maps) Get(key string) (core.MapInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i := sort.Search(len(m.list), func(i int) bool { return m.list[i].Key >= key })
	if i < len(m.list) && m.list[i].Key == key {
		return m.list[i], true
	}
	return core.MapInfo{}, false
}

// Put stores info under info.Key and rewrites maps.json (tmp + rename) when it changed.
// It reports whether anything changed.
func (m *Maps) Put(info core.MapInfo) (bool, error) {
	if info.Key == "" || len(info.Key) > core.MaxNameBytes {
		return false, fmt.Errorf("%w: map key %q", core.ErrBadKey, info.Key)
	}
	if info.Instances == nil {
		info.Instances = []core.InstanceInfo{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := sort.Search(len(m.list), func(i int) bool { return m.list[i].Key >= info.Key })
	if i < len(m.list) && m.list[i].Key == info.Key {
		if reflect.DeepEqual(m.list[i], info) {
			return false, nil
		}
		next := append([]core.MapInfo(nil), m.list...)
		next[i] = info
		if err := m.save(next); err != nil {
			return false, err
		}
		m.list = next
	} else {
		if len(m.list) >= MaxMaps {
			return false, fmt.Errorf("maps: more than %d maps", MaxMaps)
		}
		next := make([]core.MapInfo, 0, len(m.list)+1)
		next = append(next, m.list[:i]...)
		next = append(next, info)
		next = append(next, m.list[i:]...)
		if err := m.save(next); err != nil {
			return false, err
		}
		m.list = next
	}
	m.gen++
	return true, nil
}

func (m *Maps) save(list []core.MapInfo) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, m.path)
}
