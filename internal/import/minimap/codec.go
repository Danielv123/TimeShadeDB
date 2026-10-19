// Ported from illisis-minimap shared/codec.ts and shared/keys.ts.
// Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package minimap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/klauspost/compress/gzip"
)

// Format constants (illisis shared/keys.ts, shared/codec.ts).
const (
	// AreaSize is an area's side in pixels at every level.
	AreaSize = 512
	// BlockPixels is the number of pixels in a block (a chunk at z0).
	BlockPixels = 1024
	// SnapBytes is the size of a decoded MMS1 snapshot.
	SnapBytes = 4 + 2*AreaSize*AreaSize
	// Levels is the number of illisis levels (z = 0..4).
	Levels = 5
)

// Size caps after decompression. The largest files in the test data are a 1.4 MB
// change file and 0.5 MB snapshots; the caps leave a wide margin while bounding what a
// hostile file can make the importer allocate.
const (
	maxChangeBytes = 64 << 20
	maxJSONBytes   = 64 << 20
)

var (
	// ErrFormat is returned (wrapped) for files that do not follow the format.
	ErrFormat = errors.New("minimap: bad format")
	// ErrTooLarge is returned (wrapped) for files over the size cap.
	ErrTooLarge = errors.New("minimap: file too large")
)

var (
	magicChange = []byte("MMC1")
	magicSnap   = []byte("MMS1")
)

var gzPool sync.Pool

