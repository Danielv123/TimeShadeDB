package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/tileenc"
)

// catalogTTL bounds how long a built catalog is reused; changes the server sees
// (commits, new datastores, map messages) rebuild it sooner.
const catalogTTL = 2 * time.Second

// LiveMs is the catalog's recommended poll interval for mutable data.
const LiveMs = 5000

// IconsFile is the file in the icons directory that names the spritesheet and its
// metadata (core.CatalogIcons with paths relative to the API base, e.g.
// "icons/spritesheet.<hash>.png"). docs/v3/FORMAT.md 5.
const IconsFile = "icons.json"

// iconsCache holds what <icons dir>/icons.json names. The Clusterio plugin replaces the
// file while the server runs (a new mod-pack export), so get re-reads it when its
// modification time or size changed: one stat per catalog build or icon request.
type iconsCache struct {
	dir    string
	mu     sync.Mutex
	loaded bool // icons and stamp are those of the file as it stood at stamp
	stamp  [2]int64
	icons  *core.CatalogIcons
}

// get returns the current icons; nil when there are none or icons.json is invalid. An
// invalid file is read again on the next call (its named files may not be there yet).
func (c *iconsCache) get() *core.CatalogIcons {
	if c.dir == "" {
		return nil
	}
	var stamp [2]int64 // zero: no icons.json
	if fi, err := os.Stat(filepath.Join(c.dir, IconsFile)); err == nil {
		stamp = [2]int64{fi.ModTime().UnixNano(), fi.Size()}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded && c.stamp == stamp {
		return c.icons
	}
	c.icons = nil
	if stamp != ([2]int64{}) {
		c.icons = loadIcons(c.dir)
	}
	c.loaded = c.icons != nil || stamp == ([2]int64{})
	c.stamp = stamp
	return c.icons
}

// loadIcons reads <icons dir>/icons.json; nil when absent or invalid.
func loadIcons(dir string) *core.CatalogIcons {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, IconsFile))
	if err != nil {
		return nil
	}
	var ic core.CatalogIcons
	if json.Unmarshal(b, &ic) != nil {
		return nil
	}
	for _, p := range []string{ic.PNG, ic.JSON} {
		name, ok := cutIconPath(p)
		if !ok || !validIconName(name) {
			return nil
		}
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || !fi.Mode().IsRegular() {
			return nil
		}
	}
	return &ic
}

func cutIconPath(p string) (string, bool) {
	if len(p) > len("icons/") && p[:len("icons/")] == "icons/" {
		return p[len("icons/"):], true
	}
	return "", false
}

