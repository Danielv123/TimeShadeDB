// Command measure records one-off measurements as result files for the report:
//
//	measure run    -name import -label v3 -out r.json -- timeshadedb import-minimap ...
//	    wall time and peak RSS of a command (metrics <name>.seconds, <name>.max_rss_mib)
//	measure v3disk -db <v3 db> [-ds <datastore id>] [-chunks N -hours H] [-mover-samples N]
//	    database size by category, B/chunk-hour (z0 and all terrain) and mover B/sample
//	measure illisis -data <export root> [-surface gw-bc41437d~2E1/nauvis/player]
//	    the same numbers for an illisis/minimap export (the test data), as a baseline
//	measure stream -data <export root> -out <dir> [-coalesce 60000]
//	    the chunk TSV stream (<map>.chunks.tsv) that ingestbench and soak replay
//	measure capture -repo <repo> [-runs 3] [-affinity FF00] [-- extra run_game.mjs args]
//	    the capture's UPS cost: runs `node capture/test/run_game.mjs --mode bench`
//	    (headless Factorio) and records its steady-state deltas per tick
//	measure movers -db <v3 db> [-surface gw-bc41437d~2E1.nauvis] -out journeys.json
//	    the train journeys stored in a v3 database, for the train evaluator's
//	    --journeys-json (the imported, re-timed rows instead of the illisis windows);
//	    -bodies adds each rail path row's body, as keyframes and live payloads give it
//	measure seekcheck -db <v3 db> [-surface ...] [-every 60000] [-out check.json]
//	    checks the bodies the mover store serves (keyframes, live payloads from disk and
//	    from memory) against those computed from the closed windows
//	measure replicate -db <v3 db> -out <new db> [-n 10]
//	    a database of n copies of a v3 database under suffixed map keys (the 10x database)
//	measure startup -server-bin <timeshadedb> -db <v3 db> [-restarts 3]
//	    median time from process start until serve answers (startup.ms)
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"timeshadedb/bench/internal/benchlib"
)

