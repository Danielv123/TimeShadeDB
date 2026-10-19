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
		Group:   GroupV3,
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
// budget is split (PLAN.md §4.3): 3/8 to the terrain store (terrain.SplitBudget), 3/16
// to the encoded-response cache, 3/32 to decoded coarse images, 1/32 to overlay caches
// and an eighth bounds the mover open rows (held only while a burst outruns the commit
// loop). The rest of the budget is left to the working memory of ingest and reads and
// to the encoders, so the caches can fill without the collector running constantly
// against the memory limit (runServeV3).
func openV3Server(ctx context.Context, o *v3ServerOptions, logf func(string, ...any)) (*v3Server, error) {
	t0 := time.Now()
	if err := os.MkdirAll(o.db, 0o755); err != nil {
		return nil, err
	}
	v := &v3Server{}
	fail := func(err error) (*v3Server, error) { return nil, errors.Join(err, v.Close()) }
	clock := ingest.WallClock
	// refClock is the movers' MaxAhead/MaxBehind reference: on the data clock the newest
	// data read, which runs ahead of the commit clock until those rows are applied.
	refClock := clock
	var dataClock *ingest.DataClock
	if o.clock == "data" {
		dataClock = &ingest.DataClock{}
		clock = dataClock.Now
		refClock = dataClock.Seen
	}

	topt := terrain.SplitBudget(o.memBudget * 3 / 8)
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
	pyr, err := pyramid.New(ctx, ter, ter.SeriesStore(), pyramid.Options{CacheBytes: o.memBudget * 3 / 32, Lazy: true,
		ZstdLevel: zstd.SpeedBetterCompression})
	if err != nil {
		return fail(fmt.Errorf("pyramid: %w", err))
	}
	v.pyr = pyr
	v.closers = append(v.closers, pyr.Close)

	rend, err := tileenc.NewRenderer(pyr, ter, tileenc.Options{CacheBytes: o.memBudget * 3 / 16})
	if err != nil {
		return fail(err)
	}
	v.rend = rend
	v.closers = append(v.closers, rend.Close)

	// About 72 bytes per open mover row (WP4); an eighth of the budget.
	maxRows := int(o.memBudget / 8 / 72)
	mov, err := movers.Open(o.db, movers.Options{SyncInterval: time.Second, MaxOpenRows: maxRows, Clock: refClock, Logf: logf,
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

	if dataClock != nil {
		// Start from the newest data in the database.
		if infos, err := ter.Datastores(ctx); err == nil {
			for _, info := range infos {
				dataClock.Observe(info.LastT)
			}
		}
		if sfcs, err := mov.Surfaces(ctx); err == nil {
			for _, si := range sfcs {
				dataClock.Observe(si.LastT)
			}
		}
	}
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

	// The budget covers caches and buffers; the collector works harder as the process
	// nears it plus a margin, so RSS stays within PLAN.md §5's mem-budget + 64 MiB: the
	// runtime keeps its mapped memory near the limit under load, and on Windows the working
	// set measured 15-30 MiB above it (the executable's pages, heap not yet returned).
	// GOMEMLIMIT overrides.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(o.memBudget + 32<<20)
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
