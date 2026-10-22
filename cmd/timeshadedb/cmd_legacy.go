package main

// Legacy (v2 and r/place) commands. They were the cases of the old switch in main.go
// and are kept unchanged until v3 replaces them (PLAN.md D11).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"timeshadedb"
)

func init() {
	register(Command{Name: "import-csv", Summary: "import an r/place gzip CSV into a legacy tile database", Group: GroupLegacy, Run: cmdImportCsv})
	register(Command{Name: "query-tile", Summary: "write one legacy tile at a timestamp as raw palette bytes", Group: GroupLegacy, Run: cmdQueryTile})
	register(Command{Name: "stats", Summary: "print database statistics as JSON", Group: GroupLegacy, Run: cmdStats})
	register(Command{Name: "verify", Summary: "verify a legacy database against its gzip CSV input", Group: GroupLegacy, Run: cmdVerify})
	register(Command{Name: "inspect-index", Summary: "print the index of one legacy tile as JSON", Group: GroupLegacy, Run: cmdInspectIndex})
	register(Command{Name: "export-tile", Summary: "export one legacy tile at a timestamp as raw or PNG", Group: GroupLegacy, Run: cmdExportTile})
	register(Command{Name: "compact", Summary: "compact a legacy tile database", Group: GroupLegacy, Run: cmdCompact})
	register(Command{Name: "copy-chunks", Summary: "copy a v2 chunk database into a new directory", Group: GroupLegacy, Run: cmdCopyChunks})
	register(Command{Name: "benchmark", Summary: "run random legacy tile queries and print timings", Group: GroupLegacy, Run: cmdBenchmark})
	register(Command{Name: "serve", Summary: "serve the v2 HTTP API and embedded web viewer", Group: GroupLegacy, Run: cmdServe})
	register(Command{Name: "tail-chunk-tsv", Summary: "follow a v2 chunk-charted TSV file and POST rows to serve", Group: GroupLegacy, Run: cmdTailChunkTsv})
}

func cmdImportCsv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import-csv", flag.ExitOnError)
	input := fs.String("input", "", "gzip CSV input")
	path := fs.String("db", "", "database directory")
	maxRows := fs.Uint64("max-rows", 0, "optional import row limit for sampling")
	pprofAddr := fs.String("pprof", "", "optional pprof listen address")
	cpuProfile := fs.String("cpuprofile", "", "optional CPU profile output path")
	memProfile := fs.String("memprofile", "", "optional heap profile output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := startPprof(*pprofAddr); err != nil {
		return err
	}
	profiles, err := startProfiles(*cpuProfile, *memProfile)
	if err != nil {
		return err
	}
	if *input == "" || *path == "" {
		return fmt.Errorf("import-csv requires --input and --db")
	}
	if _, err := os.Stat(*path); err == nil {
		return fmt.Errorf("database path already exists: %s", *path)
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *path})
	if err != nil {
		return err
	}
	stats, err := timeshadedb.ImportCSVWithOptions(ctx, db, *input, timeshadedb.ImportOptions{MaxRows: *maxRows})
	if profileErr := profiles.Stop(); err == nil {
		err = profileErr
	}
	if err != nil {
		return err
	}
	return writeJSON(stats)
}

