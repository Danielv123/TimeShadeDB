// Package api is the v3 HTTP API (docs/v3/API.md, PLAN.md §4.6, WP6): the read routes
// under /api/v3/ over the terrain store, the pyramid, the tile renderer, the mover and
// overlay stores; POST /api/v3/ingest over internal/v3/ingest; live events over
// Server-Sent Events; /metrics, /healthz and the embedded viewer. Run drives the commit
// loop (terrain Commit, pyramid Update, movers Commit) and the live tile events.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/ingest"
	"timeshadedb/internal/v3/pyramid"
	"timeshadedb/internal/v3/tileenc"
)

// Coarse is what the API needs from the pyramid: a TileSource for every level, live
// content at every level, the build bound and the build step of the commit loop.
// *pyramid.Pyramid implements it.
type Coarse interface {
	core.TileSource
	ReadTile(ctx context.Context, ds core.DatastoreKey, tile core.TileCoord, t core.TimeMs, live bool) (*core.TileImage, core.TileState, error)
	BuiltUpTo(key core.DatastoreKey) (core.TimeMs, bool)
	Update(ctx context.Context) (pyramid.BuildStats, error)
}

// MoverFiles is implemented by mover stores that keep windows and keyframes as zstd
// frames of their MVW3 bodies (internal/v3/movers), so the API can send them unchanged.
type MoverFiles interface {
	WindowFile(ctx context.Context, sfc core.SurfaceKey, start core.TimeMs) ([]byte, error)
	KeyframeFile(ctx context.Context, sfc core.SurfaceKey, t core.TimeMs) ([]byte, error)
}

// Config wires a Server. Terrain, Tiles, Renderer, Movers and Overlays are required.
type Config struct {
	Terrain  core.TerrainStore
	Tiles    Coarse
	Renderer *tileenc.Renderer
	Movers   core.MoverStore
	Overlays core.OverlayStore
	// Ingest applies POST ingest bodies (nil: ingest answers 404).
	Ingest *ingest.Ingester
	// Maps is the database's maps.json (nil: no map metadata).
	Maps *ingest.Maps
	// IconsDir holds the catalog icon files (<db>/icons, FORMAT.md 5); "" or missing: none.
	IconsDir string
	// Web is the viewer bundle served at / (nil: no viewer).
	Web fs.FS

	// ReadToken, if set, is required on every API read (API.md 1.3).
	ReadToken string
	// IngestToken, if set, is required on POST ingest.
	IngestToken string

	// Clock is the map clock (default: wall clock).
	Clock func() core.TimeMs
	// MaxBody bounds an ingest body after decompression (default 32 MiB).
	MaxBody int64
	// IngestSpool bounds the raw ingest bodies held in memory at once (default 64 MiB):
	// a body is read whole before it is applied, so a stalled client never holds the
	// ingester; beyond the budget ingest answers 503. IngestReadTimeout bounds the time
	// to receive one body (default 60 s; then 408).
	IngestSpool       int64
	IngestReadTimeout time.Duration
	// LiveBuffer bounds the queued bytes per live client (default 1 MiB); LiveClients
	// bounds the clients (default 64).
	LiveBuffer  int
	LiveClients int
	// LivePing is the SSE comment interval (default 15 s).
	LivePing time.Duration
	// CommitEvery is the commit loop period (default 1 s). TerrainGrace and MoverGrace
	// are how far behind the clock terrain and movers commit (defaults 10 s and 15 s;
	// movers need at least core.HandoffWindow).
	CommitEvery  time.Duration
	TerrainGrace core.TimeMs
	MoverGrace   core.TimeMs
	// SlowLog, if positive, logs requests that take longer (Logf).
	SlowLog time.Duration
	// MemBudget is reported as tsdb_mem_budget_bytes.
	MemBudget int64
	// Logf receives errors of the commit loop (default: discard).
	Logf func(format string, args ...any)
}

