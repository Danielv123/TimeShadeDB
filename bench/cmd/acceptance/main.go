// Command acceptance runs the whole WP11 suite and writes bench/REPORT.md. It is what
// bench/run_acceptance.sh and bench/run_acceptance.ps1 call.
//
// Two modes:
//
//	-mode candidate  (default) the v3 acceptance run: import the test data with
//	                 `timeshadedb import-minimap`, then disk, tile, ingest, startup,
//	                 attack, soak, train and client benchmarks against v3.
//	-mode baseline   the Wave 1 baselines: TimeShadeDB master (-master-bin) over the
//	                 same inputs, and the illisis viewer and export.
//
// Each step writes a result file under -results/<label>/; a failing step is logged and
// the run goes on, so the report shows NOT RUN for what could not be measured. Servers
// always listen on 127.0.0.1 with random ports.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"timeshadedb/bench/internal/benchlib"
)

type config struct {
	mode       string
	repo       string
	work       string
	results    string
	testdata   string
	stream     string
	bin        string
	masterBin  string
	importArgs string
	soak       time.Duration
	attackN    int
	webRuns    int
	skip       map[string]bool
	only       map[string]bool
	report     string
	strict     bool
	synthN     int
	moversN    int
}

func main() {
	var c config
	var skip, only string
	flag.StringVar(&c.mode, "mode", "candidate", "candidate (v3 acceptance) or baseline (master + illisis)")
	flag.StringVar(&c.repo, "repo", ".", "TimeShadeDB repository root")
	flag.StringVar(&c.work, "work", "", "scratch directory for binaries, databases and logs (default: <os temp>/tsdb-acceptance)")
	flag.StringVar(&c.results, "results", "", "result directory (default: bench/results for candidate, bench/baselines for baseline)")
	flag.StringVar(&c.testdata, "testdata", os.Getenv("TSDB_TESTDATA"), "illisis export root (env TSDB_TESTDATA)")
	flag.StringVar(&c.stream, "stream", os.Getenv("TSDB_STREAM"), "directory of chunk TSV files at 60 s windows (env TSDB_STREAM; default: made from -testdata by `measure stream` under -work)")
	flag.StringVar(&c.bin, "bin", os.Getenv("TSDB_BIN"), "v3 timeshadedb binary (default: built from -repo)")
	flag.StringVar(&c.masterBin, "master-bin", os.Getenv("TSDB_MASTER_BIN"), "TimeShadeDB master binary for -mode baseline")
	flag.StringVar(&c.importArgs, "import-args", envOr("TSDB_IMPORT_ARGS", "import-minimap --data {data} --db {db}"), "arguments of the import command ({data}, {db})")
	flag.DurationVar(&c.soak, "soak", 0, "soak duration (default 6h candidate, 10m baseline)")
	flag.IntVar(&c.attackN, "attack-n", 1000000, "hostile requests")
	flag.IntVar(&c.webRuns, "web-runs", 10, "cold and warm runs of the client benchmark")
	flag.IntVar(&c.synthN, "synth", 0, "synthetic chunk updates (default 1000000 candidate, 200000 baseline)")
	flag.IntVar(&c.moversN, "movers", 5000000, "synthetic mover samples (candidate)")
	flag.StringVar(&skip, "skip", "", "comma-separated steps to skip")
	flag.StringVar(&only, "only", "", "comma-separated steps to run (default all)")
	flag.StringVar(&c.report, "report", "bench/REPORT.md", "report file")
	flag.BoolVar(&c.strict, "strict", false, "exit 1 when a target fails or was not run")
	flag.Parse()
	c.skip, c.only = set(skip), set(only)
	repo, err := filepath.Abs(c.repo)
	check(err)
	c.repo = repo
	if c.work == "" {
		c.work = filepath.Join(os.TempDir(), "tsdb-acceptance")
	}
	c.work, _ = filepath.Abs(c.work)
	if c.results == "" {
		c.results = map[string]string{"candidate": "bench/results", "baseline": "bench/baselines"}[c.mode]
	}
	if !filepath.IsAbs(c.results) {
		c.results = filepath.Join(c.repo, c.results)
	}
	if c.soak == 0 {
		c.soak = map[string]time.Duration{"candidate": 6 * time.Hour, "baseline": 10 * time.Minute}[c.mode]
	}
	if c.synthN == 0 {
		c.synthN = map[string]int{"candidate": 1000000, "baseline": 200000}[c.mode]
	}
	for _, d := range []string{"bin", "db", "logs"} {
		check(os.MkdirAll(filepath.Join(c.work, d), 0o755))
	}
	// The tools shorten paths under these roots in the result files they write.
	os.Setenv("TSDB_WORK", c.work)
	if c.testdata != "" {
		os.Setenv("TSDB_TESTDATA", c.testdata)
	}
	if c.stream != "" {
		os.Setenv("TSDB_STREAM", c.stream)
	}
	r := &runner{c: c}
	switch c.mode {
	case "candidate":
		r.candidate()
	case "baseline":
		r.baseline()
	default:
		check(fmt.Errorf("unknown mode %q", c.mode))
	}
	r.step("report", func() error {
		args := []string{"-targets", filepath.Join(c.repo, "bench", "targets.json"),
			"-results", filepath.Join(c.repo, "bench", "baselines") + "," + filepath.Join(c.repo, "bench", "results"),
			"-candidate", "v3", "-out", abs(c.repo, c.report)}
		if c.strict {
			args = append(args, "-strict")
		}
		return r.run("report", r.tool("report"), args...)
	})
	fmt.Println()
	for _, s := range r.summary {
		fmt.Println(s)
	}
	if r.failed > 0 {
		fmt.Printf("%d step(s) failed; see %s\n", r.failed, filepath.Join(c.work, "logs"))
		if c.strict {
			os.Exit(1)
		}
	}
}

