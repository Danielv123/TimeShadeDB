package api

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

var errTooLarge = errors.New("body over the ingest cap")

// capReader fails with errTooLarge once more than n bytes were read.
type capReader struct {
	r io.Reader
	n int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n < 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > c.n+1 {
		p = p[:c.n+1]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	if c.n < 0 {
		return n, errTooLarge
	}
	return n, err
}

// POST ingest (API.md 5).
func (s *Server) serveIngest(w http.ResponseWriter, r *http.Request) int {
	if s.cfg.Ingest == nil {
		return s.writeError(w, notFound("ingest is disabled"))
	}
	if r.ContentLength > s.cfg.MaxBody {
		return s.writeError(w, errTooLarge)
	}
	// The raw body is capped too, so a client cannot stream forever.
	raw := http.MaxBytesReader(w, r.Body, s.cfg.MaxBody)
	defer raw.Close()
	var body io.Reader = raw
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(raw)
		if err != nil {
			return s.writeError(w, badRequest("gzip body: %v", err))
		}
		defer zr.Close()
		body = zr
	case "zstd":
		zr, err := zstd.NewReader(raw, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(8<<20), zstd.WithDecoderMaxMemory(uint64(s.cfg.MaxBody)+(1<<20)))
		if err != nil {
			return s.writeError(w, badRequest("zstd body: %v", err))
		}
		defer zr.Close()
		body = zr
	default:
		return s.writeError(w, &apiError{http.StatusUnsupportedMediaType, "bad_request", "unsupported Content-Encoding " + truncate(enc)})
	}
	resp, err := s.cfg.Ingest.Ingest(r.Context(), &capReader{r: body, n: s.cfg.MaxBody})
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			err = errTooLarge
		}
		return s.writeError(w, err)
	}
	w.Header().Set("Cache-Control", "no-store")
	return s.writeJSON(w, r, cacheNone, resp)
}
