package api

import (
	"context"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"timeshadedb/internal/v3/tileenc"
)

// serveWeb serves the embedded viewer: index.html for / (revalidated on every load, so
// a new binary takes effect), content-hashed assets/* as immutable, other files with a
// short cache. Text files are compressed per Accept-Encoding through the encoded cache
// (the bundle never changes while the binary runs).
func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request) int {
	if s.cfg.Web == nil {
		return s.writeError(w, notFound("no viewer"))
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		return s.writeError(w, &apiError{http.StatusMethodNotAllowed, "bad_request", "method not allowed"})
	}
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if !fs.ValidPath(p) {
		return s.writeError(w, notFound("no file %s", truncate(p)))
	}
	fi, err := fs.Stat(s.cfg.Web, p)
	if err != nil || fi.IsDir() {
		return s.writeError(w, notFound("no file %s", truncate(p)))
	}
	ctype := webTypes[path.Ext(p)]
	if ctype == "" {
		ctype = mime.TypeByExtension(path.Ext(p))
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	enc := tileenc.Identity
	if strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript") || strings.Contains(ctype, "json") || strings.Contains(ctype, "svg") {
		enc = tileenc.Negotiate(r.Header.Get("Accept-Encoding"))
	}
	b, err := s.cfg.Renderer.Cached(r.Context(), "web/"+p, enc, func(context.Context) ([]byte, error) {
		return fs.ReadFile(s.cfg.Web, p)
	})
	if err != nil {
		return s.writeError(w, err)
	}
	c := cacheWeb
	if strings.HasPrefix(p, "assets/") {
		c = cacheImmutable
	}
	status := s.writeBody(w, ctype, c, b)
	return status
}

// webTypes fixes the types of the bundle's files: mime.TypeByExtension reads the
// Windows registry, which can map .js to text/plain and break module scripts.
var webTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".wasm":  "application/wasm",
	".ico":   "image/x-icon",
	".map":   "application/json",
	".txt":   "text/plain; charset=utf-8",
	".woff2": "font/woff2",
}