// Server serves the v3 API. Create it with New; it implements http.Handler.
type Server struct {
	cfg     Config
	files   MoverFiles
	hub     *hub
	live    *liveState
	metrics *metrics
	icons   *core.CatalogIcons
	spool   spool
	// ingestWG counts ingest requests in flight (WaitIngest).
	ingestWG sync.WaitGroup
	// builds counts commit steps in which the pyramid built tiles (live tile events).
	builds atomic.Uint64
	// closing is closed by CloseLive: live streams end.
	closing   chan struct{}
	closeOnce sync.Once
	// kick asks the commit loop for a step now (ingest filled the mover open windows).
	kick chan struct{}

	catMu    sync.Mutex
	catBody  []byte
	catETag  string
	catAt    time.Time
	catGen   uint64 // bumped when the catalog must be rebuilt
	catBuilt uint64
	dsMu     sync.Mutex
	dsInfos  map[core.DatastoreKey]core.DatastoreInfo
	dsAt     time.Time
}

// New returns a server for cfg.
func New(cfg Config) (*Server, error) {
	if cfg.Terrain == nil || cfg.Tiles == nil || cfg.Renderer == nil || cfg.Movers == nil || cfg.Overlays == nil {
		return nil, errors.New("api: Terrain, Tiles, Renderer, Movers and Overlays are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = ingest.WallClock
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 32 << 20
	}
	if cfg.IngestSpool <= 0 {
		cfg.IngestSpool = 64 << 20
	}
	if cfg.IngestReadTimeout <= 0 {
		cfg.IngestReadTimeout = 60 * time.Second
	}
	if cfg.LiveBuffer <= 0 {
		cfg.LiveBuffer = 1 << 20
	}
	if cfg.LiveClients <= 0 {
		cfg.LiveClients = 64
	}
	if cfg.LivePing <= 0 {
		cfg.LivePing = 15 * time.Second
	}
	if cfg.CommitEvery <= 0 {
		cfg.CommitEvery = time.Second
	}
	if cfg.TerrainGrace <= 0 {
		cfg.TerrainGrace = 10 * core.Second
	}
	if cfg.MoverGrace <= 0 {
		cfg.MoverGrace = 15 * core.Second
	}
	if cfg.MoverGrace < core.HandoffWindow {
		cfg.MoverGrace = core.HandoffWindow
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Server{cfg: cfg, metrics: newMetrics(), closing: make(chan struct{}), kick: make(chan struct{}, 1)}
	s.spool.max = cfg.IngestSpool
	s.files, _ = cfg.Movers.(MoverFiles)
	s.hub = newHub(cfg.LiveClients, cfg.LiveBuffer)
	s.live = newLiveState()
	s.icons = loadIcons(cfg.IconsDir)
	return s, nil
}

// CloseLive ends every live stream and refuses new ones; call it when the HTTP server
// shuts down (http.Server.RegisterOnShutdown), whose Shutdown otherwise waits for them.
func (s *Server) CloseLive() { s.closeOnce.Do(func() { close(s.closing) }) }

// WaitIngest waits for the ingest requests in flight. After the HTTP server stopped
// (Shutdown returned, or Close was called), call it before closing the stores.
func (s *Server) WaitIngest() { s.ingestWG.Wait() }

// SetIngester sets the ingester POST ingest uses (after New, because the ingester takes
// the server's Notifier).
func (s *Server) SetIngester(in *ingest.Ingester) { s.cfg.Ingest = in }

// Notifier returns the ingest notifier that feeds live events; pass it as
// ingest.Options.Notifier.
func (s *Server) Notifier() ingest.Notifier { return (*notifier)(s) }

// Cache classes (API.md 1.1).
type cacheClass uint8

const (
	cacheNone cacheClass = iota
	cacheImmutable
	cacheShort
	cacheLive
	cacheWeb // viewer files other than assets
)

func (s *Server) cacheControl(c cacheClass) string {
	vis := "public"
	if s.cfg.ReadToken != "" {
		vis = "private"
	}
	switch c {
	case cacheImmutable:
		return vis + ", max-age=31536000, immutable"
	case cacheShort:
		return vis + ", max-age=10"
	case cacheLive:
		return vis + ", max-age=2"
	case cacheWeb:
		return "no-cache"
	}
	return "no-store"
}

// apiError is an error with its HTTP status and code.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, "bad_request", fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &apiError{http.StatusNotFound, "not_found", fmt.Sprintf(format, args...)}
}

// statusOf maps an error to its status and code (API.md 1.2).
func statusOf(err error) (int, string) {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae.status, ae.code
	case errors.Is(err, core.ErrBadTile):
		return http.StatusBadRequest, "bad_tile"
	case errors.Is(err, core.ErrBadKey):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, core.ErrUnknownDatastore), errors.Is(err, core.ErrNoTile), errors.Is(err, core.ErrNoVersion):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, core.ErrOverloaded):
		return http.StatusServiceUnavailable, "overloaded"
	case errors.Is(err, errTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large"
	}
	return http.StatusInternalServerError, "internal"
}

