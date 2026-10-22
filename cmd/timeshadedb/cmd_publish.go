package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"strings"
	"syscall"
	"time"

	"timeshadedb"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/publish"
)

func init() {
	register(Command{
		Name:    "publish",
		Summary: "export the v3 database as static files to a directory or an S3/R2 bucket",
		Run:     cmdPublish,
	})
	register(Command{
		Name:    "publish-serve",
		Summary: "serve a directory written by publish --dir (for testing an export)",
		Run:     cmdPublishServe,
	})
}

func cmdPublish(ctx context.Context, args []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runPublish(ctx, os.Stdout, os.Stderr, args)
}

type publishOptions struct {
	db, from, readToken, dir, state, encoding, webDir               string
	s3Endpoint, s3Bucket, s3Prefix, s3Region, s3KeyID, s3SecretFile string
	gzipLevel, keyframeEvery, workers, maxAttempts, liveMs          int
	every                                                           time.Duration
	reset, noViewer, noMovers, noOverlays, s3VirtualHost, quiet     bool
}

func parsePublish(stdout io.Writer, args []string) (*publishOptions, error) {
	o := &publishOptions{}
	f := flag.NewFlagSet("publish", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.db, "db", "", "v3 database to export (opened here: no server may run on it)")
	f.StringVar(&o.from, "from", "", "export from a running server instead: its API base URL, e.g. http://127.0.0.1:8080/api/v3/")
	f.StringVar(&o.readToken, "read-token", "", "read token of the --from server")
	f.StringVar(&o.dir, "dir", "", "write the export into this directory")
	f.StringVar(&o.s3Endpoint, "s3-endpoint", "", "upload to this S3-compatible endpoint (https://<account>.r2.cloudflarestorage.com, http://127.0.0.1:9000)")
	f.StringVar(&o.s3Bucket, "s3-bucket", "", "bucket")
	f.StringVar(&o.s3Prefix, "s3-prefix", "", "key prefix inside the bucket")
	f.StringVar(&o.s3Region, "s3-region", "auto", "signing region (auto for R2, the bucket's region for AWS)")
	f.BoolVar(&o.s3VirtualHost, "s3-virtual-host", false, "address the bucket as <bucket>.<host> instead of <endpoint>/<bucket>")
	f.StringVar(&o.s3KeyID, "s3-access-key-id", "", "access key id (default: $AWS_ACCESS_KEY_ID)")
	f.StringVar(&o.s3SecretFile, "s3-secret-key-file", "", "file holding the secret access key (default: $AWS_SECRET_ACCESS_KEY)")
	f.StringVar(&o.state, "state", "", "publish state directory (default: <dir>/.tsdb-publish, or <db>/publish/<target> for S3)")
	f.BoolVar(&o.reset, "reset", false, "forget what the state recorded and store every object again")
	f.StringVar(&o.encoding, "encoding", "gzip", "Content-Encoding of stored objects: gzip, zstd or identity (identity for a plain static server)")
	f.IntVar(&o.gzipLevel, "gzip-level", 6, "gzip level")
	f.IntVar(&o.keyframeEvery, "keyframe-every", 1, "export a z0 tile version as a full tile only when it is 0, 1, the newest or a multiple of this (1: every version)")
	f.BoolVar(&o.noViewer, "no-viewer", false, "do not export the viewer (index.html, assets)")
	f.StringVar(&o.webDir, "web-dir", "", "export the viewer from this directory instead of the embedded bundle")
	f.BoolVar(&o.noMovers, "no-movers", false, "leave out movers")
	f.BoolVar(&o.noOverlays, "no-overlays", false, "leave out overlays")
	f.DurationVar(&o.every, "every", 0, "publish again at this interval until interrupted (with --from; 0: once)")
	f.IntVar(&o.liveMs, "live-ms", 0, "the catalog's poll interval for viewers (default: --every, else the server's)")
	f.IntVar(&o.workers, "workers", 8, "objects read and stored at once")
	f.IntVar(&o.maxAttempts, "max-attempts", 6, "attempts per object on network errors, 5xx and 429 before the round stops (negative: unbounded)")
	f.BoolVar(&o.quiet, "quiet", false, "log only errors")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			f.SetOutput(stdout)
			fmt.Fprintln(stdout, "usage: timeshadedb publish (--db <dir> | --from <url>) (--dir <out> | --s3-endpoint <url> --s3-bucket <name>) [flags]")
			f.PrintDefaults()
			return nil, nil
		}
		return nil, fmt.Errorf("publish: %w", err)
	}
	if f.NArg() != 0 {
		return nil, errors.New("publish: unexpected arguments")
	}
	if (o.db == "") == (o.from == "") {
		return nil, errors.New("publish: give exactly one of --db and --from")
	}
	if (o.dir == "") == (o.s3Endpoint == "") {
		return nil, errors.New("publish: give exactly one of --dir and --s3-endpoint")
	}
	if o.s3Endpoint != "" && o.s3Bucket == "" {
		return nil, errors.New("publish: --s3-bucket is required with --s3-endpoint")
	}
	if o.s3Endpoint != "" && o.state == "" && o.db == "" {
		return nil, errors.New("publish: --state is required with --from and --s3-endpoint")
	}
	if o.every < 0 || (o.every > 0 && o.db != "") {
		return nil, errors.New("publish: --every needs --from (an opened --db does not change while it is exported)")
	}
	if o.every > 0 && o.every < time.Second {
		return nil, errors.New("publish: --every must be at least 1s")
	}
	if o.keyframeEvery < 1 {
		return nil, errors.New("publish: --keyframe-every must be at least 1")
	}
	if o.liveMs == 0 && o.every > 0 {
		o.liveMs = int(o.every.Milliseconds())
	}
	return o, nil
}

