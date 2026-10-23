package publish

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
)

// ErrNotFound is returned by a Source for a path that does not exist (HTTP 404).
var ErrNotFound = errors.New("not found")

// Resource is one API response body, without Content-Encoding.
type Resource struct {
	Body        []byte
	ContentType string
}

// Source reads API resources by their path relative to the API base ("catalog",
// "timeline/<ds>/0/2026-10-12.json", ...), the same paths the static export writes
// (docs/v3/API.md 6).
type Source interface {
	Get(ctx context.Context, path string) (Resource, error)
}

// OverlayLister is implemented by sources that know which z0 tiles hold overlay
// intervals (an opened database). Other sources publish the overlays of every z0 tile
// with terrain that has at least one interval.
type OverlayLister interface {
	OverlayTiles(ctx context.Context, ds string) ([]core.TileCoord, error)
}

// HandlerSource reads from an http.Handler in the same process (the v3 API server
// over an opened database), without a network.
type HandlerSource struct {
	Handler http.Handler
	// Base is the API path prefix, default "/api/v3/".
	Base string
	// ReadToken, if set, is sent as a bearer token.
	ReadToken string
	// Overlays, if set, lists overlay tiles (OverlayLister).
	Overlays func(ctx context.Context, ds string) ([]core.TileCoord, error)
}

// Get implements Source.
func (h *HandlerSource) Get(ctx context.Context, path string) (Resource, error) {
	base := h.Base
	if base == "" {
		base = "/api/v3/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://publish.local"+base+path, nil)
	if err != nil {
		return Resource{}, err
	}
	if h.ReadToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.ReadToken)
	}
	rec := &recorder{header: http.Header{}}
	h.Handler.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return response(path, rec.status, rec.header, rec.body.Bytes())
}

// OverlayTiles implements OverlayLister when Overlays is set.
func (h *HandlerSource) OverlayTiles(ctx context.Context, ds string) ([]core.TileCoord, error) {
	if h.Overlays == nil {
		return nil, errors.ErrUnsupported
	}
	return h.Overlays(ctx, ds)
}

// recorder is a minimal http.ResponseWriter.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(s int) {
	if r.status == 0 {
		r.status = s
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// HTTPSource reads from a running server (serve-v3, or the Clusterio proxy in front of
// it), so a database in use is published without opening it twice.
type HTTPSource struct {
	// Base is the API base URL, for example http://127.0.0.1:8080/api/v3/.
	Base      string
	ReadToken string
	// Client defaults to one with a 60 s timeout that keeps enough idle connections for
	// the publisher's workers (the default transport keeps 2 per host, and the closed
	// ones can exhaust the ephemeral ports on Windows).
	Client *http.Client

	once   sync.Once
	client *http.Client
}

// Get implements Source. Network errors and 5xx answers are retried twice.
func (h *HTTPSource) Get(ctx context.Context, path string) (Resource, error) {
	h.once.Do(func() {
		h.client = h.Client
		if h.client == nil {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.MaxIdleConns = 64
			tr.MaxIdleConnsPerHost = 64
			h.client = &http.Client{Timeout: 60 * time.Second, Transport: tr}
		}
	})
	client := h.client
	base := h.Base
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Resource{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return Resource{}, err
		}
		// Set explicitly, so the transport hands over the encoded body unchanged.
		req.Header.Set("Accept-Encoding", "zstd, gzip")
		if h.ReadToken != "" {
			req.Header.Set("Authorization", "Bearer "+h.ReadToken)
		}
		res, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, maxRawResponse+1))
		res.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if len(body) > maxRawResponse {
			// Never store a truncated object: the item fails (and is tried again next round).
			return Resource{}, fmt.Errorf("get %s: response over %d bytes", path, maxRawResponse)
		}
		if res.StatusCode >= 500 {
			last = fmt.Errorf("get %s: status %d", path, res.StatusCode)
			continue
		}
		return response(path, res.StatusCode, res.Header, body)
	}
	return Resource{}, last
}

// response turns a status, headers and raw body into a decoded Resource.
func response(path string, status int, h http.Header, body []byte) (Resource, error) {
	if status == http.StatusNotFound {
		return Resource{}, fmt.Errorf("get %s: %w", path, ErrNotFound)
	}
	if status != http.StatusOK {
		msg := body
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return Resource{}, fmt.Errorf("get %s: status %d: %s", path, status, bytes.TrimSpace(msg))
	}
	raw, err := decode(h.Get("Content-Encoding"), body)
	if err != nil {
		return Resource{}, fmt.Errorf("get %s: %w", path, err)
	}
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	return Resource{Body: raw, ContentType: ct}, nil
}

// maxRawResponse and maxDecoded bound one response as sent and once its Content-Encoding is removed. A response past
// them fails its item; it is never stored truncated.
const (
	maxRawResponse = 64 << 20
	maxDecoded     = 256 << 20
)

var zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(maxDecoded))

// decode removes a Content-Encoding.
func decode(enc string, b []byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "", "identity":
		return b, nil
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(io.LimitReader(r, maxDecoded+1))
		if err == nil && len(out) > maxDecoded {
			err = fmt.Errorf("body over %d bytes decoded", maxDecoded)
		}
		return out, err
	case "zstd":
		return zdec.DecodeAll(b, nil)
	}
	return nil, fmt.Errorf("unknown Content-Encoding %q", enc)
}
