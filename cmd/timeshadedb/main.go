package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"timeshadedb"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: timeshadedb <import-csv|query-tile|stats|verify|inspect-index|export-tile|compact|benchmark|serve|tail-chunk-tsv>")
	}
	switch args[0] {
	case "import-csv":
		fs := flag.NewFlagSet("import-csv", flag.ExitOnError)
		input := fs.String("input", "", "gzip CSV input")
		path := fs.String("db", "", "database directory")
		maxRows := fs.Uint64("max-rows", 0, "optional import row limit for sampling")
		pprofAddr := fs.String("pprof", "", "optional pprof listen address")
		cpuProfile := fs.String("cpuprofile", "", "optional CPU profile output path")
		memProfile := fs.String("memprofile", "", "optional heap profile output path")
		if err := fs.Parse(args[1:]); err != nil {
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
	case "query-tile":
		fs := flag.NewFlagSet("query-tile", flag.ExitOnError)
		path := fs.String("db", "", "database directory")
		tsText := fs.String("timestamp", "", "RFC3339 timestamp")
		tileText := fs.String("tile", "", "tile coordinate x,y")
		out := fs.String("out", "", "raw output path")
		if err := fs.Parse(args[1:]); err != nil {
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
	case "stats":
		fs := flag.NewFlagSet("stats", flag.ExitOnError)
		path := fs.String("db", "", "database directory")
		if err := fs.Parse(args[1:]); err != nil {
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
		return writeJSON(db.Stats())
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		input := fs.String("input", "", "gzip CSV input")
		path := fs.String("db", "", "database directory")
		maxRows := fs.Uint64("max-rows", 0, "optional verify row limit for sampled imports")
		if err := fs.Parse(args[1:]); err != nil {
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
	case "inspect-index":
		fs := flag.NewFlagSet("inspect-index", flag.ExitOnError)
		path := fs.String("db", "", "database directory")
		tileText := fs.String("tile", "", "tile coordinate x,y")
		if err := fs.Parse(args[1:]); err != nil {
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
	case "export-tile":
		fs := flag.NewFlagSet("export-tile", flag.ExitOnError)
		path := fs.String("db", "", "database directory")
		tsText := fs.String("timestamp", "", "RFC3339 timestamp")
		tileText := fs.String("tile", "", "tile coordinate x,y")
		out := fs.String("out", "", "output path")
		format := fs.String("format", "raw", "raw or png")
		if err := fs.Parse(args[1:]); err != nil {
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
	case "compact":
		fs := flag.NewFlagSet("compact", flag.ExitOnError)
		path := fs.String("db", "", "database directory")
		pprofAddr := fs.String("pprof", "", "optional pprof listen address")
		cpuProfile := fs.String("cpuprofile", "", "optional CPU profile output path")
		memProfile := fs.String("memprofile", "", "optional heap profile output path")
		if err := fs.Parse(args[1:]); err != nil {
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
	case "benchmark":
		fs := flag.NewFlagSet("benchmark", flag.ExitOnError)
		path := fs.String("db", "", "database directory")
		queries := fs.Int("queries", 100, "queries per benchmark scenario")
		cacheSize := fs.Int64("cache-size", 0, "decoded snapshot cache bytes")
		seed := fs.Int64("seed", 1, "random seed")
		pprofAddr := fs.String("pprof", "", "optional pprof listen address")
		cpuProfile := fs.String("cpuprofile", "", "optional CPU profile output path")
		memProfile := fs.String("memprofile", "", "optional heap profile output path")
		if err := fs.Parse(args[1:]); err != nil {
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
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		path := fs.String("db", "", "database directory")
		addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
		dev := fs.Bool("dev", false, "start Vite dev server and proxy web requests to it")
		cacheSize := fs.Int64("cache-size", 64*1024*1024, "decoded snapshot cache bytes")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *path == "" {
			return fmt.Errorf("serve requires --db")
		}
		return serveHTTP(ctx, *addr, *path, *cacheSize, *dev)
	case "tail-chunk-tsv":
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
		if err := fs.Parse(args[1:]); err != nil {
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
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func parsePair(s string) (int, int, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected x,y")
	}
	x, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	y, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	return x, y, nil
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writePNG(path string, res *timeshadedb.TileResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return encodeTilePNG(f, res, res.Width, res.Height, png.DefaultCompression)
}

func startPprof(addr string) error {
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		_ = http.Serve(ln, nil)
	}()
	return nil
}

type profileSession struct {
	cpuFile *os.File
	memPath string
}

func startProfiles(cpuPath, memPath string) (*profileSession, error) {
	s := &profileSession{memPath: memPath}
	if cpuPath == "" {
		return s, nil
	}
	f, err := os.Create(cpuPath)
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	s.cpuFile = f
	return s, nil
}

func (s *profileSession) Stop() error {
	var errs []error
	if s.cpuFile != nil {
		pprof.StopCPUProfile()
		if err := s.cpuFile.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.memPath != "" {
		runtime.GC()
		f, err := os.Create(s.memPath)
		if err != nil {
			errs = append(errs, err)
		} else {
			if err := pprof.WriteHeapProfile(f); err != nil {
				errs = append(errs, err)
			}
			if err := f.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
