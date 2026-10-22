package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"timeshadedb/internal/import/minimap"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
	"timeshadedb/internal/v3/pyramid"
	"timeshadedb/internal/v3/terrain"
)

func init() {
	register(Command{
		Name:    "import-minimap",
		Summary: "import an illisis/minimap export (terrain, movers, overlays) and verify it",
		Run:     cmdImportMinimap,
	})
}

// importStores are the stores import-minimap writes into: the file-backed v3 stores
// under --db (v3stores.go), or the in-memory reference stores without it.
type importStores struct {
	Terrain  core.TerrainStore
	Movers   core.MoverStore
	Overlays core.OverlayStore
	Close    func() error
}

func openMemImportStores() importStores {
	t, m, o := memstore.NewTerrain(), memstore.NewMovers(), memstore.NewOverlays()
	return importStores{Terrain: t, Movers: m, Overlays: o, Close: func() error {
		return errors.Join(t.Close(), m.Close(), o.Close())
	}}
}

func cmdImportMinimap(ctx context.Context, args []string) error {
	return runImportMinimap(ctx, os.Stdout, args)
}

func runImportMinimap(ctx context.Context, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("import-minimap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	data := fs.String("data", "", "illisis export root (the directory holding maps.json.gz); may also be given as the only argument")
	window := fs.Duration("window", 60*time.Second, "terrain window for the imported datastores")
	workers := fs.Int("workers", runtime.GOMAXPROCS(0), "parallel decoders")
	maps := fs.String("maps", "", "comma-separated raw map keys to import (default all)")
	surfaces := fs.String("surfaces", "", "comma-separated raw surface names to import (default all)")
	noTerrain := fs.Bool("no-terrain", false, "skip terrain")
	noMovers := fs.Bool("no-movers", false, "skip movers")
	noOverlays := fs.Bool("no-overlays", false, "skip tags and recipes")
	verify := fs.Bool("verify", false, "compare the imported state with the illisis data and read the movers and overlays back")
	history := fs.Int("verify-history", 0, "historical checks per z0 area with --verify (-1: every snapshot)")
	reportPath := fs.String("report", "", "write the full JSON report to this file")
	mapsOut := fs.String("maps-out", "", "write the catalog map metadata (core.MapInfo list) to this JSON file")
	allowErrors := fs.Bool("allow-errors", false, "exit 0 even when some files fail to read or decode (a best-effort import)")
	dbDir := fs.String("db", "", "v3 database root to import into (default: in-memory reference stores, nothing is written)")
	noPyramid := fs.Bool("no-pyramid", false, "with --db, skip building the coarse levels after the import")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fmt.Fprintln(stdout, "usage: timeshadedb import-minimap [flags] <export dir>")
			fs.PrintDefaults()
			return nil
		}
		return fmt.Errorf("import-minimap: %w", err)
	}
	root := *data
	switch {
	case root == "" && fs.NArg() == 1:
		root = fs.Arg(0)
	case root != "" && fs.NArg() == 0:
	default:
		return errors.New("usage: timeshadedb import-minimap [flags] <export dir>")
	}
	if *window <= 0 || *window%time.Millisecond != 0 {
		return fmt.Errorf("import-minimap: bad --window %v", *window)
	}

	d, err := minimap.Open(root)
	if err != nil {
		return err
	}
	var stores importStores
	var ter *terrain.Store
	if *dbDir != "" {
		stores, ter, err = openV3ImportStores(*dbDir)
		if err != nil {
			return err
		}
		// The import cannot resume: the stores would drop everything before their
		// horizon as late, so it only writes into an empty database.
		if err := checkImportTargetEmpty(ctx, stores); err != nil {
			stores.Close()
			return fmt.Errorf("import-minimap: %s: %w", *dbDir, err)
		}
	} else {
		stores = openMemImportStores()
	}
	closed := false
	defer func() {
		if !closed {
			stores.Close()
		}
	}()
	opt := minimap.Options{
		Window:   core.TimeMs(window.Milliseconds()),
		Workers:  *workers,
		Maps:     splitList(*maps),
		Surfaces: splitList(*surfaces),
	}
	if !*noTerrain {
		opt.Terrain = stores.Terrain
	}
	if !*noMovers {
		opt.Movers = stores.Movers
	}
	if !*noOverlays {
		opt.Overlays = stores.Overlays
	}
	// The pyramid is built from the terrain store alone, so it starts as soon as the
	// terrain import is done, while the movers are still importing.
	var pyr *pyramid.BuildStats
	var pyrMs int64
	if ter != nil && opt.Terrain != nil && !*noPyramid {
		opt.AfterTerrain = func(ctx context.Context) error {
			t0 := time.Now()
			st, err := buildV3Pyramid(ctx, ter)
			if err != nil {
				return fmt.Errorf("pyramid: %w", err)
			}
			pyr, pyrMs = &st, time.Since(t0).Milliseconds()
			return nil
		}
	}
	rep, err := minimap.Import(ctx, d, opt)
	if err != nil {
		return err
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	full := struct {
		Import         *minimap.Report         `json:"import"`
		HeapAllocBytes uint64                  `json:"heap_alloc_bytes"`
		Pyramid        *pyramid.BuildStats     `json:"pyramid,omitempty"`
		PyramidMs      int64                   `json:"pyramid_ms,omitempty"`
		CloseMs        int64                   `json:"close_ms,omitempty"`
		DiskBytes      int64                   `json:"disk_bytes,omitempty"`
		TerrainVerify  *minimap.TerrainVerify  `json:"terrain_verify,omitempty"`
		MoverVerify    []minimap.MoverVerify   `json:"mover_verify,omitempty"`
		OverlayVerify  []minimap.OverlayVerify `json:"overlay_verify,omitempty"`
	}{Import: rep, HeapAllocBytes: mem.HeapAlloc, Pyramid: pyr, PyramidMs: pyrMs}
	printImportReport(stdout, rep, mem.HeapAlloc)
	if pyr != nil {
		fmt.Fprintf(stdout, "pyramid: %d versions (%d snapshots, %d deltas), %d payload bytes, %d ms (started after terrain, during the mover import)\n",
			pyr.Versions, pyr.Snapshots, pyr.Deltas, pyr.Bytes, pyrMs)
	}

	ok := true
	if *verify {
		if opt.Terrain != nil {
			tv, err := minimap.VerifyTerrain(ctx, d, stores.Terrain, opt, minimap.VerifyOptions{HistoryPerArea: *history, Workers: *workers})
			if err != nil {
				return err
			}
			full.TerrainVerify = tv
			ok = ok && tv.OK()
			fmt.Fprintf(stdout, "verify terrain: final %d/%d areas equal (%d against the latest snapshot alone), history %d/%d, coarse z1-z4 %d/%d (%d ms)\n",
				tv.FinalEqual, tv.FinalAreas, tv.LatestSnapshotAreas, tv.HistoryEqual, tv.HistoryChecks, tv.CoarseEqual, tv.CoarseAreas, tv.Ms)
			for _, f := range tv.Failures {
				fmt.Fprintf(stdout, "  FAIL %s %v %s diff=%d %s\n", f.DS, f.Tile, f.What, f.Diff, f.Error)
			}
		}
		if opt.Movers != nil {
			mv, err := minimap.VerifyMovers(ctx, d, rep, stores.Movers)
			if err != nil {
				return err
			}
			full.MoverVerify = mv
			for _, v := range mv {
				ok = ok && v.Match
				fmt.Fprintf(stdout, "verify movers %s: match=%v windows=%d keyframes=%d samples=%v gones=%v handoffs=%d\n",
					v.Surface, v.Match, v.Windows, v.Keyframes, fmtCounts(v.Stored.Samples), fmtCounts(v.Stored.Gones), v.Handoffs)
				fmt.Fprintf(stdout, "  against the source: %d rows, %d matched (%d re-timed), %d duplicates, %d missing, %d extra, %d flag errors\n",
					v.SourceRows, v.Matched, v.MatchedRetimed, v.SourceDups, v.Missing, v.Extra, v.FlagErrors)
				for _, e := range v.Examples {
					fmt.Fprintf(stdout, "  %s\n", e)
				}
			}
		}
		if opt.Overlays != nil {
			ov, err := minimap.VerifyOverlays(ctx, rep, stores.Overlays)
			if err != nil {
				return err
			}
			full.OverlayVerify = ov
			for _, v := range ov {
				ok = ok && v.Match
				fmt.Fprintf(stdout, "verify overlays %s: match=%v intervals=%d\n", v.DS, v.Match, v.Intervals)
			}
		}
	}
	if *dbDir != "" {
		if err := writeV3Maps(*dbDir, rep.Maps); err != nil {
			return err
		}
		if err := copyV3Icons(*dbDir, d.Root, rep.Icons); err != nil {
			return err
		}
		t0 := time.Now()
		closed = true
		if err := stores.Close(); err != nil {
			return err
		}
		full.CloseMs = time.Since(t0).Milliseconds()
		if full.DiskBytes, err = dirBytes(*dbDir); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "database %s: %d bytes on disk, closed in %d ms\n", *dbDir, full.DiskBytes, full.CloseMs)
	}
	if *reportPath != "" {
		if err := writeJSONFile(*reportPath, full); err != nil {
			return err
		}
	}
	if *mapsOut != "" {
		if err := writeJSONFile(*mapsOut, rep.Maps); err != nil {
			return err
		}
	}
	if !ok {
		return errors.New("import-minimap: verification failed")
	}
	// The importer feeds every store in time order, so late rows mean lost history.
	if n := rep.TerrainApply.Late + rep.MoverApply.Late + rep.OverlayApply.Late; n > 0 {
		return fmt.Errorf("import-minimap: the stores dropped %d updates as late (terrain %d, movers %d, overlays %d)",
			n, rep.TerrainApply.Late, rep.MoverApply.Late, rep.OverlayApply.Late)
	}
	if n := rep.FileErrors(); n > 0 && !*allowErrors {
		return fmt.Errorf("import-minimap: %d files failed to read or decode (listed above); the import is incomplete (--allow-errors accepts that)", n)
	}
	return nil
}