// Test-data denominators (research/testdata.md): 7,483 charted nauvis chunks
// over 169.1 h.
const (
	nauvisChunks = 7483
	nauvisHours  = 169.1
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "v3disk":
		cmdV3Disk(os.Args[2:])
	case "illisis":
		cmdIllisis(os.Args[2:])
	case "stream":
		cmdStream(os.Args[2:])
	case "capture":
		cmdCapture(os.Args[2:])
	case "movers":
		cmdMovers(os.Args[2:])
	case "seekcheck":
		cmdSeekCheck(os.Args[2:])
	case "replicate":
		cmdReplicate(os.Args[2:])
	case "startup":
		cmdStartup(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: measure run|v3disk|illisis|stream|capture|movers|seekcheck|replicate|startup [flags]")
	os.Exit(2)
}

type common struct {
	label, target, scenario, out string
}

func (c *common) register(fs *flag.FlagSet, defTarget string) {
	fs.StringVar(&c.label, "label", "", "system under test (default: target)")
	fs.StringVar(&c.target, "target", defTarget, "v2, v3 or illisis")
	fs.StringVar(&c.scenario, "scenario", "", "workload name")
	fs.StringVar(&c.out, "out", "", "result JSON file")
}

func (c *common) result(tool string) *benchlib.Result {
	l := c.label
	if l == "" {
		l = c.target
	}
	r := benchlib.NewResult(tool, l, c.target)
	r.Scenario = c.scenario
	return r
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs, "v3")
	name := fs.String("name", "run", "metric prefix")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		benchlib.Fatal(fmt.Errorf("measure run: command missing after --"))
	}
	res := c.result("measure-run")
	res.Param("command", strings.Join(rest, " "))
	cmd := exec.Command(rest[0], rest[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	start := time.Now()
	if err := benchlib.StartTied(cmd); err != nil {
		benchlib.Fatal(err)
	}
	sampler := benchlib.SampleMemory(cmd.Process.Pid, 100*time.Millisecond)
	err := cmd.Wait()
	el := time.Since(start)
	samples := sampler.Stop()
	res.Set(*name+".seconds", el.Seconds())
	res.Set(*name+".max_rss_mib", benchlib.MiB(benchlib.MaxRSS(samples)))
	if err != nil {
		res.Set(*name+".failed", 1)
		res.Note("command failed: %v", err)
	}
	if werr := res.Write(c.out); werr != nil {
		benchlib.Fatal(werr)
	}
	if err != nil {
		os.Exit(1)
	}
}

func cmdV3Disk(args []string) {
	fs := flag.NewFlagSet("v3disk", flag.ExitOnError)
	var c common
	c.register(fs, "v3")
	db := fs.String("db", "", "v3 database directory")
	ds := fs.String("ds", "gw-bc41437d~2E1.nauvis.player", "datastore id for the per-chunk-hour numbers")
	chunks := fs.Float64("chunks", nauvisChunks, "charted chunks of that datastore")
	hours := fs.Float64("hours", nauvisHours, "hours covered")
	samples := fs.Float64("mover-samples", 0, "stored mover samples for B/sample (default: counted from the closed windows in the database)")
	fs.Parse(args)
	d, err := benchlib.MeasureV3Disk(*db)
	if err != nil {
		benchlib.Fatal(err)
	}
	res := c.result("measure-v3disk")
	res.Param("db", *db)
	res.Param("ds", *ds)
	res.Set("disk.db_bytes", float64(d.Total))
	res.Set("disk.db_mib", float64(d.Total)/(1<<20))
	// Like the illisis figure: everything stored except the icons (and the WAL, which
	// is empty once the store is closed).
	res.Set("disk.store_mib", float64(d.Total-d.WAL-d.Icons)/(1<<20))
	res.Set("disk.icons_bytes", float64(d.Icons))
	res.Set("disk.terrain_z0_bytes", float64(d.TerrainZ0))
	res.Set("disk.pyramid_bytes", float64(d.Pyramid))
	res.Set("disk.terrain_meta_bytes", float64(d.TerrainMeta))
	res.Set("disk.wal_bytes", float64(d.WAL))
	res.Set("disk.movers_bytes", float64(d.Movers))
	res.Set("disk.overlays_bytes", float64(d.Overlays))
	ch := *chunks * *hours
	if z0, ok := d.ByDatastoreZ0[*ds]; ok && ch > 0 {
		res.Set("disk.b_per_chunk_hour_z0", float64(z0)/ch)
		res.Set("disk.b_per_chunk_hour_terrain", float64(d.ByDatastoreTerrain[*ds])/ch)
	} else {
		res.Note("datastore %s not found; B/chunk-hour skipped", *ds)
	}
	n := *samples
	if n <= 0 {
		rows, err := benchlib.CountV3MoverRows(*db)
		if err != nil {
			benchlib.Fatal(fmt.Errorf("counting mover samples: %w", err))
		}
		res.Set("disk.mover_windows", float64(rows.Windows))
		res.Set("disk.mover_gones", float64(rows.Gones))
		n = float64(rows.Samples)
		if rows.Windows > 0 {
			res.Note("mover samples counted from %d closed windows (keyframes not counted; open-window samples in the WAL are not counted)", rows.Windows)
		}
	}
	res.Set("disk.mover_samples", n)
	if n > 0 {
		// Like the illisis figure, the numerator holds every mover file (windows,
		// keyframes, day indexes) but not the WAL.
		res.Set("disk.mover_b_per_sample", float64(d.Movers)/n)
	} else if d.Movers > 0 {
		res.Note("no closed mover windows found; mover B/sample skipped")
	}
	if err := res.Write(c.out); err != nil {
		benchlib.Fatal(err)
	}
}

func cmdIllisis(args []string) {
	fs := flag.NewFlagSet("illisis", flag.ExitOnError)
	var c common
	c.register(fs, "illisis")
	data := fs.String("data", "", "illisis export root (holds maps.json.gz)")
	surface := fs.String("surface", "gw-bc41437d~2E1/nauvis/player", "map/surface_key/force_key for the per-chunk-hour number")
	chunks := fs.Float64("chunks", nauvisChunks, "charted chunks of that surface")
	hours := fs.Float64("hours", nauvisHours, "hours covered")
	fs.Parse(args)
	d, err := benchlib.MeasureIllisisDisk(*data, true)
	if err != nil {
		benchlib.Fatal(err)
	}
	res := c.result("measure-illisis")
	res.Param("data", *data)
	res.Set("disk.db_bytes", float64(d.Store))
	res.Set("disk.db_mib", float64(d.Store)/(1<<20))
	res.Set("disk.store_mib", float64(d.Store)/(1<<20))
	res.Set("disk.export_total_bytes", float64(d.Total))
	res.Set("disk.terrain_z0_bytes", float64(d.TerrainZ0))
	res.Set("disk.pyramid_bytes", float64(d.TerrainCoarse))
	res.Set("disk.movers_bytes", float64(d.Movers))
	res.Set("disk.overlays_bytes", float64(d.Overlays))
	res.Set("disk.mover_samples", float64(d.MoverSamples))
	res.Set("disk.mover_gones", float64(d.MoverGones))
	if d.MoverSamples > 0 {
		res.Set("disk.mover_b_per_sample", float64(d.Movers)/float64(d.MoverSamples))
	}
	if z0, ok := d.SurfaceZ0[*surface]; ok {
		res.Set("disk.b_per_chunk_hour_z0", float64(z0)/(*chunks**hours))
		// illisis keeps levels 1-4 for every surface together; the terrain figure adds
		// the surface's share of them by its z0 share.
		share := float64(z0) / float64(max(1, d.TerrainZ0))
		res.Set("disk.b_per_chunk_hour_terrain", (float64(z0)+share*float64(d.TerrainCoarse))/(*chunks**hours))
	} else {
		res.Note("surface %s not found", *surface)
	}
	if err := res.Write(c.out); err != nil {
		benchlib.Fatal(err)
	}
}

func cmdStream(args []string) {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	data := fs.String("data", "", "illisis export root (holds maps.json.gz)")
	out := fs.String("out", "", "output directory for <map>.chunks.tsv")
	coalesce := fs.Int64("coalesce", 60000, "window in ms: one row per changed chunk per window (0: every record, very large)")
	mapKey := fs.String("map", "", "only this map (export key, e.g. gw-bc41437d~2E1)")
	surface := fs.String("surface", "", "only this surface name")
	dropEnemy := fs.Bool("drop-enemy", false, "ignore pixel writes of the enemy colour 0xF8C3")
	fs.Parse(args)
	if *data == "" || *out == "" {
		benchlib.Fatal(fmt.Errorf("measure stream: -data and -out are required"))
	}
	start := time.Now()
	files, err := benchlib.WriteIllisisStream(*data, *out, benchlib.StreamOptions{
		CoalesceMs: *coalesce, Map: *mapKey, Surface: *surface, DropEnemy: *dropEnemy})
	if err != nil {
		benchlib.Fatal(err)
	}
	for _, f := range files {
		fmt.Printf("%s: %d chunk rows -> %s\n", f.Map, f.Rows, f.Path)
	}
	fmt.Printf("stream written in %.1fs\n", time.Since(start).Seconds())
}

// Lines printed by capture/test/run_game.mjs --mode bench (runBench).
var (
	captureMeanRE   = regexp.MustCompile(`bench: capture costs (-?[0-9.]+) ms/tick of Lua time \(scriptUpdate \+ luaGarbageIncremental, mean\), (-?[0-9.]+) ms/tick of the whole tick`)
	captureMedianRE = regexp.MustCompile(`bench: capture costs (-?[0-9.]+) ms/tick of Lua time by the medians`)
	captureResyncRE = regexp.MustCompile(`capture costs (-?[0-9.]+) ms/tick\s*$`)
	capturePairRE   = regexp.MustCompile(`bench pair \d+: capture Lua cost (-?[0-9.]+) ms/tick`)
	captureVersion  = regexp.MustCompile(`^Factorio (\S+) at `)
)

// parseCaptureBench reads the bench summary out of run_game.mjs's output.
func parseCaptureBench(out string, res *benchlib.Result) error {
	m := captureMeanRE.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("no \"bench: capture costs ... mean\" line in the output")
	}
	lua, _ := strconv.ParseFloat(m[1], 64)
	whole, _ := strconv.ParseFloat(m[2], 64)
	res.Set("capture.ms_per_tick", lua)
	res.Set("capture.whole_ms_per_tick", whole)
	if m := captureMedianRE.FindStringSubmatch(out); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		res.Set("capture.ms_per_tick_p50", v)
	}
	var pairs []float64
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := capturePairRE.FindStringSubmatch(line); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			pairs = append(pairs, v)
		}
		if strings.Contains(line, "during a resync") {
			if m := captureResyncRE.FindStringSubmatch(line); m != nil {
				v, _ := strconv.ParseFloat(m[1], 64)
				res.Set("capture.resync_ms_per_tick", v)
			}
		}
		if m := captureVersion.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			res.Param("factorio", m[1])
		}
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "WARN bench:") {
			res.Note("%s", t)
		}
	}
	if len(pairs) > 0 {
		lo, hi := pairs[0], pairs[0]
		for _, v := range pairs {
			lo, hi = min(lo, v), max(hi, v)
		}
		res.Set("capture.pairs", float64(len(pairs)))
		res.Set("capture.pair_min_ms_per_tick", lo)
		res.Set("capture.pair_max_ms_per_tick", hi)
	}
	return nil
}

