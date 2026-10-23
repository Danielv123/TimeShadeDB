// Command ingestbench measures live ingest over HTTP: chunk updates and mover samples per
// second, whether the rate stays flat as history grows, server memory while ingesting,
// the database size per chunk-hour, and the restart time of the resulting database.
//
// Inputs are either chunk TSV files from `measure stream` (real test data)
// or the synthetic workload (benchlib.Synth). Targets: v2 (TimeShadeDB master's
// /api/ingest/chunk) and v3 (POST /api/v3/ingest with PROTOCOL.md envelopes).
//
//	ingestbench -target v2 -server-bin tsdb-master.exe -input nauvis.chunks.tsv -out r.json
//	ingestbench -target v3 -server-bin timeshadedb.exe -synth 1000000 -movers 5000000
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"timeshadedb/bench/internal/benchlib"
)

func main() {
	var sf benchlib.ServerFlags
	sf.Register(flag.CommandLine)
	input := flag.String("input", "", "comma-separated chunk TSV files (measure stream output); empty uses -synth")
	surface := flag.String("surface", "", "only rows of this surface (with -input)")
	synthN := flag.Int("synth", 0, "synthetic chunk updates to send (with no -input); default 200000")
	synthChunks := flag.Int("synth-chunks", 8000, "charted chunks in the synthetic workload")
	synthRate := flag.Float64("synth-rate", 200, "synthetic chunk updates per data second")
	movers := flag.Int("movers", 0, "synthetic mover samples to send after the chunks (v3 only)")
	trains := flag.Int("trains", 25, "synthetic trains")
	batch := flag.Int("batch", 4096, "chunk rows per request")
	moverBatch := flag.Int("mover-batch", 16384, "mover samples per request")
	conc := flag.Int("conc", 1, "parallel requests (1 keeps the stream ordered, like a forwarder)")
	workers := flag.Int("workers", 0, "client encoder goroutines (default NumCPU/2)")
	slices := flag.Int("slices", 10, "row slices for the flatness measurement")
	hours := flag.Float64("hours", 0, "override the time span (hours) used for B/chunk-hour")
	restarts := flag.Int("restarts", 3, "restart the server this many times after ingest to measure startup (0: skip)")
	shift := flag.Int64("time-shift-ms", 0, "add this to every input row time (replay the same data later, to grow a history)")
	settle := flag.Duration("settle", 2*time.Second, "wait after ingest before measuring disk and memory (lets windows flush)")
	allowRejected := flag.Bool("allow-rejected", false, "record rows the server did not store (late, invalid, bad lines) instead of failing; rates then count stored rows only")
	retryWait := flag.Duration("max-retry-wait", benchlib.MaxRetryWait, "longest total wait on 503 answers for one request")
	dataClock := flag.Bool("data-clock", true, "v3: start the server with --clock data (the map clock follows the replayed data; on the wall clock every replayed row is late)")
	flag.Parse()
	if *dataClock && sf.Target == "v3" && sf.Base == "" {
		sf.AddServerArgs("--clock data")
	}
	popt := benchlib.PipelineOptions{AllowRejected: *allowRejected, MaxRetryWait: *retryWait}

	res := sf.NewResult("ingestbench")
	if sf.Base == "" {
		if err := sf.EnsureDB("ingestbench-"); err != nil {
			benchlib.Fatal(err)
		}
		res.Param("db", sf.DB)
	}
	srv, base, err := sf.Start()
	if err != nil {
		benchlib.Fatal(err)
	}
	defer srv.Stop()
	if srv != nil {
		res.Set("startup.empty_ms", benchlib.Ms(srv.Startup))
	}
	client := benchlib.NewClient(base, *conc)
	client.Token = sf.Token
	target, err := benchlib.NewTarget(sf.Target, client)
	if err != nil {
		benchlib.Fatal(err)
	}
	var sampler *benchlib.MemSampler
	if srv != nil {
		sampler = benchlib.SampleMemory(srv.PID, 250*time.Millisecond)
	}
	ctx := context.Background()

	// Chunk source.
	var chunkRows []benchlib.ChunkRow
	var synth *benchlib.Synth
	totalChunks := 0
	if *input != "" {
		paths := strings.Split(*input, ",")
		res.Param("input", paths)
		res.Param("surface", *surface)
		chunkRows, err = benchlib.ReadAllTSV(paths, *surface)
		if err != nil {
			benchlib.Fatal(err)
		}
		if *shift != 0 {
			for i := range chunkRows {
				chunkRows[i].T += *shift
			}
			res.Param("time_shift_ms", *shift)
		}
		totalChunks = len(chunkRows)
	} else {
		n := *synthN
		if n == 0 {
			n = 200000
		}
		cfg := benchlib.DefaultSynth()
		cfg.Chunks, cfg.UpdateRate, cfg.Trains = *synthChunks, *synthRate, *trains
		synth = benchlib.NewSynth(cfg)
		totalChunks = n
		res.Param("synth", map[string]any{"updates": n, "chunks": cfg.Chunks, "hot_fraction": cfg.HotFraction,
			"hot_share": cfg.HotShare, "update_rate": cfg.UpdateRate, "trains": cfg.Trains})
	}
	res.Param("batch", *batch)
	res.Param("conc", *conc)

	sent := 0
	gen := func() ([]benchlib.ChunkRow, bool) {
		if sent >= totalChunks {
			return nil, false
		}
		n := min(*batch, totalChunks-sent)
		var rows []benchlib.ChunkRow
		if synth != nil {
			rows = synth.NextChunks(n)
		} else {
			rows = chunkRows[sent : sent+n]
		}
		sent += n
		return rows, true
	}
	var memBefore benchlib.MemInfo
	if srv != nil {
		memBefore, _ = srv.Memory()
		res.Set("mem.start_rss_mib", benchlib.MiB(memBefore.RSS))
	}
	st, err := benchlib.SendPipeline(ctx, client, *conc, *workers, popt, gen, target.EncodeChunks)
	if err != nil {
		benchlib.Fatal(fmt.Errorf("chunk ingest: %w", err))
	}
	res.Set("ingest.chunk_rows", float64(st.Rows))
	res.Set("ingest.chunk_requests", float64(st.Requests))
	res.Set("ingest.chunk_seconds", st.Wall.Seconds())
	res.Set("ingest.chunk_rows_per_s", st.StoredRate())
	res.Set("ingest.chunk_client_starved_s", st.Starved.Seconds())
	res.Set("ingest.chunk_retries_503", float64(st.Retries))
	res.Set("ingest.chunk_retry_wait_s", st.RetryWait.Seconds())
	recordOutcome(res, "ingest.chunk", st)
	res.SetSummary("ingest.chunk_request_ms", benchlib.Summarize(st.Latencies))
	rates := st.SliceRates(*slices)
	for i, r := range rates {
		res.Point("ingest.chunk_slice_rows_per_s", float64(i), r)
	}
	if f := benchlib.Flatness(rates); f > 0 {
		res.Set("ingest.chunk_flatness", f)
	}
	if st.Starved > st.Wall/4 {
		res.Note("chunk ingest: senders waited %.1fs of %.1fs for the client encoder; the rate may be client-bound", st.Starved.Seconds(), st.Wall.Seconds())
	}
	var rowsMem [][2]float64
	if sampler != nil {
		for _, s := range sampler.Samples() {
			rowsMem = append(rowsMem, [2]float64{s.T, benchlib.MiB(s.Mem.RSS)})
		}
	}

	// Movers.
	if *movers > 0 {
		if synth == nil {
			cfg := benchlib.DefaultSynth()
			cfg.Chunks, cfg.Trains = 1, *trains
			synth = benchlib.NewSynth(cfg)
		} else {
			// The chunks moved a data clock to their last time; continue from there.
			synth.AlignMovers()
		}
		msent := 0
		mgen := func() ([]benchlib.MoverRow, bool) {
			if msent >= *movers {
				return nil, false
			}
			n := min(*moverBatch, *movers-msent)
			msent += n
			return synth.NextMovers(n), true
		}
		mst, err := benchlib.SendPipeline(ctx, client, *conc, *workers, popt, mgen, target.EncodeMovers)
		switch {
		case err == benchlib.ErrUnsupported || (err != nil && strings.Contains(err.Error(), benchlib.ErrUnsupported.Error())):
			res.Note("mover ingest: not supported by target %s", sf.Target)
		case err != nil:
			benchlib.Fatal(fmt.Errorf("mover ingest: %w", err))
		default:
			res.Set("ingest.mover_samples", float64(mst.Rows))
			res.Set("ingest.mover_seconds", mst.Wall.Seconds())
			res.Set("ingest.mover_samples_per_s", mst.StoredRate())
			res.Set("ingest.mover_retries_503", float64(mst.Retries))
			res.Set("ingest.mover_retry_wait_s", mst.RetryWait.Seconds())
			recordOutcome(res, "ingest.mover", mst)
			res.Set("ingest.mover_client_starved_s", mst.Starved.Seconds())
			mr := mst.SliceRates(*slices)
			for i, r := range mr {
				res.Point("ingest.mover_slice_samples_per_s", float64(i), r)
			}
			if f := benchlib.Flatness(mr); f > 0 {
				res.Set("ingest.mover_flatness", f)
			}
		}
	}

	time.Sleep(*settle)
	if sampler != nil {
		samples := sampler.Stop()
		for _, s := range samples {
			res.Point("mem.rss_mib", s.T, benchlib.MiB(s.Mem.RSS))
		}
		res.Set("mem.max_rss_mib", benchlib.MiB(benchlib.MaxRSS(samples)))
		if mi, err := srv.Memory(); err == nil {
			res.Set("mem.final_rss_mib", benchlib.MiB(mi.RSS))
			res.Set("mem.final_private_mib", benchlib.MiB(mi.Private))
			if st.Rows > 0 {
				res.Set("mem.rss_growth_kib_per_chunk_row", float64(int64(mi.RSS)-int64(memBefore.RSS))/1024/float64(st.Rows))
			}
		}
		_ = rowsMem
	}

	// Disk and restart.
	if srv != nil {
		if err := srv.Stop(); err != nil {
			res.Note("stop: %v", err)
		}
		total, byTop, err := benchlib.DirSize(sf.DB)
		if err != nil {
			benchlib.Fatal(err)
		}
		res.Set("disk.db_bytes", float64(total))
		res.Set("disk.db_mib", float64(total)/(1<<20))
		for k, v := range byTop {
			res.Set("disk.top."+k, float64(v))
		}
		if chunkRows != nil {
			chunks, h := benchlib.ChunkHours(chunkRows)
			if *hours > 0 {
				h = *hours
			}
			res.Set("disk.chunks", float64(chunks))
			res.Set("disk.hours", h)
			if chunks > 0 && h > 0 {
				ch := float64(chunks) * h
				res.Set("disk.b_per_chunk_hour_all", float64(total)/ch)
				if sf.Target == "v2" {
					// v2 stores only z0 terrain (no pyramid), all of it under datastores/.
					res.Set("disk.b_per_chunk_hour_z0", float64(byTop["datastores"])/ch)
					res.Set("disk.b_per_chunk_hour_terrain", float64(byTop["datastores"])/ch)
					res.Set("disk.store_mib", float64(total)/(1<<20))
				} else if d, err := benchlib.MeasureV3Disk(sf.DB); err == nil {
					// The chunk-hour denominator covers every datastore in the input, so
					// the numerator is all of them too.
					res.Set("disk.b_per_chunk_hour_z0", float64(d.TerrainZ0)/ch)
					res.Set("disk.b_per_chunk_hour_terrain", float64(d.TerrainZ0+d.Pyramid+d.TerrainMeta)/ch)
					res.Set("disk.store_mib", float64(d.Total-d.WAL-d.Icons)/(1<<20))
					res.Set("disk.wal_bytes", float64(d.WAL))
				}
			}
		}
		var ms []float64
		for i := 0; i < *restarts; i++ {
			spec, _ := sf.Spec()
			spec.Log = io.Discard
			s2, err := benchlib.StartServer(spec)
			if err != nil {
				res.Note("restart %d failed: %v", i+1, err)
				break
			}
			ms = append(ms, benchlib.Ms(s2.Startup))
			if i == 0 {
				if mi, err := s2.Memory(); err == nil {
					res.Set("mem.restart_rss_mib", benchlib.MiB(mi.RSS))
				}
			}
			s2.Stop()
		}
		if len(ms) > 0 {
			res.Set("startup.ms", benchlib.Median(ms))
			res.Set("startup.db_mib", float64(total)/(1<<20))
		}
	}
	if err := res.Write(sf.Out); err != nil {
		benchlib.Fatal(err)
	}
}

// recordOutcome stores the server's account of the rows: stored, late, invalid and bad
// lines (all rows count as stored for v2, which rejects bad rows with an error status).
func recordOutcome(res *benchlib.Result, prefix string, st benchlib.SendStats) {
	o := st.Outcome
	res.Set(prefix+"_stored", float64(o.Accepted))
	if o.Late+o.Invalid+o.BadLines > 0 {
		res.Set(prefix+"_late", float64(o.Late))
		res.Set(prefix+"_invalid", float64(o.Invalid))
		res.Set(prefix+"_bad_lines", float64(o.BadLines))
		res.Note("%s: the server did not store %d of %d rows (late %d, invalid %d, bad lines %d); rates count stored rows",
			prefix, st.Rows-o.Accepted, st.Rows, o.Late, o.Invalid, o.BadLines)
	}
}
