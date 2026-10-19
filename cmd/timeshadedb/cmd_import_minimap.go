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
)

func init() {
	register(Command{
		Name:    "import-minimap",
		Summary: "import an illisis/minimap export (terrain, movers, overlays) and verify it",
		Group:   GroupV3,
		Run:     cmdImportMinimap,
	})
}

// importStores are the stores import-minimap writes into. Until the v3 terrain (WP2) and
// mover/overlay (WP4) stores are merged the command imports into the in-memory
// reference stores, which is what the WP5 acceptance run measures.
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
	stores := openMemImportStores()
	defer stores.Close()
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
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	rep, err := minimap.Import(ctx, d, opt)
	if err != nil {
		return err
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	full := struct {
		Import         *minimap.Report         `json:"import"`
		HeapAllocBytes uint64                  `json:"heap_alloc_bytes"`
		TerrainVerify  *minimap.TerrainVerify  `json:"terrain_verify,omitempty"`
		MoverVerify    []minimap.MoverVerify   `json:"mover_verify,omitempty"`
		OverlayVerify  []minimap.OverlayVerify `json:"overlay_verify,omitempty"`
	}{Import: rep, HeapAllocBytes: mem.HeapAlloc}
	printImportReport(stdout, rep, mem.HeapAlloc)

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
			mv, err := minimap.VerifyMovers(ctx, rep, stores.Movers)
			if err != nil {
				return err
			}
			full.MoverVerify = mv
			for _, v := range mv {
				ok = ok && v.Match
				fmt.Fprintf(stdout, "verify movers %s: match=%v windows=%d keyframes=%d samples=%v gones=%v handoffs=%d\n",
					v.Surface, v.Match, v.Windows, v.Keyframes, fmtCounts(v.Stored.Samples), fmtCounts(v.Stored.Gones), v.Handoffs)
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
		fmt.Fprintf(w, "  headings derived %d, no heading %d, stops %d, end switches %d, handoffs %d, train jumps %d -> %d\n",
			m.HeadingsDerived, m.NoHeading, m.Stops, m.EndSwitches, m.Handoffs, m.TrainJumpsBefore, m.TrainJumpsAfter)
		fmt.Fprintf(w, "  imported journeys %s samples %s gones %s\n", fmtCounts(m.Imported.Journeys), fmtCounts(m.Imported.Samples), fmtCounts(m.Imported.Gones))
		for _, e := range m.Errors {
			fmt.Fprintf(w, "  error: %s\n", e)
		}
	}
	fmt.Fprintf(w, "mover store: %+v\n", r.MoverApply)
	for _, o := range r.Overlays {
		fmt.Fprintf(w, "overlays %s: %d tag and %d recipe versions, %d invalid, %d empty, %d overlaps -> %d intervals, %d events\n",
			o.DS, o.TagVersions, o.RecipeVersions, o.Invalid, o.Empty, o.Overlaps, o.Intervals, o.Events)
	}
	fmt.Fprintf(w, "overlay store: %+v\n", r.OverlayApply)
	for _, s := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", s)
	}
}
