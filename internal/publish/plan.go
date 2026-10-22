package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"timeshadedb/internal/core"
)

// Cache-Control values of the static export (docs/API.md 6).
const (
	CacheImmutable = "public, max-age=31536000, immutable"
	CacheCatalog   = "public, max-age=10"
	CacheOpen      = "public, max-age=10" // timelines and day indexes until final
	CacheLive      = "public, max-age=5"  // movers live.bin
	CacheOverlays  = "public, max-age=60"
	CachePage      = "public, max-age=300" // index.html and other viewer files that are not content-hashed
)

// Phases: everything a file names is stored before the file. Immutable data first, then
// the final timelines and day indexes that name it (only when every data object of the
// round was stored: a stored final index is never read again, see planDatastore), then
// the mutable data, then the catalog, then the page.
const (
	phaseImmutable = 0
	phaseIndexes   = 1
	phaseMutable   = 2
	phaseCatalog   = 3
	phasePage      = 4
	numPhases      = 5
)

// Item sources.
const (
	srcAPI     = ""        // Path is read from the Source
	srcCatalog = "catalog" // the catalog, rewritten for the static export
	srcWeb     = "web"     // Path is a file of the viewer bundle
)

// Item is one object to store. Items are persisted in queue.json, so a restarted
// publisher finishes a round.
type Item struct {
	Key       string `json:"k"`
	Src       string `json:"s,omitempty"`
	Path      string `json:"p,omitempty"`
	Immutable bool   `json:"i,omitempty"`
	Cache     string `json:"c"`
	Phase     int    `json:"ph"`
	// SkipEmpty: an overlay file with no intervals is not stored unless it was before
	// (sources without an OverlayLister ask every z0 tile).
	SkipEmpty bool `json:"e,omitempty"`

	// body is a response read while planning (timelines, day indexes), reused once.
	body  []byte
	ctype string
}

// planner enumerates a round's objects from the catalog, the timelines and the day
// indexes.
type planner struct {
	src Source
	opt *Options
	// stored says whether the state records key as stored immutable.
	stored func(key string) bool
	items  []Item
}

// span is the range of versions of a tile a round enumerates.
type span struct{ lo, hi core.Version }

// tileVersions holds the versions to enumerate per tile at one level.
type tileVersions map[[2]int32]span

func (p *planner) add(it Item) { p.items = append(p.items, it) }

func (p *planner) getJSON(ctx context.Context, path string, v any) (Resource, error) {
	r, err := p.src.Get(ctx, path)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(r.Body, v); err != nil {
		return r, fmt.Errorf("get %s: %w", path, err)
	}
	return r, nil
}

// escapeSeg is what the viewer's encodeURIComponent makes of an id; ids are canonical
// (FORMAT.md 1), so this is the id itself, and anything else is refused by validKey.
func escapeSeg(s string) string { return url.PathEscape(s) }

func utcDays(from, to core.TimeMs) []string {
	var days []string
	for t := core.TimeMs(int64(from) - int64(from)%int64(core.Day)); t <= to; t += core.Day {
		days = append(days, core.UTCDay(t))
	}
	return days
}

// plan returns the round's items, grouped by phase and ordered within each phase.
func (p *planner) plan(ctx context.Context) ([]Item, error) {
	catRes, err := p.src.Get(ctx, "catalog")
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var cat core.Catalog
	if err := json.Unmarshal(catRes.Body, &cat); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	staticCat, err := staticCatalog(catRes.Body, p.opt.LiveMs)
	if err != nil {
		return nil, err
	}
	today := core.UTCDay(core.TimeMs(p.opt.Now().UnixMilli()))

	if cat.Icons != nil {
		for _, f := range []string{cat.Icons.PNG, cat.Icons.JSON} {
			if !strings.HasPrefix(f, "icons/") || !validKey(f) {
				p.opt.Logf("publish: skipping icon file %q", f)
				continue
			}
			p.add(Item{Key: f, Path: f, Immutable: true, Cache: CacheImmutable, Phase: phaseImmutable})
		}
	}
	for _, m := range cat.Maps {
		for _, s := range m.Surfaces {
			for _, d := range s.Datastores {
				if err := p.planDatastore(ctx, d, today); err != nil {
					return nil, err
				}
			}
			if s.Movers != nil && !p.opt.NoMovers {
				if err := p.planMovers(ctx, s.ID, s.Movers); err != nil {
					return nil, err
				}
			}
		}
	}
	if p.opt.Web != nil {
		if err := p.planWeb(); err != nil {
			return nil, err
		}
	}
	p.add(Item{Key: "catalog.json", Src: srcCatalog, Cache: CacheCatalog, Phase: phaseCatalog, body: staticCat, ctype: "application/json"})
	sort.SliceStable(p.items, func(i, j int) bool { return p.items[i].Phase < p.items[j].Phase })
	return p.items, nil
}

