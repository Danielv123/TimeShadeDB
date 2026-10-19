package benchlib

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file turns the illisis/minimap static export (the test data) into the chunk TSV
// stream the tools replay: one row per chunk per coalescing window, holding the chunk's
// full chart at the end of the window, stamped with the time of the window's last record,
// and only when the chart differs from the window's start. It is a Go port of the
// chunks.tsv output of the research script to_stream.py (same rows, same order); the
// export formats are illisis shared/keys.ts (key tree, name escaping, area and block
// maths) and shared/codec.ts (MMC1 change files). The export is only parsed, never run.

// StreamOptions configure WriteIllisisStream.
type StreamOptions struct {
	// CoalesceMs is the window length; 0 writes every record that changes a chunk
	// (nauvis alone has 7.67 M records, about 31 GB of TSV).
	CoalesceMs int64
	// Map and Surface restrict the output (map key as in the export, surface name).
	Map, Surface string
	// DropEnemy ignores pixel writes of the enemy colour 0xF8C3.
	DropEnemy bool
}

// StreamFile is one written file and its row count.
type StreamFile struct {
	Map  string
	Path string
	Rows int
}

const enemyColour = 0xF8C3

var (
	illAreaIndexZ0 = regexp.MustCompile(`^([^/]+)/([^/]+)/([^/]+)/0/(-?\d+)_(-?\d+)/index\.json\.gz$`)
	illMoveWindow  = regexp.MustCompile(`^([^/]+)/([^/]+)/m/\d+\.m\.gz$`)
)

// UnnameIllisis inverts illisis keys.ts name(): "~XX" (and "%XX") is one UTF-8 byte.
func UnnameIllisis(s string) string {
	if !strings.ContainsAny(s, "~%") {
		return s
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == '~' || c == '%') && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b = append(b, byte(v))
				i += 2
				continue
			}
		}
		b = append(b, c)
	}
	return string(b)
}

type illStreamArea struct {
	mapKey, surfaceKey, forceKey string
	ax, ay                       int
}

// WriteIllisisStream writes <outDir>/<map>.chunks.tsv for every map of the export at root
// (the directory holding maps.json.gz). Rows are "t \t surface \t cx,cy \t force \t hex"
// with the chart as 1,024 little-endian RGB565 values, sorted by time.
func WriteIllisisStream(root, outDir string, o StreamOptions) ([]StreamFile, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, "maps.json.gz")); err != nil {
		return nil, fmt.Errorf("%s does not look like an illisis export (no maps.json.gz): %w", root, err)
	}
	var areas []illStreamArea
	maps := map[string]bool{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		key := filepath.ToSlash(rel)
		if m := illAreaIndexZ0.FindStringSubmatch(key); m != nil {
			if (o.Map != "" && m[1] != o.Map) || (o.Surface != "" && UnnameIllisis(m[2]) != o.Surface) {
				return nil
			}
			ax, _ := strconv.Atoi(m[4])
			ay, _ := strconv.Atoi(m[5])
			areas = append(areas, illStreamArea{m[1], m[2], m[3], ax, ay})
			maps[m[1]] = true
		} else if m := illMoveWindow.FindStringSubmatch(key); m != nil {
			if (o.Map != "" && m[1] != o.Map) || (o.Surface != "" && UnnameIllisis(m[2]) != o.Surface) {
				return nil
			}
			maps[m[1]] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(areas, func(i, j int) bool {
		a, b := areas[i], areas[j]
		if a.mapKey != b.mapKey {
			return a.mapKey < b.mapKey
		}
		if a.surfaceKey != b.surfaceKey {
			return a.surfaceKey < b.surfaceKey
		}
		if a.forceKey != b.forceKey {
			return a.forceKey < b.forceKey
		}
		if a.ax != b.ax {
			return a.ax < b.ax
		}
		return a.ay < b.ay
	})
	mapKeys := make([]string, 0, len(maps))
	for m := range maps {
		mapKeys = append(mapKeys, m)
	}
	sort.Strings(mapKeys)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	var out []StreamFile
	for _, mp := range mapKeys {
		var rows []streamRow
		for _, a := range areas {
			if a.mapKey != mp {
				continue
			}
			if err := streamArea(root, a, o, &rows); err != nil {
				return nil, err
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].t < rows[j].t })
		path := filepath.Join(outDir, mp+".chunks.tsv")
		if err := writeStreamRows(path, rows); err != nil {
			return nil, err
		}
		out = append(out, StreamFile{Map: mp, Path: path, Rows: len(rows)})
	}
	return out, nil
}

type streamRow struct {
	t              int64
	surface, force string
	cx, cy         int
	px             [ChunkPixelsLen]uint16
}

