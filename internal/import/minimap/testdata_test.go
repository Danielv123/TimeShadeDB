package minimap_test

import (
	"context"
	"os"
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"testing"
	"time"

	"timeshadedb/internal/import/minimap"
	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/memstore"
)

// sourceCounts are the illisis export's mover counts as measured by the research scripts
// (research/out/movers.txt): journeys, samples and gones by kind, and consecutive
// same-time sample pairs.
var sourceCounts = map[string]struct {
	journeys, samples, gones map[string]int
	dups                     int
}{
	"gw-bc41437d~2E1.nauvis": {
		map[string]int{"p": 11, "v": 27, "t": 32},
		map[string]int{"p": 252669, "v": 529551, "t": 1119617},
		map[string]int{"p": 2702, "t": 9176, "v": 102466}, 1112,
	},
	"gw-bc41437d~2E1.fulgora": {
		map[string]int{"p": 2, "t": 1}, map[string]int{"p": 8637, "t": 102826}, map[string]int{"p": 20}, 103,
	},
	"gw-bc41437d~2E1.gleba":    {map[string]int{"p": 1}, map[string]int{"p": 9235}, map[string]int{"p": 33}, 0},
	"gw-bc41437d~2E1.vulcanus": {map[string]int{"p": 2, "v": 4}, map[string]int{"p": 26448, "v": 76}, map[string]int{"p": 67, "v": 4}, 10},
	"i-248495586.platform-1":   {map[string]int{"p": 1}, map[string]int{"p": 259}, map[string]int{"p": 6}, 0},
	"i-430127327.platform-2":   {map[string]int{"p": 2}, map[string]int{"p": 175}, map[string]int{"p": 4}, 0},
}