func (p *planner) planDatastore(ctx context.Context, d core.CatalogDatastore, today string) error {
	id := escapeSeg(d.ID)
	if !validKey(id) {
		p.opt.Logf("publish: skipping datastore with bad id %q", d.ID)
		return nil
	}
	if len(d.Levels) == 0 || d.Last == 0 {
		return nil
	}
	last := d.Last + d.WindowMs
	// The stores take no time before 2016 nor more than an hour ahead of the clock
	// (terrain and movers MinTime, MaxAhead), so a datastore spans a bounded number of days.
	days := utcDays(d.First, last)
	if days[len(days)-1] < today {
		// Start-up on a static export asks for today's timeline for "latest"
		// (web/src/map/boot.ts), which answers for every time after the newest data.
		days = append(days, today)
	}
	if d.PaletteVersion > 0 {
		k := "palette/" + id + "/" + strconv.FormatUint(uint64(d.PaletteVersion), 10) + ".bin"
		p.add(Item{Key: k, Path: k, Immutable: true, Cache: CacheImmutable, Phase: phaseImmutable})
	}
	// A final timeline the state records as stored is not read again: it was stored
	// after every tile version and delta it names (phaseIndexes), and final days form a
	// prefix of the days, so the first timeline read starts at a v0 whose versions are
	// all stored. Steady rounds read only the open days.
	var z0 tileVersions
	for _, z := range d.Levels {
		zs := strconv.Itoa(int(z))
		vers := tileVersions{}
		for _, day := range days {
			k := "timeline/" + id + "/" + zs + "/" + day + ".json"
			if p.stored(k) {
				continue
			}
			var tl core.TimelineResponse
			r, err := p.getJSON(ctx, k, &tl)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			it := Item{Key: k, Path: k, Cache: CacheOpen, Phase: phaseMutable, body: r.Body, ctype: r.ContentType}
			if tl.Final {
				it.Immutable, it.Cache, it.Phase = true, CacheImmutable, phaseIndexes
			}
			p.add(it)
			for _, t := range tl.Tiles {
				xy := [2]int32{t.X, t.Y}
				sp, ok := vers[xy]
				if !ok {
					sp = span{lo: t.V0, hi: t.V0}
				}
				sp.lo = min(sp.lo, t.V0)
				sp.hi = max(sp.hi, t.V0)
				for _, pt := range t.Points {
					sp.hi = max(sp.hi, core.Version(pt[1]))
				}
				vers[xy] = sp
			}
		}
		if z == 0 {
			z0 = vers
		}
		p.planTiles(id, z, vers)
	}
	if p.opt.NoOverlays {
		return nil
	}
	return p.planOverlays(ctx, d.ID, id, z0)
}

// fullVersion says whether version v of a tile whose newest version is latest is
// exported as a full tile (docs: README.md "Tile versions").
func (p *planner) fullVersion(z int8, v, latest core.Version) bool {
	n := core.Version(max(p.opt.KeyframeEvery, 1))
	return z < 0 || n == 1 || v <= 1 || v == latest || v%n == 0
}

func (p *planner) planTiles(id string, z int8, vers tileVersions) {
	keys := make([][2]int32, 0, len(vers))
	for xy := range vers {
		keys = append(keys, xy)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][1] != keys[j][1] {
			return keys[i][1] < keys[j][1]
		}
		return keys[i][0] < keys[j][0]
	})
	zs := strconv.Itoa(int(z))
	for _, xy := range keys {
		sp := vers[xy]
		latest := sp.hi
		xs, ys := strconv.Itoa(int(xy[0])), strconv.Itoa(int(xy[1]))
		prefix := "tile/" + id + "/" + zs + "/" + xs + "/" + ys + "/"
		for v := sp.lo; v <= latest; v++ {
			if p.fullVersion(z, v, latest) {
				k := prefix + strconv.FormatUint(uint64(v), 10) + ".bin"
				p.add(Item{Key: k, Path: k, Immutable: true, Cache: CacheImmutable, Phase: phaseImmutable})
			}
		}
		if z != 0 {
			continue
		}
		dprefix := "tiledelta/" + id + "/" + xs + "/" + ys + "/"
		for v := max(sp.lo, 1); v < latest; v++ {
			k := dprefix + strconv.FormatUint(uint64(v), 10) + "-" + strconv.FormatUint(uint64(v+1), 10) + ".bin"
			p.add(Item{Key: k, Path: k, Immutable: true, Cache: CacheImmutable, Phase: phaseImmutable})
		}
	}
}

