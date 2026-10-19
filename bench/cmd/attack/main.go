// Command attack sends random hostile tile requests (random levels and coordinates,
// unknown datastores, missing versions) and measures how much server memory they leave
// behind and how fast they are answered (PLAN.md §4.3 G2, §5 "Hostile tile requests":
// 1 M random coordinates add < 2 MB, each answered in < 1 ms).
//
// Memory is read several times before and after (the median of -mem-readings, after
// -settle), as the commit charge (private bytes, the target metric), as RSS, and as the
// Go heap from /metrics when the server exports go_memstats_heap_inuse_bytes. A single
// working-set reading moves by more than the 2 MiB budget on its own.
//
// A guard stops the run when the server's RSS passes -max-rss-mib or after
// -max-duration. The growth per million requests is extrapolated only from at least
// -min-extrapolate requests, or when the growth is already far past the budget
// (-clear-growth-mib); both cases are noted in the result.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
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
	readings := flag.Int("mem-readings", 5, "memory readings before and after (the median is used)")
	settle := flag.Duration("settle", 2*time.Second, "wait before the readings after the run")
	minExtrap := flag.Int("min-extrapolate", 100000, "fewest requests to extrapolate the growth per million from")
	clearGrowth := flag.Float64("clear-growth-mib", 64, "growth (MiB) that is reported per million even from fewer requests, because it is far past the budget")
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
	var before memReading
	if srv != nil {
		before = readMem(ctx, srv, client, sf.Target, *readings)
		res.Set("attack.rss_before_mib", before.rss)
		res.Set("attack.private_before_mib", before.private)
		if before.heap >= 0 {
			res.Set("attack.heap_before_mib", before.heap)
		}
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
		time.Sleep(*settle)
		if srv.Exited() {
			res.Note("server exited; no memory after")
		} else {
			after := readMem(ctx, srv, client, sf.Target, *readings)
			res.Set("attack.rss_after_mib", after.rss)
			res.Set("attack.private_after_mib", after.private)
			res.Set("attack.rss_growth_mib", after.rss-before.rss)
			res.Set("attack.private_growth_mib", after.private-before.private)
			if after.heap >= 0 && before.heap >= 0 {
				res.Set("attack.heap_growth_mib", after.heap-before.heap)
			}
			if done > 0 {
				per := func(name string, growth float64) {
					switch {
					case done >= *minExtrap:
						res.Set("attack."+name+"_growth_mib_per_1m", growth*1e6/float64(done))
						if done < 1000000 {
							res.Note("%s_growth_mib_per_1m is extrapolated linearly from %d requests", name, done)
						}
					case growth > *clearGrowth:
						res.Set("attack."+name+"_growth_mib_per_1m", growth*1e6/float64(done))
						res.Note("%s_growth_mib_per_1m is extrapolated from only %d requests because %.0f MiB is already far past the budget", name, done, growth)
					default:
						res.Note("%s growth %.2f MiB after %d requests: too few requests to extrapolate per million", name, growth, done)
					}
				}
				per("private", after.private-before.private)
				per("rss", after.rss-before.rss)
				if after.heap >= 0 && before.heap >= 0 {
					per("heap", after.heap-before.heap)
				}
			}
		}
	}
	if err := res.Write(sf.Out); err != nil {
		benchlib.Fatal(err)
	}
}

type memReading struct {
	rss, private float64
	// heap is go_memstats_heap_inuse_bytes from /metrics in MiB, or -1.
	heap float64
}

// readMem takes n readings 250 ms apart and returns the medians.
func readMem(ctx context.Context, srv *benchlib.Server, c *benchlib.Client, target string, n int) memReading {
	var rss, priv, heap []float64
	for i := 0; i < max(1, n); i++ {
		if i > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		if mi, err := srv.Memory(); err == nil {
			rss = append(rss, benchlib.MiB(mi.RSS))
			priv = append(priv, benchlib.MiB(mi.Private))
		}
		if target == "v3" {
			if h, ok := goHeap(ctx, c); ok {
				heap = append(heap, h)
			}
		}
	}
	m := memReading{rss: benchlib.Median(rss), private: benchlib.Median(priv), heap: -1}
	if len(heap) > 0 {
		m.heap = benchlib.Median(heap)
	}
	return m
}

// goHeap reads go_memstats_heap_inuse_bytes from the server's /metrics, if exported.
func goHeap(ctx context.Context, c *benchlib.Client) (float64, bool) {
	r, err := c.Get(ctx, "/metrics", true, nil)
	if err != nil || r.Status != 200 {
		return 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(r.Body))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "go_memstats_heap_inuse_bytes "); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return f / (1 << 20), true
			}
		}
	}
	return 0, false
}
