// Command attack sends random hostile tile requests (random levels and coordinates,
// unknown datastores, missing versions) and measures how much server memory they leave
// behind and how fast they are answered (the Hostile requests targets in bench/targets.json:
// 1 M random coordinates add < 2 MB, each answered in < 1 ms).
//
// Memory is read several times before and after (the median of -mem-readings, after
// -settle), as the commit charge (private bytes, the target metric), as RSS, and as the
// Go heap from /metrics when the server exports go_memstats_heap_inuse_bytes. A single
// working-set reading moves by more than the 2 MiB budget on its own.
//
// With -gc (the default), a v3 server the tool starts also gets --pprof on a free
// loopback port, and every memory reading is preceded by a forced garbage collection
// (GET /debug/pprof/heap?gc=1) and -gc-settle for the runtime to return freed pages.
// Without it the readings mostly show how far the collector let garbage grow, which
// says nothing about what the requests left behind. The live heap after the forced
// collection (go_memstats_heap_alloc_bytes) is what the requests retained.
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
	"io"
	"math/rand"
	"net"
	"net/http"
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
	warmHostile := flag.Int("warm-hostile", 200000, "hostile warm-up requests (at -conc) before the baseline memory reading, not counted")
	seed := flag.Int64("seed", 1, "random seed")
	readings := flag.Int("mem-readings", 5, "memory readings before and after (the median is used)")
	settle := flag.Duration("settle", 2*time.Second, "wait before the readings after the run")
	minExtrap := flag.Int("min-extrapolate", 100000, "fewest requests to extrapolate the growth per million from")
	clearGrowth := flag.Float64("clear-growth-mib", 64, "growth (MiB) that is reported per million even from fewer requests, because it is far past the budget")
	forceGC := flag.Bool("gc", true, "v3: start the server with --pprof and force a garbage collection before each memory reading")
	pprofURL := flag.String("pprof", "", "pprof base URL of a -base server, for -gc (e.g. http://127.0.0.1:6060)")
	gcSettle := flag.Duration("gc-settle", 30*time.Second, "wait after the forced collection, before the readings (the runtime returns freed pages gradually)")
	flag.Parse()

	res := sf.NewResult("attack")
	if sf.Base == "" && sf.DB == "" {
		if err := sf.EnsureDB("attack-"); err != nil {
			benchlib.Fatal(err)
		}
	}
	res.Param("db", sf.DB)
	gcURL := ""
	if *forceGC && sf.Target == "v3" {
		switch {
		case sf.Base != "":
			gcURL = strings.TrimRight(*pprofURL, "/")
		default:
			port, err := freePort()
			if err != nil {
				benchlib.Fatal(err)
			}
			args := sf.Args
			if args == "" {
				args = benchlib.DefaultServerArgs["v3"]
				if sf.Token != "" {
					args += " --ingest-token " + sf.Token
				}
			}
			sf.Args = args + fmt.Sprintf(" --pprof 127.0.0.1:%d", port)
			gcURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		}
	}
	if gcURL != "" {
		res.Param("gc_settle_s", gcSettle.Seconds())
		res.Note("memory read after a forced garbage collection (pprof heap?gc=1) and %s of settling", *gcSettle)
	}
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
	// Hostile warm-up at the measured concurrency: the first requests grow per-connection
	// goroutine stacks and runtime caches once (a step of about 8 MiB of private bytes on
	// Windows that further millions of requests do not repeat), which is not memory the
	// requests retain. Without it the per-million growth measured that step.
	if *warmHostile > 0 {
		var wsent atomic.Int64
		var wwg sync.WaitGroup
		for w := 0; w < *conc; w++ {
			wwg.Add(1)
			go func() {
				defer wwg.Done()
				for wsent.Add(1) <= int64(*warmHostile) {
					if _, err := client.Get(ctx, nextURL(), false, nil); err != nil {
						return
					}
				}
			}()
		}
		wwg.Wait()
		res.Param("warm_hostile", *warmHostile)
	}
	time.Sleep(time.Second)
	var before memReading
	if srv != nil {
		before = readMem(ctx, srv, client, sf.Target, *readings, gcURL, *gcSettle)
		res.Set("attack.rss_before_mib", before.rss)
		res.Set("attack.private_before_mib", before.private)
		if before.heap >= 0 {
			res.Set("attack.heap_before_mib", before.heap)
		}
		if before.live >= 0 {
			res.Set("attack.live_heap_before_mib", before.live)
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
			after := readMem(ctx, srv, client, sf.Target, *readings, gcURL, *gcSettle)
			res.Set("attack.rss_after_mib", after.rss)
			res.Set("attack.private_after_mib", after.private)
			res.Set("attack.rss_growth_mib", after.rss-before.rss)
			res.Set("attack.private_growth_mib", after.private-before.private)
			if after.heap >= 0 && before.heap >= 0 {
				res.Set("attack.heap_growth_mib", after.heap-before.heap)
			}
			if after.live >= 0 && before.live >= 0 {
				res.Set("attack.live_heap_after_mib", after.live)
				res.Set("attack.live_heap_growth_mib", after.live-before.live)
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
				if after.live >= 0 && before.live >= 0 {
					per("live_heap", after.live-before.live)
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
	// live is go_memstats_heap_alloc_bytes in MiB after a forced collection, or -1.
	live float64
}

// freePort returns a loopback port that was free a moment ago.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// collect forces a garbage collection in the server through pprof (heap?gc=1 runs
// runtime.GC before writing the profile).
func collect(gcURL string) error {
	r, err := http.Get(gcURL + "/debug/pprof/heap?gc=1")
	if err != nil {
		return err
	}
	defer r.Body.Close()
	io.Copy(io.Discard, r.Body)
	if r.StatusCode != 200 {
		return fmt.Errorf("pprof heap: HTTP %d", r.StatusCode)
	}
	return nil
}

// readMem takes n readings 250 ms apart and returns the medians. With gcURL it first
// forces a collection, waits settle, and collects again.
func readMem(ctx context.Context, srv *benchlib.Server, c *benchlib.Client, target string, n int, gcURL string, settle time.Duration) memReading {
	gcOK := false
	if gcURL != "" {
		if err := collect(gcURL); err != nil {
			fmt.Println("attack: forced collection failed:", err)
		} else {
			time.Sleep(settle)
			gcOK = collect(gcURL) == nil
		}
	}
	var rss, priv, heap, live []float64
	for i := 0; i < max(1, n); i++ {
		if i > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		if mi, err := srv.Memory(); err == nil {
			rss = append(rss, benchlib.MiB(mi.RSS))
			priv = append(priv, benchlib.MiB(mi.Private))
		}
		if target == "v3" {
			if h, a, ok := goHeap(ctx, c); ok {
				heap = append(heap, h)
				if gcOK && a >= 0 {
					live = append(live, a)
				}
			}
		}
	}
	m := memReading{rss: benchlib.Median(rss), private: benchlib.Median(priv), heap: -1, live: -1}
	if len(heap) > 0 {
		m.heap = benchlib.Median(heap)
	}
	if len(live) > 0 {
		m.live = benchlib.Median(live)
	}
	return m
}

// goHeap reads go_memstats_heap_inuse_bytes and go_memstats_heap_alloc_bytes (-1 when
// missing) from the server's /metrics, in MiB.
func goHeap(ctx context.Context, c *benchlib.Client) (inuse, alloc float64, ok bool) {
	r, err := c.Get(ctx, "/metrics", true, nil)
	if err != nil || r.Status != 200 {
		return 0, 0, false
	}
	alloc = -1
	sc := bufio.NewScanner(bytes.NewReader(r.Body))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if v, found := strings.CutPrefix(sc.Text(), "go_memstats_heap_inuse_bytes "); found {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				inuse, ok = f/(1<<20), true
			}
		}
		if v, found := strings.CutPrefix(sc.Text(), "go_memstats_heap_alloc_bytes "); found {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				alloc = f / (1 << 20)
			}
		}
	}
	return inuse, alloc, ok
}