func cmdCapture(args []string) {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	var c common
	c.register(fs, "v3")
	repo := fs.String("repo", ".", "TimeShadeDB repository root (holds capture/test/run_game.mjs)")
	runs := fs.Int("runs", 3, "interleaved pairs (with and without the capture)")
	affinity := fs.String("affinity", "", "Windows only: hex CPU mask to pin node and Factorio to (cmd start /affinity), e.g. FF00")
	host := fs.String("host", "mod", "mod (standalone mod) or clusterio (the save-patch module)")
	fs.Parse(args)
	if c.scenario == "" {
		c.scenario = "ups"
	}
	script := filepath.Join(*repo, "capture", "test", "run_game.mjs")
	nodeArgs := append([]string{script, "--mode", "bench", "--runs", strconv.Itoa(*runs), "--host", *host}, fs.Args()...)
	var cmd *exec.Cmd
	if *affinity != "" && runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", append([]string{"/c", "start", "", "/affinity", *affinity, "/wait", "/b", "node"}, nodeArgs...)...)
	} else {
		cmd = exec.Command("node", nodeArgs...)
	}
	cmd.Dir = filepath.Join(*repo, "capture", "test")
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	res := c.result("capture")
	res.Param("runs", *runs)
	res.Param("host", *host)
	res.Param("affinity", *affinity)
	if err := benchlib.StartTied(cmd); err != nil {
		benchlib.Fatal(err)
	}
	runErr := cmd.Wait()
	out := buf.String()
	if strings.Contains(out, "SKIP: no Factorio") {
		benchlib.Fatal(fmt.Errorf("measure capture: no Factorio found (set FACTORIO_EXE)"))
	}
	if err := parseCaptureBench(out, res); err != nil {
		benchlib.Fatal(fmt.Errorf("measure capture: %w (run_game.mjs: %v)", err, runErr))
	}
	if runErr != nil {
		res.Note("run_game.mjs exited with %v (it fails when a steady-state delta is over 0.3 ms/tick)", runErr)
	}
	if err := res.Write(c.out); err != nil {
		benchlib.Fatal(err)
	}
}