// ChunkPixelsLen is the number of pixels in a chunk chart.
const ChunkPixelsLen = 1024

func writeStreamRows(path string, rows []streamRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	var raw [2 * ChunkPixelsLen]byte
	var hx [4 * ChunkPixelsLen]byte
	for i := range rows {
		r := &rows[i]
		for k, v := range r.px {
			binary.LittleEndian.PutUint16(raw[2*k:], v)
		}
		hex.Encode(hx[:], raw[:])
		fmt.Fprintf(w, "%d\t%s\t%d,%d\t%s\t", r.t, r.surface, r.cx, r.cy, r.force)
		w.Write(hx[:])
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readGz(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return io.ReadAll(io.LimitReader(zr, 1<<30))
}

// streamArea replays one z0 area's change files and appends its rows.
func streamArea(root string, a illStreamArea, o StreamOptions, rows *[]streamRow) error {
	adir := filepath.Join(root, a.mapKey, a.surfaceKey, a.forceKey, "0", fmt.Sprintf("%d_%d", a.ax, a.ay))
	b, err := readGz(filepath.Join(adir, "index.json.gz"))
	if err != nil {
		return err
	}
	var idx struct {
		Changes [][]json.Number `json:"changes"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return fmt.Errorf("%s: %w", adir, err)
	}
	surface, force := UnnameIllisis(a.surfaceKey), UnnameIllisis(a.forceKey)
	state := map[int]*[ChunkPixelsLen]uint16{}
	type winEntry struct {
		block int
		start [ChunkPixelsLen]uint16
	}
	var winOrder []winEntry
	inWin := map[int]bool{}
	lastT := map[int]int64{}
	var curW int64
	haveW := false
	emit := func(block int, t int64) {
		bx, by := block&15, block>>4
		*rows = append(*rows, streamRow{t: t, surface: surface, force: force,
			cx: a.ax*16 - 8 + bx, cy: a.ay*16 - 8 + by, px: *state[block]})
	}
	flush := func() {
		for _, e := range winOrder {
			if *state[e.block] != e.start {
				emit(e.block, lastT[e.block])
			}
		}
		winOrder = winOrder[:0]
		clear(inWin)
	}
	for _, ch := range idx.Changes {
		if len(ch) == 0 {
			continue
		}
		first, err := ch[0].Int64()
		if err != nil {
			return fmt.Errorf("%s: change %v: %w", adir, ch, err)
		}
		data, err := readGz(filepath.Join(adir, fmt.Sprintf("%d.c.gz", first)))
		if err != nil {
			return err
		}
		if len(data) < 4 || string(data[:4]) != "MMC1" {
			return fmt.Errorf("%s/%d.c.gz: not MMC1", adir, first)
		}
		for off := 4; off < len(data); {
			if off+7 > len(data) {
				return errors.New("truncated MMC1 record header")
			}
			t := first + int64(binary.LittleEndian.Uint32(data[off:]))
			block := int(data[off+4])
			n := int(binary.LittleEndian.Uint16(data[off+5:]))
			off += 7
			st := state[block]
			if st == nil {
				st = new([ChunkPixelsLen]uint16)
				state[block] = st
			}
			if o.CoalesceMs > 0 {
				w := t / o.CoalesceMs
				if haveW && w != curW {
					flush()
				}
				curW, haveW = w, true
				if !inWin[block] {
					inWin[block] = true
					winOrder = append(winOrder, winEntry{block: block, start: *st})
				}
			}
			changed := false
			set := func(p int, c uint16) {
				if o.DropEnemy && c == enemyColour {
					return
				}
				if st[p] != c {
					st[p] = c
					changed = true
				}
			}
			if n == 0 {
				if off+2*ChunkPixelsLen > len(data) {
					return errors.New("truncated MMC1 full block")
				}
				for p := 0; p < ChunkPixelsLen; p++ {
					set(p, binary.LittleEndian.Uint16(data[off+2*p:]))
				}
				off += 2 * ChunkPixelsLen
			} else {
				if off+4*n > len(data) {
					return errors.New("truncated MMC1 pixel list")
				}
				for k := 0; k < n; k++ {
					p := int(binary.LittleEndian.Uint16(data[off+4*k:]))
					if p >= ChunkPixelsLen {
						return fmt.Errorf("MMC1 pixel %d out of range", p)
					}
					set(p, binary.LittleEndian.Uint16(data[off+4*k+2:]))
				}
				off += 4 * n
			}
			lastT[block] = t
			if o.CoalesceMs == 0 && changed {
				emit(block, t)
			}
		}
	}
	if o.CoalesceMs > 0 {
		flush()
	}
	return nil
}
