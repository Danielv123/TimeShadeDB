// Key layout ported from illisis-minimap shared/keys.ts.
// Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package minimap

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"timeshadedb/internal/v3/core"
)

// Dataset is an opened export: its catalog and the files discovered under its root.
// Open only lists directories and reads the catalog; the import reads the files.
type Dataset struct {
	// Root is the absolute export root (the directory holding maps.json.gz).
	Root string
	// WindowMs is illisis's window length from the catalog (300000 by default). The
	// clamp fix uses it to find samples stamped on a window floor.
	WindowMs int64
	// Maps is the map metadata from the catalog, sorted by key.
	Maps []core.MapInfo
	// Icons are the catalog's icon spritesheet and metadata paths relative to Root, when
	// both exist on disk.
	Icons *core.CatalogIcons
	// Datastores are the (map, surface, force) directories with z0 areas, tags or
	// recipes, sorted by ID.
	Datastores []*SourceDatastore
	// Surfaces are the map surfaces with mover files, sorted by ID.
	Surfaces []*SourceSurface
	// Warnings lists odd things found while listing (unknown files are ignored).
	Warnings []string
}

// SourceDatastore is one <map>/<surface>/<force> directory.
type SourceDatastore struct {
	Key core.DatastoreKey
	Dir string
	// Areas are the z0 areas with an index, sorted by (y, x).
	Areas []SourceArea
	// Coarse lists the areas of levels 1..4 (index i is level i+1); only Verify reads them.
	Coarse [Levels - 1][]SourceArea
	// Tags is the tags.json.gz path, or "".
	Tags string
	// Recipes are the z0 recipes.json.gz paths.
	Recipes []string
}

// SourceArea is one <z>/<ax>_<ay> directory. At z0 Tile is the v3 tile with the same
// coordinates; at level k it is the v3 tile at z = -k (both grids share the -256 tile
// origin).
type SourceArea struct {
	Tile core.TileCoord
	Dir  string
}

// SourceSurface is one <map>/<surface>/m directory.
type SourceSurface struct {
	Key core.SurfaceKey
	Dir string
	// Windows and Keyframes are the file times (their names), sorted.
	Windows   []int64
	Keyframes []int64
}

// WindowPath returns the path of the window file named t.
func (s *SourceSurface) WindowPath(t int64) string {
	return filepath.Join(s.Dir, strconv.FormatInt(t, 10)+".m.gz")
}

// KeyframePath returns the path of the keyframe file named t.
func (s *SourceSurface) KeyframePath(t int64) string {
	return filepath.Join(s.Dir, strconv.FormatInt(t, 10)+".k.gz")
}

var (
	areaDirRe = regexp.MustCompile(`^(-?[0-9]{1,6})_(-?[0-9]{1,6})$`)
	timeRe    = regexp.MustCompile(`^([0-9]{1,15})\.([mk])\.gz$`)
)

// catalogJSON is maps.json.gz (illisis keys.ts Catalog).
type catalogJSON struct {
	Updated  int64 `json:"updated"`
	WindowMs int64 `json:"window_ms"`
	Icons    *struct {
		PNG  string `json:"png"`
		JSON string `json:"json"`
	} `json:"icons"`
	Maps map[string]struct {
		Title     string `json:"title"`
		TileSize  int    `json:"tileSize"`
		Instances map[string]struct {
			Name   string     `json:"name"`
			Bounds *[]float64 `json:"bounds"`
		} `json:"instances"`
		Surfaces map[string]struct {
			Edges [][]float64 `json:"edges"`
		} `json:"surfaces"`
	} `json:"maps"`
}

// Open lists an export directory. root is the directory holding maps.json.gz; it is
// passed explicitly and never taken from the working directory.
func Open(root string) (*Dataset, error) {
	if root == "" {
		return nil, errors.New("minimap: no data directory given")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("minimap: %s is not a directory", abs)
	}
	d := &Dataset{Root: abs, WindowMs: 300000}
	if err := d.readCatalog(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "icons" {
			continue
		}
		if err := d.listMap(e.Name()); err != nil {
			return nil, err
		}
	}
	sort.Slice(d.Datastores, func(i, j int) bool { return d.Datastores[i].Key.ID() < d.Datastores[j].Key.ID() })
	sort.Slice(d.Surfaces, func(i, j int) bool { return d.Surfaces[i].Key.ID() < d.Surfaces[j].Key.ID() })
	return d, nil
}

