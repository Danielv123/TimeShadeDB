package publish

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// NewStaticHandler serves a directory export the way the S3 target stores it: files
// whose bytes start with the gzip or zstd magic are sent with that Content-Encoding
// (no export file starts with those bytes otherwise), Content-Type and Cache-Control
// follow the key (docs/v3/API.md 6; timelines and day indexes always get the short
// class, since the file does not say whether it is final). Dot files (the publish state)
// are not served. It is meant for testing an export and small deployments; a CDN or
// S3 bucket serves the same objects with the metadata the S3 target stores.
func NewStaticHandler(root string) http.Handler {
	return &staticHandler{root: root}
}

type staticHandler struct{ root string }

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if key == "" {
		key = "index.html"
	}
	if !validKey(key) || strings.HasPrefix(key, ".") || strings.Contains(key, "/.") {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(h.root, filepath.FromSlash(key)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hd := w.Header()
	switch {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		hd.Set("Content-Encoding", "gzip")
	case bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		hd.Set("Content-Encoding", "zstd")
	}
	hd.Set("Content-Type", keyContentType(key))
	hd.Set("Cache-Control", keyCacheControl(key))
	hd.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

// keyContentType is the Content-Type of an exported key.
func keyContentType(key string) string {
	switch {
	case strings.HasSuffix(key, ".bin"):
		return "application/octet-stream"
	case strings.HasSuffix(key, ".json"):
		return "application/json"
	}
	return webContentType(key)
}

// keyCacheControl is the Cache-Control of an exported key (API.md 6).
func keyCacheControl(key string) string {
	switch {
	case key == "catalog.json":
		return CacheCatalog
	case key == "index.html":
		return CachePage
	case strings.HasPrefix(key, "overlays/"):
		return CacheOverlays
	case strings.HasPrefix(key, "movers/") && strings.HasSuffix(key, "/live.bin"):
		return CacheLive
	case strings.HasPrefix(key, "timeline/"), strings.HasPrefix(key, "movers/") && strings.Contains(key, "/day/"):
		return CacheOpen
	case strings.HasPrefix(key, "tile/"), strings.HasPrefix(key, "tiledelta/"), strings.HasPrefix(key, "palette/"),
		strings.HasPrefix(key, "movers/"), strings.HasPrefix(key, "icons/"), strings.HasPrefix(key, "assets/"):
		return CacheImmutable
	}
	return CachePage
}