// writeError writes the JSON error body; errors are never cached.
func (s *Server) writeError(w http.ResponseWriter, err error) int {
	status, code := statusOf(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		s.cfg.Logf("api: %v", err)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Encoding")
	h.Del("ETag")
	if status == http.StatusServiceUnavailable {
		h.Set("Retry-After", "2")
	}
	b, _ := json.Marshal(core.ErrorResponse{Error: msg, Code: code})
	w.WriteHeader(status)
	_, _ = w.Write(b)
	return status
}

// writeBody writes an encoded body with its cache class.
func (s *Server) writeBody(w http.ResponseWriter, ctype string, c cacheClass, b tileenc.Body) int {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", s.cacheControl(c))
	h.Set("Vary", "Accept-Encoding")
	if e := b.Encoding.Header(); e != "" {
		h.Set("Content-Encoding", e)
	}
	h.Set("Content-Length", strconv.Itoa(len(b.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Data)
	return http.StatusOK
}

// writeJSON marshals v and writes it compressed per Accept-Encoding. Short-cached
// responses get a weak ETag and answer If-None-Match with 304.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, c cacheClass, v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return s.writeError(w, err)
	}
	return s.writeRawJSON(w, r, c, raw, "")
}

func (s *Server) writeRawJSON(w http.ResponseWriter, r *http.Request, c cacheClass, raw []byte, etag string) int {
	if c == cacheShort || c == cacheLive {
		if etag == "" {
			etag = weakETag(raw)
		}
		w.Header().Set("ETag", etag)
		if etagMatch(r.Header.Get("If-None-Match"), etag) {
			h := w.Header()
			h.Set("Cache-Control", s.cacheControl(c))
			h.Set("Vary", "Accept-Encoding")
			w.WriteHeader(http.StatusNotModified)
			return http.StatusNotModified
		}
	}
	enc := tileenc.Negotiate(r.Header.Get("Accept-Encoding"))
	if len(raw) < 512 {
		enc = tileenc.Identity
	}
	b, err := s.cfg.Renderer.Compress(enc, raw)
	if err != nil {
		return s.writeError(w, err)
	}
	return s.writeBody(w, "application/json", c, b)
}

func weakETag(b []byte) string {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return `W/"` + strconv.FormatUint(h.Sum64(), 36) + `"`
}

// etagMatch implements the weak comparison of If-None-Match.
func etagMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	strip := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	want := strip(etag)
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		if p == "*" || strip(p) == want {
			return true
		}
	}
	return false
}