func cmdQueryTile(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("query-tile", flag.ExitOnError)
	path := fs.String("db", "", "database directory")
	tsText := fs.String("timestamp", "", "RFC3339 timestamp")
	tileText := fs.String("tile", "", "tile coordinate x,y")
	out := fs.String("out", "", "raw output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *tsText == "" || *tileText == "" || *out == "" {
		return fmt.Errorf("query-tile requires --db, --timestamp, --tile, and --out")
	}
	ts, err := time.Parse(time.RFC3339Nano, *tsText)
	if err != nil {
		return err
	}
	tx, ty, err := parsePair(*tileText)
	if err != nil {
		return err
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *path, ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := db.TileAt(ctx, timeshadedb.TileAtOptions{Timestamp: ts, Tile: timeshadedb.TileCoord{X: tx, Y: ty}})
	if err != nil {
		return err
	}
	return os.WriteFile(*out, res.Pixels, 0o644)
}

func cmdStats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	path := fs.String("db", "", "database directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("stats requires --db")
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *path, ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	stats, err := statsValue(db)
	if err != nil {
		return err
	}
	return writeJSON(stats)
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	input := fs.String("input", "", "gzip CSV input")
	path := fs.String("db", "", "database directory")
	maxRows := fs.Uint64("max-rows", 0, "optional verify row limit for sampled imports")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" || *path == "" {
		return fmt.Errorf("verify requires --input and --db")
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *path, ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	stats, err := timeshadedb.VerifyCSVWithOptions(ctx, db, *input, timeshadedb.VerifyOptions{MaxRows: *maxRows})
	if err != nil {
		return err
	}
	return writeJSON(stats)
}

func cmdInspectIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inspect-index", flag.ExitOnError)
	path := fs.String("db", "", "database directory")
	tileText := fs.String("tile", "", "tile coordinate x,y")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *tileText == "" {
		return fmt.Errorf("inspect-index requires --db and --tile")
	}
	tx, ty, err := parsePair(*tileText)
	if err != nil {
		return err
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *path, ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	info, err := db.InspectIndex(timeshadedb.TileCoord{X: tx, Y: ty})
	if err != nil {
		return err
	}
	return writeJSON(info)
}

func cmdExportTile(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export-tile", flag.ExitOnError)
	path := fs.String("db", "", "database directory")
	tsText := fs.String("timestamp", "", "RFC3339 timestamp")
	tileText := fs.String("tile", "", "tile coordinate x,y")
	out := fs.String("out", "", "output path")
	format := fs.String("format", "raw", "raw or png")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *tsText == "" || *tileText == "" || *out == "" {
		return fmt.Errorf("export-tile requires --db, --timestamp, --tile, and --out")
	}
	ts, err := time.Parse(time.RFC3339Nano, *tsText)
	if err != nil {
		return err
	}
	tx, ty, err := parsePair(*tileText)
	if err != nil {
		return err
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *path, ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := db.TileAt(ctx, timeshadedb.TileAtOptions{Timestamp: ts, Tile: timeshadedb.TileCoord{X: tx, Y: ty}})
	if err != nil {
		return err
	}
	switch *format {
	case "raw":
		return os.WriteFile(*out, res.Pixels, 0o644)
	case "png":
		return writePNG(*out, res)
	default:
		return fmt.Errorf("unsupported export format %q", *format)
	}
}

func cmdCompact(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("compact", flag.ExitOnError)
	path := fs.String("db", "", "database directory")
	pprofAddr := fs.String("pprof", "", "optional pprof listen address")
	cpuProfile := fs.String("cpuprofile", "", "optional CPU profile output path")
	memProfile := fs.String("memprofile", "", "optional heap profile output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("compact requires --db")
	}
	if err := startPprof(*pprofAddr); err != nil {
		return err
	}
	profiles, err := startProfiles(*cpuProfile, *memProfile)
	if err != nil {
		return err
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *path})
	if err != nil {
		return err
	}
	defer db.Close()
	stats, err := db.Compact(ctx)
	if profileErr := profiles.Stop(); err == nil {
		err = profileErr
	}
	if err != nil {
		return err
	}
	return writeJSON(stats)
}

func cmdCopyChunks(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("copy-chunks", flag.ExitOnError)
	srcPath := fs.String("src", "", "source chunk database directory")
	dstPath := fs.String("dst", "", "destination chunk database directory to create")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *srcPath == "" || *dstPath == "" {
		return fmt.Errorf("copy-chunks requires --src and --dst")
	}
	if _, err := os.Stat(*dstPath); err == nil {
		return fmt.Errorf("destination database path already exists: %s", *dstPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	src, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *srcPath, ReadOnly: true, Format: timeshadedb.FormatChunks})
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *dstPath, Format: timeshadedb.FormatChunks})
	if err != nil {
		return err
	}
	stats, err := src.CopyChunksTo(ctx, dst)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return writeJSON(stats)
}

func cmdBenchmark(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("benchmark", flag.ExitOnError)
	path := fs.String("db", "", "database directory")
	queries := fs.Int("queries", 100, "queries per benchmark scenario")
	cacheSize := fs.Int64("cache-size", 0, "decoded snapshot cache bytes")
	seed := fs.Int64("seed", 1, "random seed")
	pprofAddr := fs.String("pprof", "", "optional pprof listen address")
	cpuProfile := fs.String("cpuprofile", "", "optional CPU profile output path")
	memProfile := fs.String("memprofile", "", "optional heap profile output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("benchmark requires --db")
	}
	if err := startPprof(*pprofAddr); err != nil {
		return err
	}
	profiles, err := startProfiles(*cpuProfile, *memProfile)
	if err != nil {
		return err
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: *path, ReadOnly: true, CacheSize: *cacheSize})
	if err != nil {
		return err
	}
	defer db.Close()
	stats, err := db.Benchmark(ctx, timeshadedb.BenchmarkOptions{Queries: *queries, Seed: *seed})
	if profileErr := profiles.Stop(); err == nil {
		err = profileErr
	}
	if err != nil {
		return err
	}
	return writeJSON(stats)
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("db", "", "database directory")
	addr := fs.String("addr", "0.0.0.0:8080", "HTTP listen address")
	dev := fs.Bool("dev", false, "start Vite dev server and proxy web requests to it")
	cacheSize := fs.Int64("cache-size", 64*1024*1024, "decoded snapshot cache bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("serve requires --db")
	}
	return serveHTTP(ctx, *addr, *path, *cacheSize, *dev)
}

func cmdTailChunkTsv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tail-chunk-tsv", flag.ExitOnError)
	input := fs.String("input", "", "chunk-charted TSV file to watch")
	savefileUUID := fs.String("savefile", "", "savefile UUID to use in ingest API path")
	baseURL := fs.String("base-url", "http://127.0.0.1:8080", "TimeShadeDB base URL")
	batchRows := fs.Int("batch-rows", 10240, "maximum rows per POST")
	pollInterval := fs.Duration("poll-interval", time.Second, "poll interval for appended rows")
	progressInterval := fs.Duration("progress-interval", 10*time.Second, "progress report interval; set to 0 to only print summaries")
	requestTimeout := fs.Duration("request-timeout", 5*time.Minute, "HTTP timeout for each ingest request")
	retryInterval := fs.Duration("retry-interval", 5*time.Second, "wait duration before retrying metadata and ingest requests")
	once := fs.Bool("once", false, "send catch-up rows and exit without watching")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *input == "" || *savefileUUID == "" {
		return fmt.Errorf("tail-chunk-tsv requires --input and --savefile")
	}
	return tailChunkTSV(ctx, tailChunkTSVOptions{
		InputPath:        *input,
		SavefileUUID:     *savefileUUID,
		BaseURL:          *baseURL,
		BatchRows:        *batchRows,
		PollInterval:     *pollInterval,
		ProgressInterval: *progressInterval,
		RequestTimeout:   *requestTimeout,
		RetryInterval:    *retryInterval,
		Once:             *once,
	})
}