// validIconName accepts plain file names: [A-Za-z0-9._-], not starting with a dot.
func validIconName(name string) bool {
	if name == "" || len(name) > 200 || name[0] == '.' || name == IconsFile {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// invalidateCatalog makes the next catalog request rebuild it.
func (s *Server) invalidateCatalog() {
	s.catMu.Lock()
	s.catGen++
	s.catMu.Unlock()
}

// GET catalog (and catalog.json)
func (s *Server) serveCatalog(ctx context.Context, w http.ResponseWriter, r *http.Request) (int, error) {
	body, etag, err := s.catalogBody(ctx)
	if err != nil {
		return 0, err
	}
	return s.writeRawJSON(w, r, cacheShort, body, etag), nil
}

// catalogBody returns the catalog JSON and its ETag. The "now" field is left out of the
// ETag so an unchanged catalog revalidates; it is refreshed on every rebuild.
func (s *Server) catalogBody(ctx context.Context) ([]byte, string, error) {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if s.catBody != nil && s.catBuilt == s.catGen && time.Since(s.catAt) < catalogTTL {
		return s.catBody, s.catETag, nil
	}
	gen := s.catGen
	cat, err := s.buildCatalog(ctx)
	if err != nil {
		return nil, "", err
	}
	cat.Now = 0
	stable, err := json.Marshal(cat)
	if err != nil {
		return nil, "", err
	}
	cat.Now = s.cfg.Clock()
	body, err := json.Marshal(cat)
	if err != nil {
		return nil, "", err
	}
	etag := weakETag(stable)
	s.catBody, s.catETag, s.catAt, s.catBuilt = body, etag, time.Now(), gen
	return body, etag, nil
}

func (s *Server) buildCatalog(ctx context.Context) (core.Catalog, error) {
	cat := core.Catalog{Version: core.APIVersion, LiveMs: LiveMs, Maps: []core.CatalogMap{}, Icons: s.icons.get()}
	infos, err := s.cfg.Terrain.Datastores(ctx)
	if err != nil {
		return cat, err
	}
	surfaces, err := s.cfg.Movers.Surfaces(ctx)
	if err != nil {
		return cat, err
	}
	type sfcEntry struct {
		cs  core.CatalogSurface
		key core.SurfaceKey
	}
	maps := map[string]map[string]*sfcEntry{} // raw map key -> surface id -> entry
	entry := func(k core.SurfaceKey) *sfcEntry {
		m := maps[k.Map]
		if m == nil {
			m = map[string]*sfcEntry{}
			maps[k.Map] = m
		}
		id := k.ID()
		e := m[id]
		if e == nil {
			e = &sfcEntry{key: k, cs: core.CatalogSurface{Name: k.Surface, ID: id, Edges: []core.Quad{}, Datastores: []core.CatalogDatastore{}}}
			m[id] = e
		}
		return e
	}
	for _, info := range infos {
		e := entry(info.Key.SurfaceKey())
		e.cs.Datastores = append(e.cs.Datastores, core.CatalogDatastore{
			Force:          info.Key.Force,
			ID:             info.ID,
			WindowMs:       info.Window,
			Horizon:        info.Horizon,
			First:          info.FirstT,
			Last:           info.LastT,
			PaletteVersion: info.PaletteVersion,
			Bounds:         info.Bounds,
			Levels:         s.levels(ctx, info),
		})
	}
	for _, si := range surfaces {
		e := entry(si.Key)
		days := si.Days
		if days == nil {
			days = []string{}
		}
		e.cs.Movers = &core.CatalogMovers{Horizon: si.Horizon, First: si.FirstT, Last: si.LastT, Days: days}
	}
	var known []core.MapInfo
	if s.cfg.Maps != nil {
		known, _ = s.cfg.Maps.List()
	}
	byKey := map[string]core.MapInfo{}
	for _, mi := range known {
		byKey[mi.Key] = mi
		if maps[mi.Key] == nil {
			maps[mi.Key] = map[string]*sfcEntry{}
		}
	}
	for _, mk := range sortedKeys(maps) {
		mi, ok := byKey[mk]
		if !ok {
			mi = core.MapInfo{Key: mk, Title: mk}
		}
		if mi.Instances == nil {
			mi.Instances = []core.InstanceInfo{}
		}
		cm := core.CatalogMap{MapInfo: mi, Surfaces: []core.CatalogSurface{}}
		for _, id := range sortedKeys(maps[mk]) {
			e := maps[mk][id]
			if edges := mi.Edges[e.key.Surface]; edges != nil {
				e.cs.Edges = edges
			}
			cm.Surfaces = append(cm.Surfaces, e.cs)
		}
		cat.Maps = append(cat.Maps, cm)
	}
	return cat, nil
}

// GET icons/{file}: the catalog icon files, immutable (content-hashed names).
func (s *Server) serveIcon(ctx context.Context, w http.ResponseWriter, r *http.Request, tail string) (int, error) {
	icons := s.icons.get()
	if icons == nil || !validIconName(tail) {
		return 0, notFound("no icon %q", truncate(tail))
	}
	if p, _ := cutIconPath(icons.PNG); tail != p {
		if j, _ := cutIconPath(icons.JSON); tail != j {
			return 0, notFound("no icon %q", truncate(tail))
		}
	}
	path := filepath.Join(s.cfg.IconsDir, tail)
	ctype := "application/octet-stream"
	switch filepath.Ext(tail) {
	case ".png":
		ctype = "image/png"
	case ".json":
		ctype = "application/json"
	}
	enc := tileenc.Identity
	if ctype == "application/json" {
		enc = tileenc.Negotiate(r.Header.Get("Accept-Encoding"))
	}
	b, err := s.cfg.Renderer.Cached(ctx, "icons/"+tail, enc, func(context.Context) ([]byte, error) {
		return os.ReadFile(path)
	})
	if err != nil {
		if os.IsNotExist(err) {
			return 0, notFound("no icon %q", truncate(tail))
		}
		return 0, err
	}
	return s.writeBody(w, ctype, cacheImmutable, b), nil
}

func jsonMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	return b[:len(b)-1], nil // drop the encoder's newline
}