func (p *planner) planOverlays(ctx context.Context, rawID, id string, z0 tileVersions) error {
	var tiles []core.TileCoord
	skipEmpty := true
	if l, ok := p.src.(OverlayLister); ok {
		ts, err := l.OverlayTiles(ctx, rawID)
		switch {
		case err == nil:
			tiles, skipEmpty = ts, false
		case !errors.Is(err, errors.ErrUnsupported):
			return fmt.Errorf("overlay tiles of %s: %w", rawID, err)
		}
	}
	if skipEmpty {
		for xy := range z0 {
			tiles = append(tiles, core.TileCoord{X: xy[0], Y: xy[1]})
		}
	}
	sort.Slice(tiles, func(i, j int) bool {
		if tiles[i].Y != tiles[j].Y {
			return tiles[i].Y < tiles[j].Y
		}
		return tiles[i].X < tiles[j].X
	})
	for _, t := range tiles {
		k := "overlays/" + id + "/" + strconv.Itoa(int(t.X)) + "/" + strconv.Itoa(int(t.Y)) + ".json"
		p.add(Item{Key: k, Path: k, Cache: CacheOverlays, Phase: phaseMutable, SkipEmpty: skipEmpty})
	}
	return nil
}

func (p *planner) planMovers(ctx context.Context, rawID string, mv *core.CatalogMovers) error {
	id := escapeSeg(rawID)
	if !validKey(id) {
		p.opt.Logf("publish: skipping surface with bad id %q", rawID)
		return nil
	}
	base := "movers/" + id + "/"
	for _, day := range mv.Days {
		if _, err := core.ParseUTCDay(day); err != nil {
			continue
		}
		k := base + "day/" + day + ".json"
		if p.stored(k) {
			continue // stored after its windows and keyframes, as for timelines
		}
		var di core.DayIndex
		r, err := p.getJSON(ctx, k, &di)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		it := Item{Key: k, Path: k, Cache: CacheOpen, Phase: phaseMutable, body: r.Body, ctype: r.ContentType}
		if di.Final {
			it.Immutable, it.Cache, it.Phase = true, CacheImmutable, phaseIndexes
		}
		for _, w := range di.Windows {
			wk := base + "window/" + strconv.FormatInt(int64(w.Start), 10) + ".bin"
			p.add(Item{Key: wk, Path: wk, Immutable: true, Cache: CacheImmutable, Phase: phaseImmutable})
		}
		for _, t := range di.Keyframes {
			kk := base + "keyframe/" + strconv.FormatInt(int64(t), 10) + ".bin"
			p.add(Item{Key: kk, Path: kk, Immutable: true, Cache: CacheImmutable, Phase: phaseImmutable})
		}
		p.add(it)
	}
	lk := base + "live.bin"
	p.add(Item{Key: lk, Path: lk, Cache: CacheLive, Phase: phaseMutable})
	return nil
}

// planWeb adds the viewer bundle: content-hashed assets/* as immutable, index.html
// last, other files as short-cached page files. Dot files are skipped.
func (p *planner) planWeb() error {
	return fs.WalkDir(p.opt.Web, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := path.Base(name)
		if name != "." && strings.HasPrefix(base, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !validKey(name) {
			p.opt.Logf("publish: skipping viewer file %q", name)
			return nil
		}
		it := Item{Key: name, Src: srcWeb, Path: name, Cache: CachePage, Phase: phaseMutable}
		switch {
		case name == "index.html":
			it.Phase = phasePage
		case strings.HasPrefix(name, "assets/"):
			it.Immutable, it.Cache, it.Phase = true, CacheImmutable, phaseImmutable
		}
		p.add(it)
		return nil
	})
}

// staticCatalog rewrites the server's catalog for the static export: "static": true and
// the poll interval. Other fields are kept as they are, unknown ones included.
func staticCatalog(raw []byte, liveMs int) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	m["static"] = json.RawMessage("true")
	if liveMs > 0 {
		m["live_ms"] = json.RawMessage(strconv.Itoa(liveMs))
	}
	return json.Marshal(m)
}

// staticIndex rewrites the viewer's index.html for a static export: the tsdb-source
// meta says "static" (start-up then never probes /api/v3/), and root-relative asset
// URLs in the page become relative. URLs inside the bundle's scripts are not rewritten:
// a bundle built with Vite's default base "/" loads its worker and images from /assets/,
// so it works at the root of a site; build with `vite build --base ./` and pass
// --web-dir for an export under a sub-path.
func staticIndex(b []byte) []byte {
	s := string(b)
	const meta = `<meta name="tsdb-source" content="`
	if i := strings.Index(s, meta); i >= 0 {
		j := i + len(meta)
		if k := strings.IndexByte(s[j:], '"'); k >= 0 {
			s = s[:j] + "static" + s[j+k:]
		}
	} else if i := strings.Index(s, "</head>"); i >= 0 {
		s = s[:i] + `<meta name="tsdb-source" content="static" />` + "\n" + s[i:]
	}
	s = strings.ReplaceAll(s, `src="/assets/`, `src="./assets/`)
	s = strings.ReplaceAll(s, `href="/assets/`, `href="./assets/`)
	return []byte(s)
}

// webContentType is the Content-Type of a viewer file by extension (not the system's
// MIME table, which differs between machines).
func webContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".wasm":
		return "application/wasm"
	case ".woff2":
		return "font/woff2"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}