// tokenOK compares a bearer token (or ?token=) with want in constant time.
func tokenOK(r *http.Request, want string, allowQuery bool) bool {
	if want == "" {
		return true
	}
	got := ""
	if a := r.Header.Get("Authorization"); a != "" {
		if t, ok := strings.CutPrefix(a, "Bearer "); ok {
			got = strings.TrimSpace(t)
		}
	}
	if got == "" && allowQuery {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	path := r.URL.Path
	route := routeOther
	status := 0
	defer func() {
		if rec := recover(); rec != nil {
			s.cfg.Logf("api: panic serving %s: %v", path, rec)
			if status == 0 {
				status = s.writeError(w, fmt.Errorf("internal error"))
			}
		}
		d := time.Since(start)
		s.metrics.observe(route, status, d)
		// The path only: the query may carry the read token.
		if s.cfg.SlowLog > 0 && d > s.cfg.SlowLog && route != routeLive {
			s.cfg.Logf("slow request: %s %s -> %d in %v", r.Method, r.URL.EscapedPath(), status, d.Round(10*time.Microsecond))
		}
	}()
	switch {
	case path == "/healthz":
		route = routeHealth
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		status = http.StatusOK
		_, _ = w.Write([]byte("ok\n"))
	case path == "/metrics":
		route = routeMetrics
		if !tokenOK(r, s.cfg.ReadToken, true) {
			status = s.writeError(w, &apiError{http.StatusUnauthorized, "unauthorized", "missing or wrong read token"})
			return
		}
		status = s.serveMetrics(w)
	case strings.HasPrefix(path, "/api/v3/"):
		rest := path[len("/api/v3/"):]
		seg, tail, _ := strings.Cut(rest, "/")
		route = routeOf(seg)
		w.Header().Set("X-TSDB-Now", strconv.FormatInt(int64(s.cfg.Clock()), 10))
		status = s.serveAPI(w, r, route, seg, tail)
	default:
		route = routeWeb
		status = s.serveWeb(w, r)
	}
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request, route int, seg, tail string) int {
	if route == routeIngest {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			return s.writeError(w, &apiError{http.StatusMethodNotAllowed, "bad_request", "ingest needs POST"})
		}
		if !tokenOK(r, s.cfg.IngestToken, false) {
			return s.writeError(w, &apiError{http.StatusUnauthorized, "unauthorized", "missing or wrong ingest token"})
		}
		return s.serveIngest(w, r)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		return s.writeError(w, &apiError{http.StatusMethodNotAllowed, "bad_request", "method not allowed"})
	}
	if !tokenOK(r, s.cfg.ReadToken, true) {
		return s.writeError(w, &apiError{http.StatusUnauthorized, "unauthorized", "missing or wrong read token"})
	}
	ctx := r.Context()
	var err error
	status := 0
	switch route {
	case routeCatalog:
		if tail != "" || (seg != "catalog" && seg != "catalog.json") {
			err = notFound("no route %s", r.URL.Path)
			break
		}
		status, err = s.serveCatalog(ctx, w, r)
	case routePalette:
		status, err = s.servePalette(ctx, w, r, tail)
	case routeView:
		status, err = s.serveView(ctx, w, r, tail)
	case routeTimeline:
		status, err = s.serveTimeline(ctx, w, r, tail)
	case routeTile:
		status, err = s.serveTile(ctx, w, r, tail)
	case routeTileDelta:
		status, err = s.serveTileDelta(ctx, w, r, tail)
	case routeMovers:
		status, err = s.serveMovers(ctx, w, r, tail)
	case routeOverlays:
		status, err = s.serveOverlays(ctx, w, r, tail)
	case routeIcons:
		status, err = s.serveIcon(ctx, w, r, tail)
	case routeLive:
		if tail != "" {
			err = notFound("no route %s", r.URL.Path)
			break
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			return s.writeError(w, &apiError{http.StatusMethodNotAllowed, "bad_request", "live needs GET"})
		}
		return s.serveLive(w, r)
	default:
		err = notFound("no route %s", r.URL.Path)
	}
	if err != nil {
		return s.writeError(w, err)
	}
	return status
}

// Path parameter parsing.

func parseDS(id string) (core.DatastoreKey, error) {
	k, err := core.ParseDatastoreID(id)
	if err != nil {
		return k, badRequest("bad datastore id %q", truncate(id))
	}
	return k, nil
}

func parseSurface(id string) (core.SurfaceKey, error) {
	k, err := core.ParseSurfaceID(id)
	if err != nil {
		return k, badRequest("bad surface id %q", truncate(id))
	}
	return k, nil
}

func truncate(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

func parseZ(s string) (int8, error) {
	z, err := strconv.ParseInt(s, 10, 8)
	if err != nil || z > 0 || z < core.MinZ {
		return 0, &apiError{http.StatusBadRequest, "bad_tile", fmt.Sprintf("bad level %q", truncate(s))}
	}
	return int8(z), nil
}

func parseCoord(s string) (int32, error) {
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, &apiError{http.StatusBadRequest, "bad_tile", fmt.Sprintf("bad coordinate %q", truncate(s))}
	}
	return int32(v), nil
}

func parseTime(s string) (core.TimeMs, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, badRequest("bad time %q", truncate(s))
	}
	return core.TimeMs(v), nil
}

func parseVersion(s string) (core.Version, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, badRequest("bad version %q", truncate(s))
	}
	return core.Version(v), nil
}

// splitN splits a path tail into exactly n segments.
func splitN(tail string, n int) ([]string, bool) {
	parts := strings.Split(tail, "/")
	if len(parts) != n {
		return nil, false
	}
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
	}
	return parts, true
}

// latest resolves t = "latest" for a datastore: max(now, LastT) (API.md 1).
func (s *Server) resolveT(q string, info core.DatastoreInfo) (core.TimeMs, error) {
	if q == "" || q == "latest" {
		return max(s.cfg.Clock(), info.LastT), nil
	}
	return parseTime(q)
}
