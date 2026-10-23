package api

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

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

// spool bounds the memory of ingest bodies read before they are applied.
type spool struct {
	mu        sync.Mutex
	used, max int64
}

func (s *spool) acquire(n int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used+n > s.max {
		return false
	}
	s.used += n
	return true
}

func (s *spool) release(n int64) {
	s.mu.Lock()
	s.used -= n
	s.mu.Unlock()
}

var errSpoolFull = &apiError{http.StatusServiceUnavailable, "overloaded", "too many ingest bodies in flight"}

// bodyBufs keeps one body buffer for the next request (bodies are read whole before
// they are applied; a forwarder posts one after another). At most maxKeptBody bytes.
var bodyBufs = make(chan []byte, 1)

const maxKeptBody = 8 << 20

func putBodyBuf(b []byte) {
	if cap(b) > maxKeptBody {
		return
	}
	select {
	case bodyBufs <- b[:0]:
	default:
	}
}

// readBody reads a raw (still compressed) request body into memory, reserving its
// buffer in the spool as it grows. It returns the bytes reserved, to release.
func (s *Server) readBody(r io.Reader, contentLength int64) ([]byte, int64, error) {
	var buf []byte
	var held int64
	if contentLength > 0 && contentLength < s.cfg.MaxBody {
		select {
		case b := <-bodyBufs:
			// Only for a body of about its size: the whole buffer is reserved.
			if c := int64(cap(b)); c > contentLength && c <= 2*contentLength+1<<20 && s.spool.acquire(c) {
				buf, held = b, int64(cap(b))
			} else {
				putBodyBuf(b)
			}
		default:
		}
	}
	for {
		if len(buf) == cap(buf) {
			want := max(2*int64(cap(buf)), 64<<10)
			if cap(buf) == 0 && contentLength > 0 {
				want = contentLength + 1 // +1: the EOF read needs no growth
				if contentLength >= 1<<20 {
					// Whole MiB, so the kept buffer fits the next body of about the
					// same size.
					want = (want + 1<<20 - 1) &^ (1<<20 - 1)
				}
			}
			want = min(want, s.cfg.MaxBody+1)
			if want <= int64(cap(buf)) {
				return buf, held, errTooLarge
			}
			if !s.spool.acquire(want - held) {
				return buf, held, errSpoolFull
			}
			held = want
			nb := make([]byte, len(buf), want)
			copy(nb, buf)
			buf = nb
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, held, nil
		}
		if err != nil {
			return buf, held, err
		}
	}
}

// POST ingest (API.md 5). The raw body is read into memory first, under a read
// deadline and the spool budget, and only then applied: the ingester applies one body
// at a time, so a client that stalls mid-body must not hold it.
func (s *Server) serveIngest(w http.ResponseWriter, r *http.Request) int {
	if s.cfg.Ingest == nil {
		return s.writeError(w, notFound("ingest is disabled"))
	}
	s.ingestWG.Add(1)
	defer s.ingestWG.Done()
	if r.ContentLength > s.cfg.MaxBody {
		return s.writeError(w, errTooLarge)
	}
	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	switch enc {
	case "", "identity", "gzip", "zstd":
	default:
		return s.writeError(w, &apiError{http.StatusUnsupportedMediaType, "bad_request", "unsupported Content-Encoding " + truncate(enc)})
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.cfg.IngestReadTimeout))
	raw := http.MaxBytesReader(w, r.Body, s.cfg.MaxBody)
	data, held, err := s.readBody(raw, r.ContentLength)
	defer s.spool.release(held)
	_ = raw.Close()
	if err != nil {
		var mbe *http.MaxBytesError
		var ae *apiError
		switch {
		case errors.As(err, &mbe), errors.Is(err, errTooLarge):
			err = errTooLarge
		case errors.As(err, &ae):
		case errors.Is(err, os.ErrDeadlineExceeded):
			err = &apiError{http.StatusRequestTimeout, "timeout", "ingest body not received in time"}
		default:
			err = badRequest("reading body: %v", err)
		}
		return s.writeError(w, err)
	}
	_ = rc.SetReadDeadline(time.Time{})
	if enc == "" || enc == "identity" {
		// Applied in place: the ingester parses the lines inside data.
		resp, err := s.cfg.Ingest.IngestBytes(r.Context(), data)
		putBodyBuf(data)
		if err != nil {
			return s.writeError(w, err)
		}
		w.Header().Set("Cache-Control", "no-store")
		return s.writeJSON(w, r, cacheNone, resp)
	}
	var body io.Reader = bytes.NewReader(data)
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return s.writeError(w, badRequest("gzip body: %v", err))
		}
		defer zr.Close()
		body = zr
	case "zstd":
		zr, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(8<<20), zstd.WithDecoderMaxMemory(uint64(s.cfg.MaxBody)+(1<<20)))
		if err != nil {
			return s.writeError(w, badRequest("zstd body: %v", err))
		}
		defer zr.Close()
		body = zr
	}
	resp, err := s.cfg.Ingest.Ingest(r.Context(), &capReader{r: body, n: s.cfg.MaxBody})
	if err != nil {
		return s.writeError(w, err)
	}
	w.Header().Set("Cache-Control", "no-store")
	return s.writeJSON(w, r, cacheNone, resp)
}
