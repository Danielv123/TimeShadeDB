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
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"timeshadedb/bench/internal/benchlib"
)

// Test-data denominators (research/testdata.md, PLAN.md §4.2): 7,483 charted nauvis chunks
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
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: measure run|v3disk|illisis|stream [flags]")
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
	res.Set("disk.store_mib", float64(d.Total-d.WAL)/(1<<20))
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
