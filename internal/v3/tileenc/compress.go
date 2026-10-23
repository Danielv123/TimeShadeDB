package tileenc

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// Encoding is an HTTP Content-Encoding the server sends (docs/v3/API.md "Compression").
type Encoding uint8

// Encodings, in increasing order of preference.
const (
	Identity Encoding = iota
	Gzip
	Zstd
)

// String returns the Content-Encoding token ("identity", "gzip" or "zstd").
func (e Encoding) String() string {
	switch e {
	case Gzip:
		return "gzip"
	case Zstd:
		return "zstd"
	}
	return "identity"
}

// Header returns the Content-Encoding header value: "" for identity.
func (e Encoding) Header() string {
	if e == Identity {
		return ""
	}
	return e.String()
}

// Negotiate picks the encoding for an Accept-Encoding header: zstd when the client
// accepts it, else gzip, else identity (API.md). Tokens with q=0 are refused, and "*"
// accepts any encoding not listed explicitly.
func Negotiate(acceptEncoding string) Encoding {
	if acceptEncoding == "" {
		return Identity
	}
	zs, gz, star := -1.0, -1.0, -1.0
	for _, part := range strings.Split(acceptEncoding, ",") {
		tok, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		switch strings.ToLower(strings.TrimSpace(tok)) {
		case "zstd":
			zs = q
		case "gzip", "x-gzip":
			gz = q
		case "*":
			star = q
		}
	}
	if zs < 0 {
		zs = star
	}
	if gz < 0 {
		gz = star
	}
	if zs > 0 {
		return Zstd
	}
	if gz > 0 {
		return Gzip
	}
	return Identity
}

// Compressor compresses response bodies. It is safe for concurrent use.
type Compressor struct {
	zenc      *zstd.Encoder
	zstdLevel zstd.EncoderLevel
	gzipLevel int
	gzPool    sync.Pool
}

// CompressorOptions configures a Compressor. Zero values pick the defaults.
type CompressorOptions struct {
	// ZstdLevel defaults to zstd.SpeedDefault (about level 3).
	ZstdLevel zstd.EncoderLevel
	// GzipLevel defaults to gzip.BestSpeed. Encoded bodies are cached, so a higher level
	// trades first-request latency for bytes.
	GzipLevel int
}

// NewCompressor returns a compressor. zstd frames use a window of at most 1 MiB, well
// under the 8 MiB browsers accept for Content-Encoding: zstd.
func NewCompressor(opt CompressorOptions) (*Compressor, error) {
	if opt.ZstdLevel == 0 {
		opt.ZstdLevel = zstd.SpeedDefault
	}
	if opt.GzipLevel == 0 {
		opt.GzipLevel = gzip.BestSpeed
	}
	zenc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(opt.ZstdLevel),
		zstd.WithEncoderConcurrency(min(runtime.GOMAXPROCS(0), 8)),
		zstd.WithWindowSize(1<<20),
		zstd.WithLowerEncoderMem(true),
	)
	if err != nil {
		return nil, err
	}
	if _, err := gzip.NewWriterLevel(io.Discard, opt.GzipLevel); err != nil {
		return nil, err
	}
	return &Compressor{zenc: zenc, zstdLevel: opt.ZstdLevel, gzipLevel: opt.GzipLevel}, nil
}

// Compress returns raw encoded with e. For Identity it returns raw itself.
func (c *Compressor) Compress(e Encoding, raw []byte) ([]byte, error) {
	switch e {
	case Identity:
		return raw, nil
	case Zstd:
		return c.zenc.EncodeAll(raw, make([]byte, 0, len(raw)/4+64)), nil
	case Gzip:
		var buf bytes.Buffer
		buf.Grow(len(raw)/4 + 64)
		w, _ := c.gzPool.Get().(*gzip.Writer)
		if w == nil {
			w, _ = gzip.NewWriterLevel(&buf, c.gzipLevel)
		} else {
			w.Reset(&buf)
		}
		if _, err := w.Write(raw); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		c.gzPool.Put(w)
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("tileenc: unknown encoding %d", e)
}

// Close releases the zstd encoder.
func (c *Compressor) Close() error { return c.zenc.Close() }