func runPublish(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	o, err := parsePublish(stdout, args)
	if err != nil || o == nil {
		return err
	}
	logger := log.New(stderr, "publish: ", log.LstdFlags)
	logf := func(format string, args ...any) {
		logger.Printf(strings.TrimPrefix(format, "publish: "), args...)
	}
	quietf := logf
	if o.quiet {
		quietf = func(string, ...any) {}
	}

	// Source.
	var src publish.Source
	if o.db != "" {
		if fi, err := os.Stat(o.db); err != nil || !fi.IsDir() {
			return fmt.Errorf("publish: --db %s is not a directory", o.db)
		}
		v, err := openV3Server(ctx, &v3ServerOptions{db: o.db, memBudget: 256 << 20, window: 60 * time.Second, clock: "wall", noViewer: true}, logf)
		if err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		defer v.Close()
		ovl := v.ovl
		src = &publish.HandlerSource{Handler: v.api, Overlays: func(ctx context.Context, id string) ([]core.TileCoord, error) {
			key, err := core.ParseDatastoreID(id)
			if err != nil {
				return nil, err
			}
			tiles, _, err := ovl.Tiles(ctx, key)
			return tiles, err
		}}
	} else {
		src = &publish.HTTPSource{Base: o.from, ReadToken: o.readToken}
	}

	// Target and state.
	var dst publish.Target
	stateDir := o.state
	if o.dir != "" {
		d, err := publish.NewDirTarget(o.dir)
		if err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		dst = d
		if stateDir == "" {
			stateDir = filepath.Join(d.Root, ".tsdb-publish")
		}
	} else {
		keyID := o.s3KeyID
		if keyID == "" {
			keyID = os.Getenv("AWS_ACCESS_KEY_ID")
		}
		secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
		if o.s3SecretFile != "" {
			b, err := os.ReadFile(o.s3SecretFile)
			if err != nil {
				return fmt.Errorf("publish: %w", err)
			}
			secret = strings.TrimSpace(string(b))
		}
		s3, err := publish.NewS3Target(publish.S3Config{
			Endpoint: o.s3Endpoint, Bucket: o.s3Bucket, Prefix: o.s3Prefix, VirtualHost: o.s3VirtualHost,
			Credentials: publish.Credentials{AccessKeyID: keyID, SecretAccessKey: secret, Region: o.s3Region},
		})
		if err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		dst = s3
		if stateDir == "" {
			h := sha256.Sum256([]byte(s3.ID()))
			stateDir = filepath.Join(o.db, "publish", "s3-"+hex.EncodeToString(h[:6]))
		}
	}
	state, err := publish.OpenState(stateDir, dst.ID(), o.reset)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	defer state.Close()

	// Viewer.
	var web fs.FS
	switch {
	case o.noViewer:
	case o.webDir != "":
		web = os.DirFS(o.webDir)
	case timeshadedb.WebBundleBuilt():
		if web, err = timeshadedb.WebDistFS(); err != nil {
			return err
		}
	default:
		logf("the binary has no viewer bundle (build web/ first, or pass --web-dir); exporting data only")
	}

	p, err := publish.New(src, dst, state, publish.Options{
		Encoding: o.encoding, GzipLevel: o.gzipLevel, KeyframeEvery: o.keyframeEvery,
		NoMovers: o.noMovers, NoOverlays: o.noOverlays, Web: web, LiveMs: o.liveMs,
		Workers: o.workers, MaxAttempts: o.maxAttempts, Logf: logf,
	})
	if err != nil {
		return err
	}
	defer p.Close()
	quietf("publishing to %s (state %s)", dst.ID(), stateDir)
	for {
		st, err := p.Round(ctx)
		fmt.Fprintf(stdout, "publish: %s\n", st)
		if o.every == 0 {
			if err != nil {
				return fmt.Errorf("publish: %w (the queue is kept; run publish again to resume)", err)
			}
			if n := st.Failed + st.Rejected; n > 0 {
				return fmt.Errorf("publish: %d objects could not be read or were refused", n)
			}
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logf("round failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(o.every):
		}
	}
}

func cmdPublishServe(ctx context.Context, args []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runPublishServe(ctx, os.Stdout, args)
}

// runPublishServe serves a publish --dir export with the headers the S3 target would
// store (publish.NewStaticHandler).
func runPublishServe(ctx context.Context, stdout io.Writer, args []string) error {
	f := flag.NewFlagSet("publish-serve", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	dir := f.String("dir", "", "directory written by publish --dir")
	listen := f.String("listen", "127.0.0.1:8080", "listen address; port 0 picks a free port (the first stdout line is \"listening on <addr>\")")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			f.SetOutput(stdout)
			fmt.Fprintln(stdout, "usage: timeshadedb publish-serve --dir <out> [--listen host:port]")
			f.PrintDefaults()
			return nil
		}
		return fmt.Errorf("publish-serve: %w", err)
	}
	if *dir == "" || f.NArg() != 0 {
		return errors.New("publish-serve: --dir is required")
	}
	if fi, err := os.Stat(*dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("publish-serve: %s is not a directory", *dir)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "listening on %s\n", ln.Addr())
	hs := &http.Server{Handler: publish.NewStaticHandler(*dir), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}
