package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb"
	"timeshadedb/internal/v3/api"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/ingest"
	"timeshadedb/internal/v3/movers"
	"timeshadedb/internal/v3/overlays"
	"timeshadedb/internal/v3/pyramid"
	"timeshadedb/internal/v3/terrain"
	"timeshadedb/internal/v3/tileenc"
)

func init() {
	register(Command{
		Name:    "serve-v3",
		Summary: "serve a v3 database: HTTP API, ingest, live events and the viewer",
		Run:     cmdServeV3,
	})
}

func cmdServeV3(ctx context.Context, args []string) error {
	return runServeV3(ctx, os.Stdout, os.Stderr, args)
}

// v3ServerOptions are the parsed serve-v3 flags.
type v3ServerOptions struct {
	db, listen, ingestToken, readToken, pprof, webDir, clock string
	memBudget                                                int64
	window, commitEvery, terrainGrace, moverGrace, slowLog   time.Duration
	sourceHold                                               time.Duration
	noViewer, stdinExit, salvageOverlays, quiet              bool
}

func parseServeV3(stdout io.Writer, args []string) (*v3ServerOptions, error) {
	o := &v3ServerOptions{}
	fs := flag.NewFlagSet("serve-v3", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.db, "db", "", "v3 database root (created if missing)")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "listen address; port 0 picks a free port (the first stdout line is \"listening on <addr>\")")
	fs.StringVar(&o.ingestToken, "ingest-token", "", "bearer token required by POST /api/v3/ingest (required when not listening on loopback)")
	fs.StringVar(&o.readToken, "read-token", "", "bearer token required by every API read (optional)")
	fs.Int64Var(&o.memBudget, "mem-budget", 256<<20, "total cache and buffer budget in bytes")
	fs.DurationVar(&o.window, "window", 60*time.Second, "terrain window for new datastores")
	fs.DurationVar(&o.commitEvery, "commit-every", time.Second, "commit loop period")
	fs.DurationVar(&o.terrainGrace, "terrain-grace", 10*time.Second, "how far behind the clock terrain windows close")
	fs.DurationVar(&o.moverGrace, "mover-grace", 15*time.Second, "how far behind the clock mover windows close (at least 10s)")
	fs.DurationVar(&o.slowLog, "slow-log", 0, "log requests slower than this (0: off)")
	fs.StringVar(&o.clock, "clock", "wall", "map clock: wall (live capture) or data (the newest ingested recv_ms, for replaying recorded captures)")
	fs.DurationVar(&o.sourceHold, "source-hold", time.Hour, "how long a source that went quiet (or every source, after a restart) holds the horizon of its maps at its newest data, so its backlog is not late (0: the horizon follows the clock alone)")
	fs.StringVar(&o.pprof, "pprof", "", "serve net/http/pprof on this separate address")
	fs.StringVar(&o.webDir, "web-dir", "", "serve the viewer from this directory instead of the embedded bundle")
	fs.BoolVar(&o.noViewer, "no-viewer", false, "do not serve the viewer")
	fs.BoolVar(&o.stdinExit, "exit-on-stdin-close", false, "shut down when stdin closes (for a parent process such as the Clusterio plugin)")
	fs.BoolVar(&o.salvageOverlays, "salvage-overlays", false, "open overlay logs damaged before their end, keeping the damaged file aside")
	fs.BoolVar(&o.quiet, "quiet", false, "log only errors")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fmt.Fprintln(stdout, "usage: timeshadedb serve-v3 --db <dir> [flags]")
			fs.PrintDefaults()
			return nil, nil
		}
		return nil, fmt.Errorf("serve-v3: %w", err)
	}
	if fs.NArg() != 0 {
		return nil, errors.New("serve-v3: unexpected arguments")
	}
	if o.db == "" {
		return nil, errors.New("serve-v3: --db is required")
	}
	if o.window <= 0 || o.window%time.Millisecond != 0 {
		return nil, fmt.Errorf("serve-v3: bad --window %v", o.window)
	}
	if o.memBudget < 32<<20 {
		return nil, errors.New("serve-v3: --mem-budget must be at least 32 MiB")
	}
	if o.sourceHold < 0 {
		return nil, fmt.Errorf("serve-v3: bad --source-hold %v", o.sourceHold)
	}
	if o.clock != "wall" && o.clock != "data" {
		return nil, fmt.Errorf("serve-v3: bad --clock %q (wall or data)", o.clock)
	}
	if o.clock == "data" {
		// A replay posts recorded data, often several bodies at once, so bodies arrive out
		// of order by tens of seconds; close windows further behind the data.
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if !set["terrain-grace"] {
			o.terrainGrace = 2 * time.Minute
		}
		if !set["mover-grace"] {
			o.moverGrace = 2 * time.Minute
		}
	}
	host, _, err := net.SplitHostPort(o.listen)
	if err != nil {
		return nil, fmt.Errorf("serve-v3: bad --listen: %w", err)
	}
	if !isLoopback(host) && o.ingestToken == "" {
		return nil, errors.New("serve-v3: --ingest-token is required when listening on anything but loopback")
	}
	return o, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// v3Server is an opened database with its API server.