func (d *Dataset) warn(format string, args ...any) {
	if len(d.Warnings) < 100 {
		d.Warnings = append(d.Warnings, fmt.Sprintf(format, args...))
	}
}

func (d *Dataset) readCatalog() error {
	b, err := readGz(filepath.Join(d.Root, "maps.json.gz"), maxJSONBytes)
	if err != nil {
		return fmt.Errorf("minimap: catalog: %w", err)
	}
	var c catalogJSON
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("minimap: catalog: %w: %v", ErrFormat, err)
	}
	if c.WindowMs >= 1000 && c.WindowMs <= int64(core.Day) {
		d.WindowMs = c.WindowMs
	}
	if c.Icons != nil && safeRel(c.Icons.PNG) && safeRel(c.Icons.JSON) &&
		isFile(filepath.Join(d.Root, filepath.FromSlash(c.Icons.PNG))) && isFile(filepath.Join(d.Root, filepath.FromSlash(c.Icons.JSON))) {
		d.Icons = &core.CatalogIcons{PNG: c.Icons.PNG, JSON: c.Icons.JSON}
	}
	keys := make([]string, 0, len(c.Maps))
	for k := range c.Maps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := c.Maps[k]
		raw, err := Unname(k)
		if err != nil {
			d.warn("catalog map %q: %v", k, err)
			continue
		}
		info := core.MapInfo{Key: raw, Title: m.Title, TileSize: m.TileSize, Instances: []core.InstanceInfo{}}
		ids := make([]string, 0, len(m.Instances))
		for id := range m.Instances {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return numLess(ids[i], ids[j]) })
		for _, id := range ids {
			in := m.Instances[id]
			ii := core.InstanceInfo{ID: id, Name: in.Name}
			if in.Bounds != nil {
				if q, ok := quad(*in.Bounds); ok {
					ii.Bounds = &q
				}
			}
			info.Instances = append(info.Instances, ii)
		}
		snames := make([]string, 0, len(m.Surfaces))
		for s := range m.Surfaces {
			snames = append(snames, s)
		}
		sort.Strings(snames)
		for _, s := range snames {
			var edges []core.Quad
			for _, e := range m.Surfaces[s].Edges {
				if q, ok := quad(e); ok {
					edges = append(edges, q)
				}
			}
			if len(edges) > 0 {
				if info.Edges == nil {
					info.Edges = map[string][]core.Quad{}
				}
				info.Edges[s] = edges
			}
		}
		d.Maps = append(d.Maps, info)
	}
	return nil
}

// edges returns the Universal Edges segments of a surface from the catalog.
func (d *Dataset) edges(k core.SurfaceKey) []core.Quad {
	for i := range d.Maps {
		if d.Maps[i].Key == k.Map {
			return d.Maps[i].Edges[k.Surface]
		}
	}
	return nil
}

// numLess orders numeric strings numerically, others after them lexically.
func numLess(a, b string) bool {
	x, errA := strconv.ParseInt(a, 10, 64)
	y, errB := strconv.ParseInt(b, 10, 64)
	switch {
	case errA == nil && errB == nil:
		return x < y
	case errA == nil:
		return true
	case errB == nil:
		return false
	}
	return a < b
}

func quad(v []float64) (core.Quad, bool) {
	if len(v) != 4 {
		return core.Quad{}, false
	}
	for _, f := range v {
		if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e7 {
			return core.Quad{}, false
		}
	}
	return core.Quad{v[0], v[1], v[2], v[3]}, true
}