// checkImportTargetEmpty fails when any file-backed store already holds a datastore
// or surface.
func checkImportTargetEmpty(ctx context.Context, s importStores) error {
	if infos, err := s.Terrain.Datastores(ctx); err != nil {
		return err
	} else if len(infos) > 0 {
		return fmt.Errorf("already holds %d terrain datastores (%s, ...); import into an empty --db", len(infos), infos[0].Key.ID())
	}
	if sfcs, err := s.Movers.Surfaces(ctx); err != nil {
		return err
	} else if len(sfcs) > 0 {
		return fmt.Errorf("already holds movers for %d surfaces; import into an empty --db", len(sfcs))
	}
	if l, ok := s.Overlays.(interface {
		Datastores(context.Context) ([]core.DatastoreKey, error)
	}); ok {
		if keys, err := l.Datastores(ctx); err != nil {
			return err
		} else if len(keys) > 0 {
			return fmt.Errorf("already holds overlays for %d datastores (%s, ...); import into an empty --db", len(keys), keys[0].ID())
		}
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fmtCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, m[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func printImportReport(w io.Writer, r *minimap.Report, heap uint64) {
	fmt.Fprintf(w, "import %s: total %d ms (terrain %d ms, movers %d ms, overlays %d ms), heap %d MB\n",
		r.Root, r.TotalMs, r.TerrainMs, r.MoversMs, r.OverlaysMs, heap>>20)
	for _, m := range r.Maps {
		fmt.Fprintf(w, "map %s %q: %d instances, edges on %d surfaces\n", m.Key, m.Title, len(m.Instances), len(m.Edges))
	}
	for _, t := range r.Terrain {
		fmt.Fprintf(w, "terrain %s: %d areas, %d files, %d records (%d whole), %d pixel writes -> %d updates (%d deletes) in %d windows, backwards %d\n",
			t.DS, t.Areas, t.ChangeFiles, t.Records, t.WholeBlocks, t.PixelWrites, t.Updates, t.Deletes, t.Windows, t.Backwards)
		for _, e := range t.Errors {
			fmt.Fprintf(w, "  error: %s\n", e)
		}
	}
	fmt.Fprintf(w, "terrain store: %+v\n", r.TerrainApply)
	for _, m := range r.Movers {
		fmt.Fprintf(w, "movers %s: %d windows, %d keyframes; source journeys %s samples %s gones %s dup pairs %d\n",
			m.Surface, m.WindowFiles, m.KeyframeFiles, fmtCounts(m.Source.Journeys), fmtCounts(m.Source.Samples), fmtCounts(m.Source.Gones), m.SourceDupPairs)
		fmt.Fprintf(w, "  fixes: boundary %d, retimed %s (+%d gones), dup samples dropped %d, dup gones dropped %d, keyframe rows %d matched %d seeded %d conflicts %d, invalid %d\n",
			m.BoundaryStamped, fmtCounts(m.Retimed), m.RetimedGones, m.DupSamplesDropped, m.DupGonesDropped, m.KeyframeRows, m.KeyframeMatched, m.KeyframeSeeded, m.KeyframeConflicts, m.InvalidRows)
		fmt.Fprintf(w, "  headings derived %d, no heading %d, stops %d, end switches %d, handoffs %d (%d edge crossings without a gone), train jumps %d -> %d\n",
			m.HeadingsDerived, m.NoHeading, m.Stops, m.EndSwitches, m.Handoffs, m.CrossingsInferred, m.TrainJumpsBefore, m.TrainJumpsAfter)
		fmt.Fprintf(w, "  imported journeys %s samples %s gones %s\n", fmtCounts(m.Imported.Journeys), fmtCounts(m.Imported.Samples), fmtCounts(m.Imported.Gones))
		for _, e := range m.Errors {
			fmt.Fprintf(w, "  error: %s\n", e)
		}
	}
	fmt.Fprintf(w, "mover store: %+v\n", r.MoverApply)
	for _, o := range r.Overlays {
		fmt.Fprintf(w, "overlays %s: %d tag and %d recipe versions, %d invalid, %d empty, %d overlaps -> %d intervals, %d events\n",
			o.DS, o.TagVersions, o.RecipeVersions, o.Invalid, o.Empty, o.Overlaps, o.Intervals, o.Events)
		for _, e := range o.Errors {
			fmt.Fprintf(w, "  error: %s\n", e)
		}
	}
	fmt.Fprintf(w, "overlay store: %+v\n", r.OverlayApply)
	for _, s := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", s)
	}
}