// terrainRecords are the z0 MMC1 record counts (research/testdata.md §2).
var terrainRecords = map[string]int{
	"gw-bc41437d~2E1.nauvis.player":   7670200,
	"gw-bc41437d~2E1.vulcanus.player": 22132,
	"gw-bc41437d~2E1.gleba.player":    7423,
	"gw-bc41437d~2E1.fulgora.player":  3062,
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestTestdata imports the full illisis test export into the memstores and checks the
// WP5 acceptance criteria. Set TSDB_TESTDATA to the export root (the directory holding
// maps.json.gz) to run it.
func TestTestdata(t *testing.T) {
	root := os.Getenv("TSDB_TESTDATA")
	if root == "" {
		t.Skip("TSDB_TESTDATA not set")
	}
	ctx := context.Background()
	d, err := minimap.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	s := stores{memstore.NewTerrain(), memstore.NewMovers(), memstore.NewOverlays()}
	rep, err := minimap.Import(ctx, d, minimap.Options{Terrain: s.t, Movers: s.m, Overlays: s.o})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("import: total %d ms (terrain %d, movers %d, overlays %d)", rep.TotalMs, rep.TerrainMs, rep.MoversMs, rep.OverlaysMs)
	if rep.TotalMs > 30000 {
		t.Errorf("import took %d ms, target <= 30000", rep.TotalMs)
	}
	if rep.TerrainApply.Late != 0 || rep.TerrainApply.Invalid != 0 || rep.MoverApply.Late != 0 || rep.MoverApply.Invalid != 0 ||
		rep.OverlayApply.Late != 0 || rep.OverlayApply.Invalid != 0 {
		t.Errorf("stores dropped data: terrain %+v movers %+v overlays %+v", rep.TerrainApply, rep.MoverApply, rep.OverlayApply)
	}

	for _, tr := range rep.Terrain {
		if want, ok := terrainRecords[tr.DS]; ok && tr.Records != want {
			t.Errorf("%s: %d records, research counted %d", tr.DS, tr.Records, want)
		}
		if len(tr.Errors) > 0 {
			t.Errorf("%s: errors %v", tr.DS, tr.Errors)
		}
	}
	tv, err := minimap.VerifyTerrain(ctx, d, s.t, minimap.Options{}, minimap.VerifyOptions{HistoryPerArea: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("terrain verify: final %d/%d, history %d/%d, coarse %d/%d", tv.FinalEqual, tv.FinalAreas, tv.HistoryEqual, tv.HistoryChecks, tv.CoarseEqual, tv.CoarseAreas)
	if !tv.OK() || tv.FinalAreas != 66 || tv.CoarseAreas != 96 {
		t.Errorf("terrain verify failed: %+v", tv)
	}

	if len(rep.Movers) != len(sourceCounts) {
		t.Errorf("%d mover surfaces, want %d", len(rep.Movers), len(sourceCounts))
	}
	trainJumps := 0
	for _, mr := range rep.Movers {
		want, ok := sourceCounts[mr.Surface]
		if !ok {
			t.Errorf("unexpected surface %s", mr.Surface)
			continue
		}
		if !sameCounts(mr.Source.Journeys, want.journeys) || !sameCounts(mr.Source.Samples, want.samples) ||
			!sameCounts(mr.Source.Gones, want.gones) || mr.SourceDupPairs != want.dups {
			t.Errorf("%s: source counts %+v dups %d differ from movers.txt", mr.Surface, mr.Source, mr.SourceDupPairs)
		}
		// Reconcile: imported = source - dropped duplicates + keyframe seeds.
		if got, want := sum(mr.Imported.Samples), sum(mr.Source.Samples)-mr.DupSamplesDropped+mr.KeyframeSeeded; got != want {
			t.Errorf("%s: imported %d samples, source minus fixes gives %d", mr.Surface, got, want)
		}
		if got, want := sum(mr.Imported.Gones), sum(mr.Source.Gones)-mr.DupGonesDropped; got != want {
			t.Errorf("%s: imported %d gones, source minus fixes gives %d", mr.Surface, got, want)
		}
		if mr.InvalidRows != 0 || len(mr.Errors) != 0 {
			t.Errorf("%s: invalid %d errors %v", mr.Surface, mr.InvalidRows, mr.Errors)
		}
		trainJumps += mr.TrainJumpsAfter
		t.Logf("%s: boundary %d retimed %v (+%d gones), dups dropped %d/%d, seeded %d, handoffs %d, end switches %d, train jumps %d -> %d",
			mr.Surface, mr.BoundaryStamped, mr.Retimed, mr.RetimedGones, mr.DupSamplesDropped, mr.DupGonesDropped,
			mr.KeyframeSeeded, mr.Handoffs, mr.EndSwitches, mr.TrainJumpsBefore, mr.TrainJumpsAfter)
	}
	// 378 jumps before the fixes (research/out/jumps_nauvis.txt, all at window floors). The
	// two left are two trains changing shape at a stop at the same moment (a split or
	// join), not clamp artefacts.
	if trainJumps > 2 {
		t.Errorf("%d train jumps left after the fixes, want <= 2", trainJumps)
	}
	mv, err := minimap.VerifyMovers(ctx, rep, s.m)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range mv {
		if !v.Match {
			t.Errorf("mover read-back mismatch: %+v", v)
		}
	}

	tags, recipes := 0, 0
	for _, or := range rep.Overlays {
		tags += or.TagVersions
		recipes += or.RecipeVersions
	}
	if tags != 6 || recipes != 8215 {
		t.Errorf("overlays: %d tag and %d recipe versions, research counted 6 and 8215", tags, recipes)
	}
	ov, err := minimap.VerifyOverlays(ctx, rep, s.o)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range ov {
		if !v.Match {
			t.Errorf("overlay read-back mismatch: %+v", v)
		}
	}
}

// countingMovers is a MoverStore that keeps nothing: it counts samples and checks that
// they arrive in time order.
type countingMovers struct {
	core.MoverStore
	samples    int
	last       core.TimeMs
	outOfOrder int
}

func (s *countingMovers) Append(_ context.Context, samples []core.MoverSample) (core.ApplyResult, error) {
	for _, m := range samples {
		if m.T < s.last {
			s.outOfOrder++
		}
		s.last = max(s.last, m.T)
	}
	s.samples += len(samples)
	return core.ApplyResult{Accepted: len(samples)}, nil
}

func (s *countingMovers) Commit(context.Context, core.TimeMs) error { return nil }

// TestTestdataMoverHeap imports the test export's movers into a store that keeps
// nothing and bounds the importer's peak heap: the mover import streams, so its memory
// does not grow with the length of the history. Set TSDB_TESTDATA to run it.
func TestTestdataMoverHeap(t *testing.T) {
	root := os.Getenv("TSDB_TESTDATA")
	if root == "" {
		t.Skip("TSDB_TESTDATA not set")
	}
	d, err := minimap.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	base := sample[0].Value.Uint64()
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		tk := time.NewTicker(2 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				metrics.Read(s)
				if v := s[0].Value.Uint64(); v > peak.Load() {
					peak.Store(v)
				}
			}
		}
	}()
	store := &countingMovers{}
	rep, err := minimap.Import(context.Background(), d, minimap.Options{Movers: store})
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	p := peak.Load()
	if p > base {
		p -= base
	} else {
		p = 0
	}
	t.Logf("movers: %d samples in %d ms, peak heap %d MB above the %d MB baseline", store.samples, rep.MoversMs, p>>20, base>>20)
	if store.samples != rep.MoverApply.Accepted || store.samples < 2_000_000 {
		t.Errorf("store got %d samples, report says %d", store.samples, rep.MoverApply.Accepted)
	}
	if store.outOfOrder != 0 {
		t.Errorf("%d samples arrived out of time order", store.outOfOrder)
	}
	if p > 150<<20 {
		t.Errorf("peak heap %d MB, want <= 150 MB", p>>20)
	}
}