// safeRel reports whether p is a relative slash path without "..", empty or odd parts.
func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, ":") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func (d *Dataset) listMap(dir string) error {
	mapName, err := Unname(dir)
	if err != nil {
		d.warn("map directory %q: %v", dir, err)
		return nil
	}
	mapDir := filepath.Join(d.Root, dir)
	surfaces, err := os.ReadDir(mapDir)
	if err != nil {
		return err
	}
	for _, se := range surfaces {
		if !se.IsDir() {
			continue
		}
		surface, err := Unname(se.Name())
		if err != nil {
			d.warn("surface directory %q: %v", se.Name(), err)
			continue
		}
		sk := core.SurfaceKey{Map: mapName, Surface: surface}
		if sk.Validate() != nil {
			d.warn("surface %q/%q: bad key", mapName, surface)
			continue
		}
		surfDir := filepath.Join(mapDir, se.Name())
		forces, err := os.ReadDir(surfDir)
		if err != nil {
			return err
		}
		for _, fe := range forces {
			if !fe.IsDir() {
				continue
			}
			if fe.Name() == "m" {
				if err := d.listMovers(sk, filepath.Join(surfDir, "m")); err != nil {
					return err
				}
				continue
			}
			force, err := Unname(fe.Name())
			if err != nil {
				d.warn("force directory %q: %v", fe.Name(), err)
				continue
			}
			key := core.DatastoreKey{Map: mapName, Surface: surface, Force: force}
			if key.Validate() != nil {
				d.warn("datastore %v: bad key", key)
				continue
			}
			if err := d.listForce(key, filepath.Join(surfDir, fe.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Dataset) listForce(key core.DatastoreKey, dir string) error {
	src := &SourceDatastore{Key: key, Dir: dir}
	if p := filepath.Join(dir, "tags.json.gz"); isFile(p) {
		src.Tags = p
	}
	for z := 0; z < Levels; z++ {
		zdir := filepath.Join(dir, strconv.Itoa(z))
		entries, err := os.ReadDir(zdir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var areas []SourceArea
		for _, e := range entries {
			m := areaDirRe.FindStringSubmatch(e.Name())
			if !e.IsDir() || m == nil {
				continue
			}
			ax, _ := strconv.Atoi(m[1])
			ay, _ := strconv.Atoi(m[2])
			tile := core.TileCoord{Z: int8(-z), X: int32(ax), Y: int32(ay)}
			if !tile.Valid() {
				d.warn("%v: area %s outside the grid", key, e.Name())
				continue
			}
			adir := filepath.Join(zdir, e.Name())
			if isFile(filepath.Join(adir, "index.json.gz")) {
				areas = append(areas, SourceArea{Tile: tile, Dir: adir})
			}
			if z == 0 {
				if p := filepath.Join(adir, "recipes.json.gz"); isFile(p) {
					src.Recipes = append(src.Recipes, p)
				}
			}
		}
		sort.Slice(areas, func(i, j int) bool {
			a, b := areas[i].Tile, areas[j].Tile
			if a.Y != b.Y {
				return a.Y < b.Y
			}
			return a.X < b.X
		})
		if z == 0 {
			src.Areas = areas
		} else {
			src.Coarse[z-1] = areas
		}
	}
	sort.Strings(src.Recipes)
	if len(src.Areas) > 0 || src.Tags != "" || len(src.Recipes) > 0 {
		d.Datastores = append(d.Datastores, src)
	}
	return nil
}

func (d *Dataset) listMovers(key core.SurfaceKey, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	s := &SourceSurface{Key: key, Dir: dir}
	for _, e := range entries {
		m := timeRe.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		t, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || !saneTime(t) {
			d.warn("%v: odd mover file %s", key, e.Name())
			continue
		}
		if m[2] == "m" {
			s.Windows = append(s.Windows, t)
		} else {
			s.Keyframes = append(s.Keyframes, t)
		}
	}
	sort.Slice(s.Windows, func(i, j int) bool { return s.Windows[i] < s.Windows[j] })
	sort.Slice(s.Keyframes, func(i, j int) bool { return s.Keyframes[i] < s.Keyframes[j] })
	if len(s.Windows) > 0 || len(s.Keyframes) > 0 {
		d.Surfaces = append(d.Surfaces, s)
	}
	return nil
}

// saneTime bounds imported times to 2000-01-01 .. 2200-01-01 UTC.
func saneTime(t int64) bool {
	return t >= 946684800000 && t < 7258118400000
}
