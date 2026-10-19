package benchlib

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"timeshadedb/internal/v3/core"
)

// TSVReader reads chunk rows from a TimeShadeDB chunk TSV file in the 5-field form
// `measure stream` (WriteIllisisStream) writes:
//
//	t \t surface \t cx,cy \t force \t hex(1024 x RGB565 little-endian)
//
// The 6-field form (t, surface, cx, cy, force, hex) is accepted too. The map key is not
// in the file; it is the file name up to ".chunks.tsv" unless given.
type TSVReader struct {
	Map  string
	sc   *bufio.Scanner
	f    io.Closer
	line int
}

// OpenTSV opens a chunk TSV. mapKey "" derives it from the file name.
func OpenTSV(path, mapKey string) (*TSVReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if mapKey == "" {
		mapKey = MapKeyFromStreamFile(path)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	return &TSVReader{Map: mapKey, sc: sc, f: f}, nil
}

// MapKeyFromStreamFile returns "gw-bc41437d.1" for ".../gw-bc41437d~2E1.chunks.tsv"
// (stream files keep the illisis escaped name).
func MapKeyFromStreamFile(path string) string {
	base := filepath.Base(path)
	for _, suf := range []string{".chunks.tsv", ".entities.tsv", ".tsv"} {
		if strings.HasSuffix(base, suf) {
			base = strings.TrimSuffix(base, suf)
			break
		}
	}
	if s, err := unescapeIllisis(base); err == nil {
		return s
	}
	return base
}

// unescapeIllisis undoes illisis keys.ts name(): "~XX" is one byte.
func unescapeIllisis(s string) (string, error) {
	if !strings.Contains(s, "~") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '~' && i+2 < len(s) {
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", err
			}
			b.WriteByte(byte(v))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

// Close closes the file.
func (r *TSVReader) Close() error { return r.f.Close() }

// Next returns the next row, or io.EOF.
func (r *TSVReader) Next() (ChunkRow, error) {
	for r.sc.Scan() {
		r.line++
		line := strings.TrimRight(r.sc.Text(), "\r\n")
		if line == "" {
			continue
		}
		row, err := ParseTSVRow(r.Map, line)
		if err != nil {
			return ChunkRow{}, fmt.Errorf("line %d: %w", r.line, err)
		}
		return row, nil
	}
	if err := r.sc.Err(); err != nil {
		return ChunkRow{}, err
	}
	return ChunkRow{}, io.EOF
}

// ParseTSVRow parses one chunk TSV row.
func ParseTSVRow(mapKey, line string) (ChunkRow, error) {
	f := strings.Split(line, "\t")
	var row ChunkRow
	row.Map = mapKey
	var hx string
	switch len(f) {
	case 5:
		xs, ys, ok := strings.Cut(f[2], ",")
		if !ok {
			return row, fmt.Errorf("bad chunk coordinate %q", f[2])
		}
		cx, err1 := strconv.ParseInt(xs, 10, 32)
		cy, err2 := strconv.ParseInt(ys, 10, 32)
		if err1 != nil || err2 != nil {
			return row, fmt.Errorf("bad chunk coordinate %q", f[2])
		}
		row.CX, row.CY, row.Force, hx = int32(cx), int32(cy), f[3], f[4]
	case 6:
		cx, err1 := strconv.ParseInt(f[2], 10, 32)
		cy, err2 := strconv.ParseInt(f[3], 10, 32)
		if err1 != nil || err2 != nil {
			return row, fmt.Errorf("bad chunk coordinate %q,%q", f[2], f[3])
		}
		row.CX, row.CY, row.Force, hx = int32(cx), int32(cy), f[4], f[5]
	default:
		return row, fmt.Errorf("expected 5 or 6 fields, got %d", len(f))
	}
	t, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return row, fmt.Errorf("bad time %q", f[0])
	}
	row.T, row.Surface = t, f[1]
	if hx == "" {
		row.Deleted = true
		return row, nil
	}
	if len(hx) != 4*core.ChunkPixelCount {
		return row, fmt.Errorf("payload has %d hex chars, want %d", len(hx), 4*core.ChunkPixelCount)
	}
	var raw [2 * core.ChunkPixelCount]byte
	if _, err := hex.Decode(raw[:], []byte(hx)); err != nil {
		return row, err
	}
	px := new(core.ChunkPixels)
	for i := range px {
		px[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	row.Pixels = px
	return row, nil
}

// ReadAllTSV reads every row of the given files, filtered by surface ("" = all).
func ReadAllTSV(paths []string, surface string) ([]ChunkRow, error) {
	var out []ChunkRow
	for _, p := range paths {
		r, err := OpenTSV(p, "")
		if err != nil {
			return nil, err
		}
		for {
			row, err := r.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				r.Close()
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			if surface == "" || row.Surface == surface {
				out = append(out, row)
			}
		}
		r.Close()
	}
	return out, nil
}

// ChunkHours returns the number of distinct (map, surface, force, chunk) keys in rows
// and the time span in hours, whose product is the "chunk-hour" denominator of the
// terrain disk target (PLAN.md §4.2: charted chunks x hours covered).
func ChunkHours(rows []ChunkRow) (chunks int, hours float64) {
	type key struct {
		m, s, f string
		x, y    int32
	}
	seen := map[key]struct{}{}
	var lo, hi int64
	for i, r := range rows {
		seen[key{r.Map, r.Surface, r.Force, r.CX, r.CY}] = struct{}{}
		if i == 0 || r.T < lo {
			lo = r.T
		}
		if i == 0 || r.T > hi {
			hi = r.T
		}
	}
	return len(seen), float64(hi-lo) / 3.6e6
}
