package benchlib

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/wire"
)

// V3Disk is the size of a v3 database by category (FORMAT.md 5).
type V3Disk struct {
	Total int64
	// TerrainZ0 is datastores/*/tiles (the z0 series, .blk + .idx).
	TerrainZ0 int64
	// Pyramid is datastores/*/pyramid.
	Pyramid int64
	// TerrainMeta is palette.bin, meta.json and anything else under datastores/ except
	// tiles, pyramid and wal.
	TerrainMeta int64
	WAL         int64
	Movers      int64
	Overlays    int64
	// Icons is icons/: catalog icons copied from the capture source (API.md icons
	// route). They are not map history, and the illisis figure (Store) leaves its
	// icons out too.
	Icons int64
	Other int64
	// ByDatastore maps a datastore id to its z0 tile bytes.
	ByDatastoreZ0 map[string]int64
	// ByDatastoreTerrain maps a datastore id to all of its terrain bytes except wal.
	ByDatastoreTerrain map[string]int64
}

// MeasureV3Disk walks a v3 database.
func MeasureV3Disk(db string) (V3Disk, error) {
	d := V3Disk{ByDatastoreZ0: map[string]int64{}, ByDatastoreTerrain: map[string]int64{}}
	err := filepath.WalkDir(db, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		n := info.Size()
		rel, _ := filepath.Rel(db, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		d.Total += n
		switch {
		case len(parts) >= 3 && parts[0] == "datastores":
			id := parts[1]
			switch parts[2] {
			case "open": // spilled open windows, a cache of the WAL
				d.WAL += n
			case "tiles":
				d.TerrainZ0 += n
				d.ByDatastoreZ0[id] += n
				d.ByDatastoreTerrain[id] += n
			case "pyramid":
				d.Pyramid += n
				d.ByDatastoreTerrain[id] += n
			case "wal":
				d.WAL += n
			default:
				d.TerrainMeta += n
				d.ByDatastoreTerrain[id] += n
			}
		case len(parts) >= 2 && parts[0] == "movers":
			// movers/<surface>/open/: the open windows' logs; movers/wal/: the single
			// log of older stores.
			if parts[1] == "wal" || (len(parts) >= 3 && parts[2] == "open") {
				d.WAL += n
			} else {
				d.Movers += n
			}
		case len(parts) >= 2 && parts[0] == "overlays":
			d.Overlays += n
		case len(parts) >= 2 && parts[0] == "icons":
			d.Icons += n
		default:
			d.Other += n
		}
		return nil
	})
	return d, err
}

// V3MoverRows counts the rows of the closed mover windows of a v3 database
// (movers/<surface>/windows/**/*.mvw, FORMAT.md 5.5: one zstd frame holding an MVW3
// body). Keyframes repeat rows that are already in windows and are not counted.
type V3MoverRows struct {
	Windows, Samples, Gones int64
}

// CountV3MoverRows decodes every closed mover window under db.
func CountV3MoverRows(db string) (V3MoverRows, error) {
	var c V3MoverRows
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return c, err
	}
	defer dec.Close()
	root := filepath.Join(db, "movers")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return c, nil
	}
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(p, ".mvw") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 || parts[1] != "windows" {
			return nil
		}
		z, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		b, err := dec.DecodeAll(z, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		w, err := wire.DecodeMoverWindow(b, core.SurfaceKey{})
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		c.Windows++
		for _, r := range w.Rows {
			if r.Flags&core.FlagGone != 0 {
				c.Gones++
			} else {
				c.Samples++
			}
		}
		return nil
	})
	return c, err
}

// IllisisDisk is the size of an illisis/minimap static export (the test data) by
// category, read without executing anything from it.
type IllisisDisk struct {
	Total int64
	// Store is everything except the viewer bundle, index.html, s3.json and icons.
	Store int64
	// TerrainZ0 and TerrainCoarse are area .c.gz, .s.gz and index.json.gz files.
	TerrainZ0, TerrainCoarse int64
	// Movers are .m.gz windows, .k.gz keyframes, day indexes and live.json.gz.
	Movers int64
	// MoverSamples counts sample rows (not gones) in the .m.gz windows.
	MoverSamples int64
	MoverGones   int64
	Overlays     int64
	// SurfaceZ0 maps "map/surface_key/force_key" to its z0 terrain bytes.
	SurfaceZ0 map[string]int64
}

var (
	illArea    = regexp.MustCompile(`^([^/]+)/([^/]+)/([^/]+)/([0-4])/-?\d+_-?\d+/(\d+\.[cs]\.gz|index\.json\.gz)$`)
	illMove    = regexp.MustCompile(`^[^/]+/[^/]+/m/\d+\.m\.gz$`)
	illMoveAux = regexp.MustCompile(`^[^/]+/[^/]+/(m/\d+\.k\.gz|m/\d{4}-\d\d-\d\d\.json\.gz|live\.json\.gz)$`)
	illOverlay = regexp.MustCompile(`(recipes|tags)\.json\.gz$`)
)

// MeasureIllisisDisk walks the export at root (the directory holding maps.json.gz).
func MeasureIllisisDisk(root string, countSamples bool) (IllisisDisk, error) {
	d := IllisisDisk{SurfaceZ0: map[string]int64{}}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		n := info.Size()
		rel, _ := filepath.Rel(root, p)
		key := filepath.ToSlash(rel)
		d.Total += n
		if strings.HasPrefix(key, "icons/") || strings.HasPrefix(key, "viewer-") || key == "index.html" || key == "s3.json" {
			return nil
		}
		d.Store += n
		switch {
		case illArea.MatchString(key):
			m := illArea.FindStringSubmatch(key)
			if m[4] == "0" {
				d.TerrainZ0 += n
				d.SurfaceZ0[m[1]+"/"+m[2]+"/"+m[3]] += n
			} else {
				d.TerrainCoarse += n
			}
		case illMove.MatchString(key):
			d.Movers += n
			if countSamples {
				s, g, err := countWindowRows(p)
				if err != nil {
					return err
				}
				d.MoverSamples += s
				d.MoverGones += g
			}
		case illMoveAux.MatchString(key):
			d.Movers += n
		case illOverlay.MatchString(key):
			d.Overlays += n
		}
		return nil
	})
	return d, err
}

// countWindowRows counts the sample and gone rows of one illisis movement window
// (store/movers.ts: {"t", "movers", "s": [[i, dt, x, y, vx, vy, h?] | [i, dt]]}).
func countWindowRows(path string) (samples, gones int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, 0, err
	}
	b, err := io.ReadAll(io.LimitReader(zr, 512<<20))
	if err != nil {
		return 0, 0, err
	}
	var w struct {
		S [][]json.RawMessage `json:"s"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return 0, 0, err
	}
	for _, r := range w.S {
		if len(r) <= 2 {
			gones++
		} else {
			samples++
		}
	}
	return samples, gones, nil
}