type v3Server struct {
	ter     *terrain.Store
	pyr     *pyramid.Pyramid
	rend    *tileenc.Renderer
	mov     *movers.Store
	ovl     *overlays.Store
	api     *api.Server
	opened  time.Duration
	closers []func() error
}

// Close closes the stores in reverse order of opening.
func (v *v3Server) Close() error {
	var errs []error
	for i := len(v.closers) - 1; i >= 0; i-- {
		errs = append(errs, v.closers[i]())
	}
	return errors.Join(errs...)
}

// openV3Server opens every store under o.db and builds the API server. The memory
// budget is split (PLAN.md §4.3): to the terrain store 3/32 for decoded tiles, 1/8 for
// open windows (Apply refuses batches beyond it, counting every update of a batch as a
// new open chunk: 1/16 was too little for the synthetic soak at 100x and 3/32 for the
// test data's 60 s stream) and 3/64 for idle indexes; 1/8 to the encoded-response cache, 1/16 to decoded coarse images, 1/32 to overlay caches
// and an eighth bounds the mover open rows (held only while a burst outruns the commit
// loop). The rest of the budget is the working memory of ingest and reads, the encoders
// and the collector's headroom: with full caches the live heap measured about 190 MiB
// at 3/8 + 3/16 + 3/32, and the process must stay within the budget + 64 MiB
// (runServeV3).
func openV3Server(ctx context.Context, o *v3ServerOptions, logf func(string, ...any)) (*v3Server, error) {
	t0 := time.Now()
	if err := os.MkdirAll(o.db, 0o755); err != nil {
		return nil, err
	}
	v := &v3Server{}
	fail := func(err error) (*v3Server, error) { return nil, errors.Join(err, v.Close()) }
	clock := ingest.WallClock
	// refClock is the movers' MaxAhead reference: the wall clock, also for a replay on
	// the data clock (whose data lies in the past; ingest clamps message times to the
	// wall clock plus 10 s), so a gap in replayed data is not taken for a broken clock.
	refClock := ingest.WallClock
	var dataClock *ingest.DataClock
	if o.clock == "data" {
		dataClock = &ingest.DataClock{}
		clock = dataClock.Now
	}
	sources, err := ingest.OpenSources(o.db, o.sourceHold, nil)
	if err != nil {
		logf("%v", err)
	}

	topt := terrain.Options{DecodedCacheBytes: o.memBudget * 3 / 32, OpenWindowBytes: o.memBudget / 8, MaxIndexBytes: o.memBudget * 3 / 64,
		MinTime: terrain.DefaultMinTime}
	topt.Window = core.TimeMs(o.window.Milliseconds())
	// Each commit worker holds decoded tiles and an encoder; bound them for memory.
	topt.Workers = min(runtime.GOMAXPROCS(0), 8)
	topt.Logf = logf
	ter, err := terrain.Open(o.db, topt)
	if err != nil {
		return nil, fmt.Errorf("terrain: %w", err)
	}
	v.ter = ter
	v.closers = append(v.closers, ter.Close)

	// Coarse series load lazily (and are preloaded in the background), so the server
	// listens at once whatever the size of the history.
	// SpeedBetterCompression rather than the default best: the best encoder holds about
	// 34 MiB of tables for the life of the server, and the coarse tiles a live server
	// builds come out only about 4% larger (imports keep the best level).
	pyr, err := pyramid.New(ctx, ter, ter.SeriesStore(), pyramid.Options{CacheBytes: o.memBudget / 16, Lazy: true,
		ZstdLevel: zstd.SpeedBetterCompression})
	if err != nil {
		return fail(fmt.Errorf("pyramid: %w", err))
	}
	v.pyr = pyr
	v.closers = append(v.closers, pyr.Close)

	rend, err := tileenc.NewRenderer(pyr, ter, tileenc.Options{CacheBytes: o.memBudget / 8})
	if err != nil {
		return fail(err)
	}
	v.rend = rend
	v.closers = append(v.closers, rend.Close)

	// An eighth of the budget for the open mover windows, counted per row (about 150 bytes
	// a row, more with carriages); beyond it whole windows wait in their logs on disk.
	mov, err := movers.Open(o.db, movers.Options{SyncInterval: time.Second, OpenBytes: o.memBudget / 8, Clock: refClock, Logf: logf,
		LightEncoder: true})
	if err != nil {
		return fail(fmt.Errorf("movers: %w", err))
	}
	v.mov = mov
	v.closers = append(v.closers, mov.Close)

	ovl, err := overlays.Open(o.db, overlays.Options{CacheBytes: o.memBudget / 32, Salvage: o.salvageOverlays})
	if err != nil {
		if errors.Is(err, overlays.ErrCorrupt) {
			err = fmt.Errorf("%w (start with --salvage-overlays to keep the damaged log aside and continue)", err)
		}
		return fail(fmt.Errorf("overlays: %w", err))
	}
	v.ovl = ovl
	v.closers = append(v.closers, ovl.Close)

	// The newest data per map: the data clock starts from it, and maps no source on record
	// feeds are held at it until a source shows up (ingest.Sources.HoldUnknown).
	newest, err := ingest.NewestData(ctx, ter, mov)
	if err != nil {
		return fail(err)
	}
	if dataClock != nil {
		for _, t := range newest {
			dataClock.Observe(t)
		}
	}
	sources.HoldUnknown(newest)
	maps, err := ingest.OpenMaps(o.db)
	if err != nil {
		return fail(err)
	}
	var web fs.FS
	switch {
	case o.noViewer:
	case o.webDir != "":
		web = os.DirFS(o.webDir)
	default:
		if web, err = timeshadedb.WebDistFS(); err != nil {
			return fail(err)
		}
	}
	srv, err := api.New(api.Config{
		Terrain:      ter,
		Tiles:        pyr,
		Renderer:     rend,
		Movers:       mov,
		Overlays:     ovl,
		Maps:         maps,
		IconsDir:     filepath.Join(o.db, "icons"),
		Web:          web,
		ReadToken:    o.readToken,
		IngestToken:  o.ingestToken,
		Clock:        clock,
		CommitEvery:  o.commitEvery,
		TerrainGrace: core.TimeMs(o.terrainGrace.Milliseconds()),
		MoverGrace:   core.TimeMs(o.moverGrace.Milliseconds()),
		Sources:      sources,
		MemBudget:    o.memBudget,
		SlowLog:      o.slowLog,
		Logf:         logf,
	})
	if err != nil {
		return fail(err)
	}
	in := ingest.New(ingest.Stores{Terrain: ter, Movers: mov, Overlays: ovl, Maps: maps}, ingest.Options{
		Clock:     clock,
		DataClock: dataClock,
		Sources:   sources,
		Notifier:  srv.Notifier(),
		Logf:      logf,
	})
	srv.SetIngester(in)
	v.api = srv
	v.opened = time.Since(t0)
	return v, nil
}