// readGz reads and gunzips a file, failing when the result exceeds limit bytes.
func readGz(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var zr *gzip.Reader
	if v := gzPool.Get(); v != nil {
		zr = v.(*gzip.Reader)
		err = zr.Reset(f)
	} else {
		zr, err = gzip.NewReader(f)
	}
	if err != nil {
		if zr != nil {
			gzPool.Put(zr)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer gzPool.Put(zr)
	var buf bytes.Buffer
	if st, err := f.Stat(); err == nil && st.Size() > 0 && st.Size() < limit/4 {
		buf.Grow(int(st.Size()) * 3)
	}
	n, err := io.Copy(&buf, io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if n > limit {
		return nil, fmt.Errorf("%s: %w (over %d bytes)", path, ErrTooLarge, limit)
	}
	return buf.Bytes(), nil
}

// Rec is one MMC1 record: block's pixels at Pos took the colours Col at time T. Pos is
// nil for a whole-block record (Col then holds all 1024 pixels, row-major).
type Rec struct {
	T     int64
	Block uint8
	Pos   []uint16
	Col   []uint16
}

// walkChange calls fn for every record of an MMC1 file named t0. data holds the
// record's raw little-endian payload: n x (u16 pos, u16 colour), or 1024 x u16 colour
// when n == 0. It validates the layout and positions before calling fn.
func walkChange(t0 int64, b []byte, fn func(t int64, block uint8, n int, data []byte) error) error {
	if len(b) < 4 || !bytes.Equal(b[:4], magicChange) {
		return fmt.Errorf("%w: not a change file (MMC1)", ErrFormat)
	}
	for o := 4; o < len(b); {
		if o+7 > len(b) {
			return fmt.Errorf("%w: change file truncated at %d", ErrFormat, o)
		}
		dt := binary.LittleEndian.Uint32(b[o:])
		block := b[o+4]
		n := int(binary.LittleEndian.Uint16(b[o+5:]))
		o += 7
		size := 4 * n
		if n == 0 {
			size = 2 * BlockPixels
		}
		if o+size > len(b) {
			return fmt.Errorf("%w: change file truncated at %d", ErrFormat, o)
		}
		data := b[o : o+size]
		if n > 0 {
			for i := 0; i < n; i++ {
				if binary.LittleEndian.Uint16(data[4*i:]) >= BlockPixels {
					return fmt.Errorf("%w: change file position out of range at %d", ErrFormat, o)
				}
			}
		}
		if err := fn(t0+int64(dt), block, n, data); err != nil {
			return err
		}
		o += size
	}
	return nil
}

// DecodeChange decodes an MMC1 change file whose name (first record time) is t0.
func DecodeChange(t0 int64, b []byte) ([]Rec, error) {
	var out []Rec
	err := walkChange(t0, b, func(t int64, block uint8, n int, data []byte) error {
		r := Rec{T: t, Block: block}
		if n == 0 {
			r.Col = make([]uint16, BlockPixels)
			for i := range r.Col {
				r.Col[i] = binary.LittleEndian.Uint16(data[2*i:])
			}
		} else {
			r.Pos, r.Col = make([]uint16, n), make([]uint16, n)
			for i := 0; i < n; i++ {
				r.Pos[i] = binary.LittleEndian.Uint16(data[4*i:])
				r.Col[i] = binary.LittleEndian.Uint16(data[4*i+2:])
			}
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// EncodeChange encodes records as an MMC1 file named t0 (records with nil Pos are
// whole blocks). It exists for tests and tools; the importer only decodes.
func EncodeChange(t0 int64, recs []Rec) []byte {
	out := append([]byte(nil), magicChange...)
	for _, r := range recs {
		var hdr [7]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(r.T-t0))
		hdr[4] = r.Block
		if r.Pos != nil {
			binary.LittleEndian.PutUint16(hdr[5:], uint16(len(r.Pos)))
		}
		out = append(out, hdr[:]...)
		for i, c := range r.Col {
			if r.Pos != nil {
				out = binary.LittleEndian.AppendUint16(out, r.Pos[i])
			}
			out = binary.LittleEndian.AppendUint16(out, c)
		}
	}
	return out
}

// applyRecord writes one record's colours into a block.
func applyRecord(px *[BlockPixels]uint16, n int, data []byte) {
	if n == 0 {
		for i := range px {
			px[i] = binary.LittleEndian.Uint16(data[2*i:])
		}
		return
	}
	for i := 0; i < n; i++ {
		px[binary.LittleEndian.Uint16(data[4*i:])] = binary.LittleEndian.Uint16(data[4*i+2:])
	}
}

// DecodeSnap decodes an MMS1 snapshot into 512 x 512 RGB565 pixels, row-major.
func DecodeSnap(b []byte) ([]uint16, error) {
	if len(b) != SnapBytes || !bytes.Equal(b[:4], magicSnap) {
		return nil, fmt.Errorf("%w: not a snapshot (MMS1, %d bytes)", ErrFormat, SnapBytes)
	}
	px := make([]uint16, AreaSize*AreaSize)
	for i := range px {
		px[i] = binary.LittleEndian.Uint16(b[4+2*i:])
	}
	return px, nil
}

// EncodeSnap encodes 512 x 512 pixels as MMS1 (tests and tools).
func EncodeSnap(px []uint16) []byte {
	out := make([]byte, 4, SnapBytes)
	copy(out, magicSnap)
	for _, v := range px {
		out = binary.LittleEndian.AppendUint16(out, v)
	}
	return out
}

// areaPixel returns the area pixel index of block's pixel pos (codec.ts at()).
func areaPixel(block uint8, pos int) int {
	return ((int(block>>4)*32)+(pos>>5))*AreaSize + int(block&15)*32 + (pos & 31)
}

// applyArea writes one record into an area image (replay.ts / codec.ts apply()).
func applyArea(px []uint16, block uint8, n int, data []byte) {
	if n == 0 {
		for p := 0; p < BlockPixels; p++ {
			px[areaPixel(block, p)] = binary.LittleEndian.Uint16(data[2*p:])
		}
		return
	}
	for i := 0; i < n; i++ {
		px[areaPixel(block, int(binary.LittleEndian.Uint16(data[4*i:])))] = binary.LittleEndian.Uint16(data[4*i+2:])
	}
}

// Unname is the inverse of illisis keys.ts name(): "~XX" is one byte of the UTF-8
// encoding; every other character stands for itself.
func Unname(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: empty name", ErrFormat)
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '~' {
			out = append(out, c)
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("%w: truncated escape in %q", ErrFormat, s)
		}
		hi, ok1 := hexVal(s[i+1])
		lo, ok2 := hexVal(s[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("%w: bad escape in %q", ErrFormat, s)
		}
		out = append(out, hi<<4|lo)
		i += 2
	}
	return string(out), nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}
