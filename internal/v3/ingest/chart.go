package ingest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zlib"

	"timeshadedb/internal/v3/core"
)

// chartDecoder decodes chunk charts (core.DecodeChart semantics) without allocating per
// chart: a fresh zlib reader per chart allocates its whole flate state, which at ingest
// rates makes the parallel decoders contend on the heap.
type chartDecoder struct {
	comp []byte
	br   bytes.Reader
	zr   io.ReadCloser
	raw  [2*core.ChunkPixelCount + 1]byte
}

var chartPool = sync.Pool{New: func() any { return &chartDecoder{} }}

// decodeChart decodes d into px. Like core.DecodeChart it rejects charts that do not
// inflate to exactly 2,048 bytes (or whose zlib checksum fails).
func decodeChart(d string, px *core.ChunkPixels) error {
	return decodeChartBytes([]byte(d), px)
}

// decodeChartBytes is decodeChart on the chart's text as bytes (not retained).
func decodeChartBytes(d []byte, px *core.ChunkPixels) error {
	cd := chartPool.Get().(*chartDecoder)
	defer chartPool.Put(cd)
	if n := base64.StdEncoding.DecodedLen(len(d)); cap(cd.comp) < n {
		cd.comp = make([]byte, n)
	}
	n, err := base64.StdEncoding.Decode(cd.comp[:cap(cd.comp)], d)
	if err != nil {
		return fmt.Errorf("chart base64: %w", err)
	}
	cd.br.Reset(cd.comp[:n])
	if cd.zr == nil {
		if cd.zr, err = zlib.NewReader(&cd.br); err != nil {
			cd.zr = nil
			return fmt.Errorf("chart zlib: %w", err)
		}
	} else if err := cd.zr.(zlib.Resetter).Reset(&cd.br, nil); err != nil {
		return fmt.Errorf("chart zlib: %w", err)
	}
	got, err := io.ReadFull(cd.zr, cd.raw[:])
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), err == nil:
	case errors.Is(err, io.EOF):
		got = 0
	default:
		return fmt.Errorf("chart inflate: %w", err)
	}
	if got != 2*core.ChunkPixelCount {
		return fmt.Errorf("chart inflates to %d bytes, want %d", got, 2*core.ChunkPixelCount)
	}
	for i := range px {
		px[i] = binary.LittleEndian.Uint16(cd.raw[2*i:])
	}
	return nil
}
