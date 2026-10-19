// Command tilebench measures server tile latency on a populated database: z0 at the
// latest time (cold, then warm from the server's caches), z0 at random historical times
// (each request a new time, so caches cannot help), and coarse levels (zoom-out).
// It can also measure startup on the database (spawn until ready).
//
// Latency is measured by the client over loopback HTTP/1.1, one tile per request, with
// -conc requests in flight (a browser on HTTP/1.1 uses 6).
//
//	tilebench -target v2 -server-bin tsdb-master.exe -db <populated db> -surface nauvis -out r.json
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"

	"timeshadedb/bench/internal/benchlib"
)

func main() {
	var sf benchlib.ServerFlags
	sf.Register(flag.CommandLine)
	mapKey := flag.String("map", "", "map key of the datastore (default: the widest datastore)")
	surface := flag.String("surface", "nauvis", "surface of the datastore")
	force := flag.String("force", "player", "force of the datastore")
	conc := flag.Int("conc", 6, "requests in flight")
	warmReps := flag.Int("warm-reps", 3, "warm passes over the latest z0 tiles")
	histTimes := flag.Int("hist-times", 20, "random historical times; each z0 tile is fetched once per time")
	histTiles := flag.Int("hist-tiles", 16, "z0 tiles per historical time (random subset of the tiles with data)")
	tFirst := flag.Int64("t-first", 0, "first data time for historical sampling (default: catalog first, or last-7d for v2)")
	levels := flag.String("levels", "-1,-2,-3,-4,-5", "coarse levels to fetch at the latest time")
	minCold := flag.Int("min-cold", 20, "cold samples per coarse level: the cold pass is repeated after restarts until it has this many (levels with few tiles)")
	restart := flag.Bool("restart-cold", true, "restart the server before each cold phase (needs a server started by tilebench)")
	startups := flag.Int("startups", 3, "startup measurements on the database (0: skip)")
	seed := flag.Int64("seed", 1, "random seed")
	flag.Parse()

	res := sf.NewResult("tilebench")
	res.Param("conc", *conc)
	if sf.Base == "" && sf.DB == "" {
		benchlib.Fatal(fmt.Errorf("-db (a populated database) or -base is required"))
	}
	res.Param("db", sf.DB)
	rng := rand.New(rand.NewSource(*seed))
	ctx := context.Background()

	var srv *benchlib.Server
	var base string
	var err error
	var startupMs []float64
	start := func() {
		srv, base, err = sf.Start()
		if err != nil {
			benchlib.Fatal(err)
		}
		if srv != nil {
			startupMs = append(startupMs, benchlib.Ms(srv.Startup))
		}
	}
	restartIfOwned := func() {
		if srv == nil || !*restart {
			return
		}
		srv.Stop()
		start()
	}
	start()
	defer func() { srv.Stop() }()
	for i := 1; i < *startups && srv != nil; i++ {
		restartIfOwned()
	}
	if len(startupMs) > 0 && *startups > 0 {
		res.Set("startup.ms", benchlib.Median(startupMs))
		if total, _, err := benchlib.DirSize(sf.DB); err == nil {
			res.Set("startup.db_mib", float64(total)/(1<<20))
		}
	}

	newTarget := func() benchlib.Target {
		c := benchlib.NewClient(base, *conc)
		c.Token = sf.Token
		t, err := benchlib.NewTarget(sf.Target, c)
		if err != nil {
			benchlib.Fatal(err)
		}
		return t
	}
	target := newTarget()
	all, err := target.Datastores(ctx)
	if err != nil {
		benchlib.Fatal(err)
	}
	ds, ok := benchlib.FindDS(all, *mapKey, *surface, *force)
	if !ok {
		benchlib.Fatal(fmt.Errorf("no datastore for map=%q surface=%q force=%q among %d", *mapKey, *surface, *force, len(all)))
	}
	res.Param("datastore", ds.ID)
	res.Param("bounds", ds.Bounds)

	record := func(name string, lat, bytes []float64, bad, n int) {
		res.SetSummary("tile."+name+"_ms", benchlib.Summarize(lat))
		if len(bytes) > 0 {
			res.Set("tile."+name+"_bytes_mean", benchlib.Summarize(bytes).Mean)
		}
		if bad > 0 {
			res.Set("tile."+name+"_errors", float64(bad))
			res.Note("%s: %d of %d requests did not answer 200", name, bad, n)
		}
	}
	fetch := func(t benchlib.Target, refs []benchlib.TileRef, name string) {
		lat, bytes, bad := fetchAll(ctx, clientOf(t), refs, *conc)
		record(name, lat, bytes, bad, len(refs))
	}

	// 1. z0 latest: cold, then warm.
	restartIfOwned()
	target = newTarget()
	refs, viewDur, err := target.Tiles(ctx, ds, 0, nil)
	if err != nil {
		benchlib.Fatal(err)
	}
	refs = withData(ctx, clientOf(target), refs, *conc, sf.Target)
	res.Set("tile.z0_tiles", float64(len(refs)))
	if viewDur > 0 {
		res.Set("tile.view_latest_ms", benchlib.Ms(viewDur))
	}
	// For v2, withData fetched every tile once to find the ones with data, so the cold
	// pass needs a restart; v3's view lists only tiles with data and withData fetches
	// nothing. Both restart here so the cold pass starts from empty caches.
	restartIfOwned()
	target = newTarget()
	if sf.Target == "v3" {
		refs, _, err = target.Tiles(ctx, ds, 0, nil)
		if err != nil {
			benchlib.Fatal(err)
		}
	}
	fetch(target, refs, "z0_latest_cold")
	var warm []benchlib.TileRef
	for i := 0; i < *warmReps; i++ {
		warm = append(warm, refs...)
	}
	fetch(target, warm, "z0_latest_warm")

	// 2. z0 historical, cold: a new random time per round.
	first := ds.First
	if *tFirst != 0 {
		first = *tFirst
	}
	if first == 0 || first >= ds.Last {
		first = ds.Last - 7*24*3600*1000
		res.Note("historical sampling from last-7d (no first time known)")
	}
	restartIfOwned()
	target = newTarget()
	var histRefs []benchlib.TileRef
	var viewMs []float64
	seen := map[string]bool{}
	for i := 0; i < *histTimes; i++ {
		t := first + rng.Int63n(max(1, ds.Last-first))
		trefs, vd, err := target.Tiles(ctx, ds, 0, &t)
		if err != nil {
			benchlib.Fatal(err)
		}
		if vd > 0 {
			viewMs = append(viewMs, benchlib.Ms(vd))
		}
		trefs = intersect(trefs, refs)
		// The same (tile, version) can be effective at several of the random times; a
		// repeat would hit the encoded cache, so each URL is fetched once.
		trefs = unseen(trefs, seen)
		rng.Shuffle(len(trefs), func(a, b int) { trefs[a], trefs[b] = trefs[b], trefs[a] })
		trefs = trefs[:min(len(trefs), *histTiles)]
		for _, r := range trefs {
			seen[r.URL] = true
		}
		histRefs = append(histRefs, trefs...)
	}
	res.Param("hist_first", first)
	res.Param("hist_last", ds.Last)
	if len(viewMs) > 0 {
		res.SetSummary("tile.view_hist_ms", benchlib.Summarize(viewMs))
	}
	fetch(target, histRefs, "z0_hist_cold")

	// 3. Coarse levels at the latest time: cold pass, then one warm pass.
	deepest := 0
	for _, ls := range strings.Split(*levels, ",") {
		ls = strings.TrimSpace(ls)
		if ls == "" {
			continue
		}
		z, err := strconv.Atoi(ls)
		if err != nil || z >= 0 {
			benchlib.Fatal(fmt.Errorf("bad level %q", ls))
		}
		restartIfOwned()
		target = newTarget()
		zrefs, _, err := target.Tiles(ctx, ds, z, nil)
		if err != nil {
			res.Note("level %d: %v", z, err)
			continue
		}
		name := fmt.Sprintf("zm%d_latest", -z)
		res.Set("tile."+name+"_tiles", float64(len(zrefs)))
		// Few tiles cover a coarse level, so one cold pass is too few samples for a p95:
		// repeat it after restarts until there are -min-cold samples.
		var lat, sizes []float64
		bad, n, rounds := 0, 0, 0
		for len(zrefs) > 0 && (rounds == 0 || (len(lat) < *minCold && srv != nil && *restart && rounds < 50)) {
			if rounds > 0 {
				restartIfOwned()
				target = newTarget()
			}
			l, b, e := fetchAll(ctx, clientOf(target), zrefs, *conc)
			lat, sizes, bad, n = append(lat, l...), append(sizes, b...), bad+e, n+len(zrefs)
			rounds++
		}
		record(name+"_cold", lat, sizes, bad, n)
		res.Set("tile."+name+"_cold_rounds", float64(rounds))
		if len(lat) < *minCold {
			res.Note("%s: only %d cold samples (no restarts possible or too few tiles)", name, len(lat))
		}
		fetch(target, zrefs, name+"_warm")
		if z < deepest {
			deepest = z
		}
	}
	res.Param("deepest_level", deepest)

	if srv != nil {
		if mi, err := srv.Memory(); err == nil {
			res.Set("mem.final_rss_mib", benchlib.MiB(mi.RSS))
		}
	}
	if err := res.Write(sf.Out); err != nil {
		benchlib.Fatal(err)
	}
}