func runServeV3(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	o, err := parseServeV3(stdout, args)
	if err != nil || o == nil {
		return err
	}
	logger := log.New(stderr, "serve-v3: ", log.LstdFlags|log.Lmicroseconds)
	logf := logger.Printf
	quietf := logf
	if o.quiet {
		quietf = func(string, ...any) {}
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.stdinExit {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			quietf("stdin closed, shutting down")
			cancel()
		}()
	}

	// The budget is the process's Go memory: caches (about half of it, openV3Server),
	// buffers and the collector's headroom. The soft limit holds the runtime's mapped
	// memory near it under load; on Windows the committed private bytes measured about
	// 30 MiB above that (runtime metadata, the executable's writable pages) and the
	// working set up to there, so RSS stays within PLAN.md §5's mem-budget + 64 MiB. About
	// 50 MiB (codecs, ingest buffers) does not scale with the budget, so below 256 MiB a
	// full-speed ingest goes past that bound (docs/v3/API.md 8). GOMEMLIMIT overrides.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(o.memBudget)
	}
	v, err := openV3Server(ctx, o, logf)
	if err != nil {
		return err
	}
	defer func() {
		if v != nil {
			_ = v.Close()
		}
	}()
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	// The first stdout line names the address (docs/v3/README.md).
	fmt.Fprintf(stdout, "listening on %s\n", ln.Addr())
	quietf("opened %s in %v (viewer: %v)", o.db, v.opened.Round(time.Millisecond), !o.noViewer && (o.webDir != "" || timeshadedb.WebBundleBuilt()))

	if o.pprof != "" {
		pl, err := net.Listen("tcp", o.pprof)
		if err != nil {
			return fmt.Errorf("pprof: %w", err)
		}
		quietf("pprof on %s", pl.Addr())
		go func() { _ = http.Serve(pl, http.DefaultServeMux) }()
		defer pl.Close()
	}

	hs := &http.Server{
		Handler:           v.api,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.New(stderr, "serve-v3 http: ", log.LstdFlags|log.Lmicroseconds),
	}
	// Shutdown waits for handlers; live streams would hold it until its timeout.
	hs.RegisterOnShutdown(v.api.CloseLive)
	srvErr := make(chan error, 1)
	go func() { srvErr <- hs.Serve(ln) }()
	loopCtx, stopLoop := context.WithCancel(context.WithoutCancel(ctx))
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = v.api.Run(loopCtx)
	}()
	go func() {
		t0 := time.Now()
		if err := v.pyr.Preload(loopCtx); err != nil && !errors.Is(err, context.Canceled) {
			logf("pyramid preload: %v", err)
			return
		}
		quietf("coarse levels loaded in %v", time.Since(t0).Round(time.Millisecond))
	}()

	select {
	case <-ctx.Done():
	case err = <-srvErr:
	}
	quietf("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if hs.Shutdown(shCtx) != nil {
		// Requests still running after the timeout: cut their connections, which ends
		// their contexts, and wait for ingest below before the stores close.
		_ = hs.Close()
	}
	v.api.WaitIngest()
	stopLoop()
	<-loopDone
	// Build every pending coarse tile at the horizon, so the latest state is stored.
	if _, ferr := v.pyr.Flush(context.Background()); ferr != nil {
		logf("pyramid flush: %v", ferr)
	}
	cerr := v.Close()
	v = nil
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Join(err, cerr)
	}
	return cerr
}
