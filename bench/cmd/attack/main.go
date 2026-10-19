// Command attack sends random hostile tile requests (random levels and coordinates,
// unknown datastores, missing versions) and measures how much server memory they leave
// behind and how fast they are answered (PLAN.md §4.3 G2, §5 "Hostile tile requests":
// 1 M random coordinates add < 2 MB, each answered in < 1 ms).
//
// A guard stops the run when the server's RSS passes -max-rss-mib or after
// -max-duration; the growth per million requests is then extrapolated and flagged.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"timeshadedb/bench/internal/benchlib"
)

func main() {
	var sf benchlib.ServerFlags
	sf.Register(flag.CommandLine)
	n := flag.Int("n", 1000000, "hostile requests to send")
	conc := flag.Int("conc", 8, "requests in flight")
	maxDur := flag.Duration("max-duration", 10*time.Minute, "stop after this long")
	maxRSS := flag.Float64("max-rss-mib", 8192, "stop when the server RSS exceeds this (protects the machine)")
	warm := flag.Int("warm", 2000, "valid-looking warm-up requests before the baseline memory reading")
	seed := flag.Int64("seed", 1, "random seed")
	flag.Parse()

	res := sf.NewResult("attack")
	if sf.Base == "" && sf.DB == "" {
		if err := sf.EnsureDB("attack-"); err != nil {
			benchlib.Fatal(err)
		}
	}
	res.Param("db", sf.DB)
	srv, base, err := sf.Start()
	if err != nil {
		benchlib.Fatal(err)
	}
	defer srv.Stop()
	client := benchlib.NewClient(base, *conc)
	client.Token = sf.Token
	target, err := benchlib.NewTarget(sf.Target, client)
	if err != nil {
		benchlib.Fatal(err)
	}
	ctx := context.Background()
	known, err := target.Datastores(ctx)
	if err != nil {
		res.Note("catalog: %v (attacking without known datastores)", err)
	}
	res.Param("known_datastores", len(known))

	rng := rand.New(rand.NewSource(*seed))
	var rngMu sync.Mutex
	nextURL := func() string {
		rngMu.Lock()
		defer rngMu.Unlock()
		return target.HostileURL(rng, known)
	}

	// Warm-up: let pools, caches and the runtime settle so the baseline is fair. The
	// warm-up requests are catalog reads, which every server must handle anyway.
	for i := 0; i < *warm; i++ {
		path := "/api/v3/catalog"
		if sf.Target == "v2" {
			path = "/api/chunk/saves"
		}
		if _, err := client.Get(ctx, path, false, nil); err != nil {
			benchlib.Fatal(err)
		}
	}
	time.Sleep(time.Second)
	var before benchlib.MemInfo
	if srv != nil {
		before, _ = srv.Memory()
		res.Set("attack.rss_before_mib", benchlib.MiB(before.RSS))
	}

	var sent, stopped atomic.Int64
	var mu sync.Mutex
	lat := make([]float64, 0, min(*n, 2000000))
	status := map[int]int{}
	errs := 0
	deadline := time.Now().Add(*maxDur)
	stopReason := ""
	var stopOnce sync.Once
	stop := func(why string) {
		stopOnce.Do(func() {
			stopReason = why
			stopped.Store(1)
		})
	}
	if srv != nil {
		go func() {
			for stopped.Load() == 0 {
				if mi, err := srv.Memory(); err == nil && benchlib.MiB(mi.RSS) > *maxRSS {
					stop(fmt.Sprintf("server RSS %.0f MiB passed -max-rss-mib", benchlib.MiB(mi.RSS)))
				}
				if srv.Exited() {
					stop("server exited")
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for stopped.Load() == 0 {
				if sent.Add(1) > int64(*n) {
					return
				}
				if time.Now().After(deadline) {
					stop("max duration reached")
					return
				}
				r, err := client.Get(ctx, nextURL(), false, nil)
				mu.Lock()
				if err != nil {
					errs++
				} else {
					status[r.Status]++
					lat = append(lat, benchlib.Ms(r.Dur))
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	stop("done")
	elapsed := time.Since(start)
	done := len(lat) + errs
	res.Set("attack.requests", float64(done))
	res.Set("attack.seconds", elapsed.Seconds())
	res.Set("attack.errors", float64(errs))
	s := benchlib.Summarize(lat)
	res.SetSummary("attack.latency_ms", s)
	over := 0
	for _, l := range lat {
		if l >= 1 {
			over++
		}
	}
	if len(lat) > 0 {
		res.Set("attack.share_over_1ms", float64(over)/float64(len(lat)))
	}
	var c2, c4, c5 int
	for code, k := range status {
		switch {
		case code < 300:
			c2 += k
		case code < 500:
			c4 += k
		default:
			c5 += k
		}
	}
	res.Set("attack.status_2xx", float64(c2))
	res.Set("attack.status_4xx", float64(c4))
	res.Set("attack.status_5xx", float64(c5))
	if stopReason != "done" {
		res.Note("stopped early after %d of %d requests: %s", done, *n, stopReason)
	}
	if srv != nil {
		time.Sleep(2 * time.Second)
		if after, err := srv.Memory(); err == nil {
			growth := benchlib.MiB(after.RSS) - benchlib.MiB(before.RSS)
			res.Set("attack.rss_after_mib", benchlib.MiB(after.RSS))
			res.Set("attack.rss_growth_mib", growth)
			if done > 0 {
				per1M := growth * 1e6 / float64(done)
				res.Set("attack.rss_growth_mib_per_1m", per1M)
				if done < 1000000 {
					res.Note("rss_growth_mib_per_1m is extrapolated linearly from %d requests", done)
				}
			}
		} else {
			res.Note("memory after: %v", err)
		}
	}
	if err := res.Write(sf.Out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
