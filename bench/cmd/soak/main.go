// Command soak replays a workload at a fixed speed-up for a fixed wall time while
// sampling server memory, and reports whether memory stays bounded as history grows
// (PLAN.md §5 "Server RSS": <= mem-budget + 64 MB at any history length, growth < 5%
// between hour 1 and hour 6 of a soak at 100x replay).
//
// "Hour 1" generalises to the end of the first sixth of the run, so a short run reports
// the same ratio. The workload is the synthetic one (terrain flicker plus trains on v3)
// or a chunk TSV replayed in a loop with times shifted by the file's span each pass, so
// the history keeps growing. A few tiles are read every second, like a viewer.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"timeshadedb/bench/internal/benchlib"
)

func main() {
	var sf benchlib.ServerFlags
	sf.Register(flag.CommandLine)
	duration := flag.Duration("duration", 10*time.Minute, "wall time of the soak")
	speed := flag.Float64("speed", 100, "data time per wall time")
	input := flag.String("input", "", "comma-separated chunk TSV files to replay in a loop (default: synthetic)")
	surface := flag.String("surface", "", "only rows of this surface (with -input)")
	synthRate := flag.Float64("synth-rate", 20, "synthetic chunk updates per data second")
	synthChunks := flag.Int("synth-chunks", 8000, "charted chunks in the synthetic workload")
	trains := flag.Int("trains", 25, "synthetic trains (v3 only)")
	sampleMs := flag.Int64("train-sample-ms", 300, "data ms between samples of one train")
	tick := flag.Duration("tick", 250*time.Millisecond, "send interval")
	slice := flag.Duration("slice", 30*time.Second, "after a stall, catch up in data slices of at most this much data time, terrain then movers for each, so the two streams stay in time order")
	readTiles := flag.Int("read-tiles", 8, "tiles read per second (0: none)")
	memEvery := flag.Duration("mem-every", 5*time.Second, "memory sampling interval")
	maxRSS := flag.Float64("max-rss-mib", 8192, "stop when the server RSS exceeds this")
	allowRejected := flag.Bool("allow-rejected", false, "record rows the server did not store instead of failing")
	slowLog := flag.Duration("server-slow-log", time.Second, "v3: start the server with --slow-log (0: off)")
	dataClock := flag.Bool("data-clock", true, "v3: start the server with --clock data and place the replayed data so it ends now")
	flag.Parse()
	ing := soakIngest{allowRejected: *allowRejected}
	// A v3 server on the wall clock counts replayed history as late, and on the data
	// clock it never moves past the wall clock (+10 s). So the soak's data is placed to
	// end at the present: it starts duration*speed (plus an hour of margin) ago.
	useDataClock := *dataClock && sf.Target == "v3"
	if useDataClock && sf.Base == "" {
		sf.AddServerArgs("--clock data")
	}
	if sf.Target == "v3" && sf.Base == "" && *slowLog > 0 {
		// Slow requests and commit steps go to the server log (-server-log), to explain a stall.
		sf.AddServerArgs("--slow-log " + slowLog.String())
	}
	placedStart := time.Now().UnixMilli() - int64(duration.Seconds()*(*speed)*1000) - 3600_000

	res := sf.NewResult("soak")
	res.Param("duration_s", duration.Seconds())
	res.Param("speed", *speed)
	if sf.Base == "" {
		if err := sf.EnsureDB("soak-"); err != nil {
			benchlib.Fatal(err)
		}
	}
	res.Param("db", sf.DB)
	srv, base, err := sf.Start()
	if err != nil {
		benchlib.Fatal(err)
	}
	defer srv.Stop()
	if srv == nil {
		res.Note("no server process: memory cannot be sampled with -base")
	}
	client := benchlib.NewClient(base, 4)
	client.Token = sf.Token
	target, err := benchlib.NewTarget(sf.Target, client)
	if err != nil {
		benchlib.Fatal(err)
	}
	ctx := context.Background()

	// Workload.
	var file []benchlib.ChunkRow
	var span int64
	var synth *benchlib.Synth
	cfg := benchlib.DefaultSynth()
	cfg.Chunks, cfg.UpdateRate, cfg.Trains, cfg.TrainSampleMs = *synthChunks, *synthRate, *trains, *sampleMs
	if *input != "" {
		file, err = benchlib.ReadAllTSV(strings.Split(*input, ","), *surface)
		if err != nil || len(file) == 0 {
			benchlib.Fatal(fmt.Errorf("input: %v (%d rows)", err, len(file)))
		}
		span = file[len(file)-1].T - file[0].T + 60000
		if useDataClock {
			off := placedStart - file[0].T
			for i := range file {
				file[i].T += off
			}
		}
		cfg.Start = file[0].T
		res.Param("input", *input)
	} else if useDataClock {
		cfg.Start = placedStart
	}
	if useDataClock {
		res.Param("clock", "data")
		res.Param("data_start_ms", cfg.Start)
	}
	synth = benchlib.NewSynth(cfg)
	res.Param("synth", map[string]any{"chunks": cfg.Chunks, "update_rate": cfg.UpdateRate, "trains": cfg.Trains})
	dataStart := cfg.Start
	fileIdx, pass := 0, 0
	moversOK := sf.Target == "v3" && *trains > 0

	var sampler *benchlib.MemSampler
	if srv != nil {
		sampler = benchlib.SampleMemory(srv.PID, *memEvery)
	}
	rng := rand.New(rand.NewSource(1))
	var tileLat []float64
	var dsList []benchlib.DSInfo
	rows, samples, tileErrs := 0, 0, 0
	start := time.Now()
	lastRead := start
	stopReason := "done"
	ticker := time.NewTicker(*tick)
	defer ticker.Stop()
	var maxStall time.Duration
	lastTick := start
	sent := dataStart
	for time.Since(start) < *duration {
		<-ticker.C
		if gap := time.Since(lastTick); gap > maxStall {
			maxStall = gap
		}
		lastTick = time.Now()
		until := dataStart + int64(time.Since(start).Seconds()*(*speed)*1000)
		// A stall (of this loop or of the server's answers) leaves a lot of data time to
		// send. Sending all terrain and then all movers would put the mover rows behind
		// the clock the terrain moved, by up to the stall times the speed-up: send it in
		// slices instead, in time order, as a capture would have.
		for sent < until {
			dataNow := min(until, sent+slice.Milliseconds())
			sent = dataNow
			// Terrain up to dataNow.
			var batch []benchlib.ChunkRow
			if file != nil {
				for {
					r := file[fileIdx]
					t := r.T + int64(pass)*span
					if t > dataNow {
						break
					}
					r.T = t
					batch = append(batch, r)
					fileIdx++
					if fileIdx == len(file) {
						fileIdx, pass = 0, pass+1
					}
				}
			} else {
				for synth.DataTime() < dataNow {
					batch = append(batch, synth.NextChunks(1)...)
				}
			}
			if err := post(ctx, client, target.EncodeChunks, batch, &ing); err != nil {
				benchlib.Fatal(fmt.Errorf("chunks: %w", err))
			}
			rows += len(batch)
			// Movers up to dataNow.
			if moversOK {
				n := 0
				if mt := synth.MoverTime(); mt < dataNow {
					n = int((dataNow-mt+cfg.TrainSampleMs-1)/cfg.TrainSampleMs) * cfg.Trains
				}
				if n > 0 {
					mb := synth.NextMovers(n)
					if err := post(ctx, client, target.EncodeMovers, mb, &ing); err != nil {
						benchlib.Fatal(fmt.Errorf("movers: %w", err))
					}
					samples += len(mb)
				}
			}
		}
		// A viewer reading tiles.
		if *readTiles > 0 && time.Since(lastRead) >= time.Second {
			lastRead = time.Now()
			if dsList == nil {
				dsList, _ = target.Datastores(ctx)
			}
			if len(dsList) > 0 {
				ds := dsList[rng.Intn(len(dsList))]
				refs, _, err := target.Tiles(ctx, ds, 0, nil)
				if err == nil {
					for i := 0; i < *readTiles && len(refs) > 0; i++ {
						r, err := client.Get(ctx, refs[rng.Intn(len(refs))].URL, false, nil)
						if err != nil || r.Status != 200 {
							tileErrs++
							continue
						}
						tileLat = append(tileLat, benchlib.Ms(r.Dur))
					}
				}
			}
			if time.Since(start) > 30*time.Second {
				dsList = nil // refresh the catalog now and then
			}
		}
		if srv != nil {
			if srv.Exited() {
				stopReason = "server exited"
				break
			}
			if mi, err := srv.Memory(); err == nil && benchlib.MiB(mi.RSS) > *maxRSS {
				stopReason = fmt.Sprintf("server RSS %.0f MiB passed -max-rss-mib", benchlib.MiB(mi.RSS))
				break
			}
		}
	}
	elapsed := time.Since(start)
	res.Set("soak.seconds", elapsed.Seconds())
	res.Set("soak.data_hours", elapsed.Seconds()*(*speed)/3600)
	res.Set("soak.chunk_rows", float64(rows))
	res.Set("soak.mover_samples", float64(samples))
	res.Set("soak.rows_stored", float64(ing.outcome.Accepted))
	res.Set("soak.retries_503", float64(ing.retries))
	res.Set("soak.retry_wait_s", ing.wait.Seconds())
	res.Set("soak.max_stall_s", maxStall.Seconds())
	if maxStall > 2**tick+time.Second {
		res.Note("longest gap between send rounds: %.1f s (sent afterwards in %s data slices)", maxStall.Seconds(), *slice)
	}
	if o := ing.outcome; o.Late+o.Invalid+o.BadLines > 0 {
		res.Note("server did not store some rows: late %d, invalid %d, bad lines %d", o.Late, o.Invalid, o.BadLines)
	}
	res.SetSummary("soak.tile_ms", benchlib.Summarize(tileLat))
	if tileErrs > 0 {
		res.Set("soak.tile_errors", float64(tileErrs))
	}
	if stopReason != "done" {
		res.Note("stopped early: %s", stopReason)
	}
	if sampler != nil {
		ms := sampler.Stop()
		for _, s := range ms {
			res.Point("mem.rss_mib", s.T, benchlib.MiB(s.Mem.RSS))
		}
		res.Set("mem.max_rss_mib", benchlib.MiB(benchlib.MaxRSS(ms)))
		if len(ms) >= 6 {
			total := ms[len(ms)-1].T
			var p1 benchlib.MemSample
			for _, s := range ms {
				if s.T <= total/6 {
					p1 = s
				}
			}
			end := ms[len(ms)-1]
			res.Set("soak.rss_first_sixth_mib", benchlib.MiB(p1.Mem.RSS))
			res.Set("soak.rss_end_mib", benchlib.MiB(end.Mem.RSS))
			if p1.Mem.RSS > 0 {
				res.Set("soak.rss_growth_ratio", float64(end.Mem.RSS)/float64(p1.Mem.RSS)-1)
			}
			var pts [][2]float64
			for _, s := range ms {
				if s.T >= total/6 {
					pts = append(pts, [2]float64{s.T, benchlib.MiB(s.Mem.RSS)})
				}
			}
			res.Set("soak.rss_slope_mib_per_hour", benchlib.LinearSlope(pts)*3600)
		} else {
			res.Note("too few memory samples (%d) for the growth ratio", len(ms))
		}
	}
	if srv != nil {
		srv.Stop()
		if total, _, err := benchlib.DirSize(sf.DB); err == nil {
			res.Set("disk.db_bytes", float64(total))
		}
	}
	if err := res.Write(sf.Out); err != nil {
		benchlib.Fatal(err)
	}
}

// post encodes and sends a batch. Every row must be stored: a v3 server answers 200 even
// when it rejected rows, so the IngestResponse counts are checked.
func post[B any](ctx context.Context, c *benchlib.Client, enc func(B) ([]benchlib.Body, error), batch B, st *soakIngest) error {
	bodies, err := enc(batch)
	if err != nil {
		return err
	}
	for _, b := range bodies {
		_, o, retries, waited, err := benchlib.PostIngest(ctx, c, b, benchlib.MaxRetryWait)
		st.retries += retries
		st.wait += waited
		if err != nil {
			return err
		}
		st.outcome.Add(o)
		if !st.allowRejected {
			if err := o.Shortfall(b.Rows); err != nil {
				return err
			}
		}
	}
	return nil
}

type soakIngest struct {
	allowRejected bool
	retries       int
	wait          time.Duration
	outcome       benchlib.IngestOutcome
}