type runner struct {
	c       config
	summary []string
	failed  int
}

func (r *runner) want(name string) bool {
	if r.c.skip[name] {
		return false
	}
	return len(r.c.only) == 0 || r.c.only[name] || name == "build-tools" || name == "report"
}

// step runs fn, logging its duration and outcome.
func (r *runner) step(name string, fn func() error) {
	if !r.want(name) {
		r.summary = append(r.summary, fmt.Sprintf("  skip  %s", name))
		return
	}
	fmt.Printf("==> %s\n", name)
	start := time.Now()
	err := fn()
	d := time.Since(start).Round(time.Second)
	if err != nil {
		r.failed++
		r.summary = append(r.summary, fmt.Sprintf("  FAIL  %-22s %8s  %v", name, d, err))
		fmt.Printf("    failed: %v\n", err)
		return
	}
	r.summary = append(r.summary, fmt.Sprintf("  ok    %-22s %8s", name, d))
}

// run executes a command, teeing its output into logs/<log>.log.
func (r *runner) run(log, bin string, args ...string) error {
	return r.runIn("", log, bin, args...)
}

func (r *runner) runIn(dir, log, bin string, args ...string) error {
	lf, err := os.OpenFile(filepath.Join(r.c.work, "logs", log+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	fmt.Fprintf(lf, "\n$ %s %s\n", bin, strings.Join(args, " "))
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if dir == "" {
		cmd.Dir = r.c.repo
	}
	cmd.Stdout = io.MultiWriter(lf, os.Stdout)
	cmd.Stderr = io.MultiWriter(lf, os.Stderr)
	// Tied to this process, so an interrupted run leaves no tool (and, through the
	// tools' own ties, no server) behind.
	if err := benchlib.StartTied(cmd); err != nil {
		return err
	}
	return cmd.Wait()
}

func (r *runner) tool(name string) string {
	return filepath.Join(r.c.work, "bin", name+exe())
}

func (r *runner) out(label, name string) string {
	return filepath.Join(r.c.results, label, name+".json")
}

func (r *runner) freshDB(name string) string {
	p := filepath.Join(r.c.work, "db", name)
	os.RemoveAll(p)
	return p
}

func (r *runner) buildTools() {
	r.step("build-tools", func() error {
		return r.run("build", "go", "build", "-o", filepath.Join(r.c.work, "bin")+string(filepath.Separator), "./bench/cmd/...")
	})
}

func (r *runner) buildJS() {
	r.step("build-js", func() error {
		for _, d := range []string{"bench/trains", "bench/web"} {
			if err := r.runIn(filepath.Join(r.c.repo, d), "build", npm(), "ci", "--no-audit", "--no-fund"); err != nil {
				return err
			}
			if err := r.runIn(filepath.Join(r.c.repo, d), "build", "node", "build.mjs"); err != nil {
				return err
			}
		}
		return nil
	})
}

// makeStream writes the 60 s chunk stream from the test data unless -stream was given.
func (r *runner) makeStream() {
	r.step("stream", func() error {
		if r.c.stream != "" {
			return nil
		}
		if r.c.testdata == "" {
			return errors.New("neither -stream (TSDB_STREAM) nor -testdata (TSDB_TESTDATA) is set")
		}
		dir := filepath.Join(r.c.work, "stream60")
		os.RemoveAll(dir)
		if err := r.run("stream", r.tool("measure"), "stream", "-data", r.c.testdata, "-out", dir, "-coalesce", "60000"); err != nil {
			return err
		}
		r.c.stream = dir
		os.Setenv("TSDB_STREAM", dir)
		return nil
	})
}

func (r *runner) streamFiles(nauvisOnly bool) (files string, surface string, err error) {
	if r.c.stream == "" {
		return "", "", errors.New("no chunk stream: set -stream (TSDB_STREAM) or -testdata so the stream step can make one")
	}
	matches, _ := filepath.Glob(filepath.Join(r.c.stream, "*.chunks.tsv"))
	if len(matches) == 0 {
		return "", "", fmt.Errorf("no *.chunks.tsv in %s", r.c.stream)
	}
	if nauvisOnly {
		gw, _ := filepath.Glob(filepath.Join(r.c.stream, "gw-*.chunks.tsv"))
		if len(gw) == 0 {
			return "", "", errors.New("no gw-*.chunks.tsv (the gridworld map) in the stream directory")
		}
		return gw[0], "nauvis", nil
	}
	return strings.Join(matches, ","), "", nil
}

// server flags shared by the server-driving tools.
func srvArgs(target, label, bin, scenario, out string) []string {
	return []string{"-target", target, "-label", label, "-server-bin", bin, "-scenario", scenario, "-out", out}
}

// tenfold builds a database holding ten time-shifted replays of the test data stream and
// measures startup on it (the "10x" startup target).
func (r *runner) tenfold(target, label, bin string) error {
	files, surface, err := r.streamFiles(true)
	if err != nil {
		return err
	}
	db := r.freshDB(label + "-10x")
	const span = int64(8 * 24 * 3600 * 1000) // longer than the 169 h test data
	for k := 0; k < 10; k++ {
		scenario, out, restarts, settle := "synth10x-build", filepath.Join(r.c.work, "logs", fmt.Sprintf("%s-10x-%d.json", label, k)), "0", "0s"
		if k == 9 {
			scenario, out, restarts, settle = "synth10x", r.out(label, "startup-synth10x"), "3", "2s"
		}
		args := append(srvArgs(target, label, bin, scenario, out), "-db", db, "-input", files, "-surface", surface,
			"-time-shift-ms", strconv.FormatInt(int64(k)*span, 10), "-restarts", restarts, "-settle", settle)
		if err := r.run("synth10x", r.tool("ingestbench"), args...); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) candidate() {
	c := r.c
	r.buildTools()
	r.buildJS()
	r.makeStream()
	bin := c.bin
	r.step("build-server", func() error {
		if bin != "" {
			return nil
		}
		if err := r.runIn(filepath.Join(c.repo, "web"), "build", npm(), "ci", "--no-audit", "--no-fund"); err != nil {
			return err
		}
		if err := r.runIn(filepath.Join(c.repo, "web"), "build", npm(), "run", "build"); err != nil {
			return err
		}
		bin = filepath.Join(c.work, "bin", "timeshadedb"+exe())
		return r.run("build", "go", "build", "-o", bin, "./cmd/timeshadedb")
	})
	if bin == "" {
		bin = filepath.Join(c.work, "bin", "timeshadedb"+exe())
	}
	imported := filepath.Join(c.work, "db", "imported")
	r.step("import", func() error {
		if c.testdata == "" {
			return errors.New("-testdata (TSDB_TESTDATA) is not set")
		}
		os.RemoveAll(imported)
		args := []string{"run", "-name", "import", "-label", "v3", "-target", "v3", "-scenario", "testdata", "-out", r.out("v3", "import"), "--", bin}
		for _, a := range strings.Fields(c.importArgs) {
			args = append(args, strings.NewReplacer("{data}", c.testdata, "{db}", imported).Replace(a))
		}
		return r.run("import", r.tool("measure"), args...)
	})
	r.step("disk", func() error {
		if err := r.run("disk", r.tool("measure"), "v3disk", "-db", imported, "-label", "v3", "-scenario", "testdata", "-out", r.out("v3", "disk-testdata")); err != nil {
			return err
		}
		return r.run("disk", r.tool("measure"), "v3disk", "-db", imported, "-label", "v3", "-scenario", "testdata-all", "-out", r.out("v3", "disk-testdata-all"))
	})
	r.step("tiles", func() error {
		return r.run("tiles", r.tool("tilebench"), append(srvArgs("v3", "v3", bin, "testdata", r.out("v3", "tiles-testdata")), "-db", imported)...)
	})
	r.step("ingest-synth", func() error {
		return r.run("ingest", r.tool("ingestbench"), append(srvArgs("v3", "v3", bin, "synth", r.out("v3", "ingest-synth")),
			"-work", filepath.Join(c.work, "db"), "-synth", strconv.Itoa(c.synthN), "-movers", strconv.Itoa(c.moversN))...)
	})
	r.step("ingest-testdata", func() error {
		files, surface, err := r.streamFiles(true)
		if err != nil {
			return err
		}
		// Its own scenario: this database is the stream re-ingested over HTTP, not the
		// imported one that the disk, tile and startup targets grade.
		return r.run("ingest", r.tool("ingestbench"), append(srvArgs("v3", "v3", bin, "testdata-stream", r.out("v3", "ingest-testdata-stream")),
			"-work", filepath.Join(c.work, "db"), "-input", files, "-surface", surface)...)
	})
	r.step("synth10x", func() error { return r.tenfold("v3", "v3", bin) })
	r.step("attack", func() error {
		return r.run("attack", r.tool("attack"), append(srvArgs("v3", "v3", bin, "attack", r.out("v3", "attack")),
			"-db", imported, "-n", strconv.Itoa(c.attackN))...)
	})
	r.step("soak", func() error {
		return r.run("soak", r.tool("soak"), append(srvArgs("v3", "v3", bin, "soak", r.out("v3", "soak")),
			"-work", filepath.Join(c.work, "db"), "-duration", c.soak.String())...)
	})
	r.step("trains", func() error {
		if c.testdata == "" {
			return errors.New("-testdata (TSDB_TESTDATA) is not set")
		}
		return r.runIn(filepath.Join(c.repo, "bench", "trains"), "trains", "node", "dist/eval.mjs", "--data", c.testdata,
			"--renderer", "v3", "--label", "v3", "--out", r.out("v3", "trains"))
	})
	r.step("web", func() error {
		// The same view as the illisis run: gridworld nauvis, z0, centred on (0, 0).
		srv, err := benchlib.StartServer(benchlib.ServerSpec{Binary: bin,
			Args: []string{"serve-v3", "--db", "{db}", "--listen", "{listen}"}, DB: imported,
			Log: logFile(filepath.Join(c.work, "logs", "web-server.log"))})
		if err != nil {
			return err
		}
		defer srv.Stop()
		return r.runIn(filepath.Join(c.repo, "bench", "web"), "web", "node", "run.mjs", "--viewer", "v3", "--url", srv.BaseURL+"/",
			"--ds", webDatastore, "--x", "0", "--y", "0", "--z", "0",
			"--runs", strconv.Itoa(c.webRuns), "--label", "v3", "--out", r.out("v3", "web"))
	})
}

// webDatastore is the datastore both client benchmarks show (illisis: map
// gw-bc41437d~2E1, surface nauvis, force player).
const webDatastore = "gw-bc41437d~2E1.nauvis.player"

func logFile(path string) io.Writer {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return io.Discard
	}
	return f
}

func (r *runner) baseline() {
	c := r.c
	r.buildTools()
	r.buildJS()
	r.makeStream()
	mb := c.masterBin
	const label = "tsdb-master"
	needMaster := func() error {
		if mb == "" {
			return errors.New("-master-bin (TSDB_MASTER_BIN) is not set: build TimeShadeDB master (git archive master, add web/dist/index.html, go build ./cmd/timeshadedb)")
		}
		return nil
	}
	db1 := filepath.Join(c.work, "db", "master-testdata")
	r.step("ingest-testdata", func() error {
		if err := needMaster(); err != nil {
			return err
		}
		files, surface, err := r.streamFiles(true)
		if err != nil {
			return err
		}
		os.RemoveAll(db1)
		return r.run("ingest", r.tool("ingestbench"), append(srvArgs("v2", label, mb, "testdata", r.out(label, "ingest-testdata")),
			"-db", db1, "-input", files, "-surface", surface)...)
	})
	r.step("ingest-testdata-all", func() error {
		if err := needMaster(); err != nil {
			return err
		}
		files, _, err := r.streamFiles(false)
		if err != nil {
			return err
		}
		return r.run("ingest", r.tool("ingestbench"), append(srvArgs("v2", label, mb, "testdata-all", r.out(label, "ingest-testdata-all")),
			"-db", r.freshDB("master-all"), "-input", files, "-restarts", "1")...)
	})
	r.step("tiles", func() error {
		if err := needMaster(); err != nil {
			return err
		}
		return r.run("tiles", r.tool("tilebench"), append(srvArgs("v2", label, mb, "testdata", r.out(label, "tiles-testdata")),
			"-db", db1, "-t-first", "1791630273766")...)
	})
	r.step("ingest-synth", func() error {
		if err := needMaster(); err != nil {
			return err
		}
		return r.run("ingest", r.tool("ingestbench"), append(srvArgs("v2", label, mb, "synth", r.out(label, "ingest-synth")),
			"-work", filepath.Join(c.work, "db"), "-synth", strconv.Itoa(c.synthN), "-restarts", "1")...)
	})
	r.step("synth10x", func() error {
		if err := needMaster(); err != nil {
			return err
		}
		return r.tenfold("v2", label, mb)
	})
	r.step("attack", func() error {
		if err := needMaster(); err != nil {
			return err
		}
		// v2 leaks about 40 MB per hostile request: cap the run by memory and time.
		cp := r.freshDB("master-attack")
		if err := copyDir(db1, cp); err != nil {
			return err
		}
		return r.run("attack", r.tool("attack"), append(srvArgs("v2", label, mb, "attack", r.out(label, "attack")),
			"-db", cp, "-n", strconv.Itoa(c.attackN), "-max-rss-mib", "4096", "-max-duration", "2m")...)
	})
	r.step("soak", func() error {
		if err := needMaster(); err != nil {
			return err
		}
		return r.run("soak", r.tool("soak"), append(srvArgs("v2", label, mb, "soak", r.out(label, "soak")),
			"-work", filepath.Join(c.work, "db"), "-duration", c.soak.String(), "-synth-rate", "2", "-max-rss-mib", "8192")...)
	})
	r.step("illisis-disk", func() error {
		if c.testdata == "" {
			return errors.New("-testdata (TSDB_TESTDATA) is not set")
		}
		for _, sc := range []string{"testdata", "testdata-all"} {
			if err := r.run("disk", r.tool("measure"), "illisis", "-data", c.testdata, "-label", "illisis", "-scenario", sc, "-out", r.out("illisis", "disk-"+sc)); err != nil {
				return err
			}
		}
		return nil
	})
	r.step("trains", func() error {
		if c.testdata == "" {
			return errors.New("-testdata (TSDB_TESTDATA) is not set")
		}
		dir := filepath.Join(c.repo, "bench", "trains")
		if err := r.runIn(dir, "trains", "node", "dist/eval.mjs", "--data", c.testdata, "--renderer", "illisis", "--label", "illisis", "--out", r.out("illisis", "trains")); err != nil {
			return err
		}
		// No delay behind live: the next sample is unknown until it arrives, so the
		// viewer extrapolates (with a 1-2 s delay it behaves as in history playback,
		// because it only looks at the next sample in the last 500 ms before it).
		return r.runIn(dir, "trains", "node", "dist/eval.mjs", "--data", c.testdata, "--renderer", "illisis", "--label", "illisis-nodelay",
			"--lookahead-ms", "0", "--out", r.out("illisis-nodelay", "trains"))
	})
	r.step("web", func() error {
		if c.testdata == "" {
			return errors.New("-testdata (TSDB_TESTDATA) is not set")
		}
		return r.runIn(filepath.Join(c.repo, "bench", "web"), "web", "node", "run.mjs", "--viewer", "illisis", "--data", c.testdata,
			"--runs", strconv.Itoa(c.webRuns), "--label", "illisis", "--out", r.out("illisis", "web"))
	})
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func set(s string) map[string]bool {
	m := map[string]bool{}
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			m[x] = true
		}
	}
	return m
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func exe() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func npm() string {
	if runtime.GOOS == "windows" {
		return "npm.cmd"
	}
	return "npm"
}

func abs(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
