package benchlib

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"sync"

	"github.com/klauspost/compress/zlib"

	"timeshadedb/internal/v3/core"
)

// chartEncoder encodes charts in the core.EncodeChart format (base64 of zlib-deflated
// little-endian RGB565) with a reused zlib writer. core.EncodeChart allocates a new
// writer, about 1 MB, per chart; at benchmark rates that made the client's encoders
// use most of the machine's cores and its garbage collector, competing with the server
// under test, and still left the senders waiting for bodies.
type chartEncoder struct {
	raw [2 * core.ChunkPixelCount]byte
	buf bytes.Buffer
	zw  *zlib.Writer
}

var chartEncPool = sync.Pool{New: func() any {
	e := &chartEncoder{}
	e.zw, _ = zlib.NewWriterLevel(&e.buf, zlib.DefaultCompression)
	return e
}}

// encodeChart returns core.EncodeChart(px) (the same format; the deflate stream may
// differ byte for byte, which the protocol allows).
func encodeChart(px *core.ChunkPixels) string {
	e := chartEncPool.Get().(*chartEncoder)
	defer chartEncPool.Put(e)
	for i, v := range px {
		binary.LittleEndian.PutUint16(e.raw[2*i:], v)
	}
	e.buf.Reset()
	e.zw.Reset(&e.buf)
	_, _ = e.zw.Write(e.raw[:])
	_ = e.zw.Close()
	return base64.StdEncoding.EncodeToString(e.buf.Bytes())
}