func clientOf(t benchlib.Target) *benchlib.Client {
	switch v := t.(type) {
	case *benchlib.V2:
		return v.C
	case *benchlib.V3:
		return v.C
	}
	panic("unknown target")
}

// fetchAll fetches every ref with conc workers and returns the latency (ms) and size of
// each 200 answer, and the number of other answers.
func fetchAll(ctx context.Context, c *benchlib.Client, refs []benchlib.TileRef, conc int) (lat, size []float64, bad int) {
	var mu sync.Mutex
	jobs := make(chan benchlib.TileRef)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				resp, err := c.Get(ctx, r.URL, false, map[string]string{"Accept-Encoding": "zstd, gzip"})
				mu.Lock()
				if err != nil || resp.Status != 200 {
					bad++
				} else {
					lat = append(lat, benchlib.Ms(resp.Dur))
					size = append(size, float64(resp.Bytes))
				}
				mu.Unlock()
			}
		}()
	}
	for _, r := range refs {
		jobs <- r
	}
	close(jobs)
	wg.Wait()
	return lat, size, bad
}

// withData drops tiles without data. v2 answers every in-bounds tile with a PNG, so a
// tile whose PNG is the empty-tile size is dropped; v3's view lists only tiles with data.
func withData(ctx context.Context, c *benchlib.Client, refs []benchlib.TileRef, conc int, kind string) []benchlib.TileRef {
	if kind != "v2" {
		return refs
	}
	type sized struct {
		ref benchlib.TileRef
		n   int64
	}
	var mu sync.Mutex
	var out []sized
	jobs := make(chan benchlib.TileRef)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				resp, err := c.Get(ctx, r.URL, false, nil)
				if err == nil && resp.Status == 200 {
					mu.Lock()
					out = append(out, sized{r, resp.Bytes})
					mu.Unlock()
				}
			}
		}()
	}
	for _, r := range refs {
		jobs <- r
	}
	close(jobs)
	wg.Wait()
	var minN int64 = -1
	for _, s := range out {
		if minN < 0 || s.n < minN {
			minN = s.n
		}
	}
	var keep []benchlib.TileRef
	for _, s := range out {
		// An all-black 512x512 PNG compresses to about 1-2 KB; anything with terrain
		// is far larger.
		if s.n > minN || minN > 4096 {
			keep = append(keep, s.ref)
		}
	}
	return keep
}

// unseen drops refs whose URL is in seen, and duplicates within refs.
func unseen(refs []benchlib.TileRef, seen map[string]bool) []benchlib.TileRef {
	var out []benchlib.TileRef
	local := map[string]bool{}
	for _, r := range refs {
		if !seen[r.URL] && !local[r.URL] {
			local[r.URL] = true
			out = append(out, r)
		}
	}
	return out
}

// intersect keeps refs whose (x, y) is among the tiles with data.
func intersect(refs, with []benchlib.TileRef) []benchlib.TileRef {
	have := map[[2]int32]bool{}
	for _, r := range with {
		have[[2]int32{r.X, r.Y}] = true
	}
	var out []benchlib.TileRef
	for _, r := range refs {
		if have[[2]int32{r.X, r.Y}] {
			out = append(out, r)
		}
	}
	return out
}
