package terrain

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
	"timeshadedb/internal/v3/core/memstore"
)

// crashOp is one step of the deterministic crash-test workload: an Apply of a batch
// (all from one datastore, so it is one WAL frame) or a Commit.
type crashOp struct {
	batch  []core.ChunkUpdate
	commit core.TimeMs
}

var crashKeys = []core.DatastoreKey{coretest.DS, {Map: "gw-test.1", Surface: "vulcanus", Force: "player"}}

// crashOps generates the workload: flicker over a few tiles, deletes, late updates and
// commits that trail the clock.
func crashOps(seed int64) func() crashOp { return crashOpsWith(seed, 48, 9) }

// crashOpsWith spreads updates over spread x spread chunks around the origin and
// commits every commitEvery operations.
func crashOpsWith(seed int64, spread, commitEvery int) func() crashOp {
	r := rand.New(rand.NewSource(seed))
	colours := []uint16{0, 0x8C71, 0xF8C3, 0x4208, 0x07E0, 0xFFFF, 0x1234}
	now := core.TimeMs(1_791_630_000_000)
	n := 0
	return func() crashOp {
		n++
		if n%commitEvery == 0 {
			return crashOp{commit: now - core.TimeMs(r.Intn(40_000))}
		}
		ds := crashKeys[r.Intn(len(crashKeys))]
		b := make([]core.ChunkUpdate, 1+r.Intn(6))
		for i := range b {
			now += core.TimeMs(r.Intn(3000))
			u := core.ChunkUpdate{DS: ds, Chunk: core.ChunkCoord{X: int32(r.Intn(spread) - spread/2), Y: int32(r.Intn(spread) - spread/2)}, T: now - core.TimeMs(r.Intn(70_000))}
			if r.Intn(25) == 0 {
				u.Deleted = true
			} else {
				px := coretest.Fill(colours[r.Intn(len(colours))])
				for j := r.Intn(20); j > 0; j-- {
					px[r.Intn(core.ChunkPixelCount)] = colours[r.Intn(len(colours))]
				}
				u.Pixels = px
			}
			b[i] = u
		}
		return crashOp{batch: b}
	}
}

// TestKillDuringIngest runs the workload in a child process (real fsyncs, WAL group
// commit every 20 ms) and kills it at random moments. After each kill the database must
// reopen, and after a final commit it must equal the oracle fed exactly the updates
// whose Apply reached the WAL: every version, its effective time and its content.
func TestKillDuringIngest(t *testing.T) {
	if os.Getenv("TERRAIN_KILL_CHILD") != "" {
		terrainKillChild()
		return
	}
	if testing.Short() {
		t.Skip("kill test is slow")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(11))
	for round := 0; round < 6; round++ {
		dir := filepath.Join(t.TempDir(), "db")
		seed := int64(100 + round)
		cmd := exec.Command(exe, "-test.run", "^TestKillDuringIngest$")
		cmd.Env = append(os.Environ(), "TERRAIN_KILL_CHILD="+dir, "TERRAIN_KILL_SEED="+strconv.FormatInt(seed, 10))
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		applied := -1
		done := make(chan struct{})
		go func() {
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "applied "); ok {
					applied, _ = strconv.Atoi(v)
				}
			}
			close(done)
		}()
		time.Sleep(time.Duration(300+r.Intn(900)) * time.Millisecond)
		cmd.Process.Kill()
		<-done
		cmd.Wait()
		if applied < 0 {
			t.Fatalf("round %d: child applied nothing", round)
		}

		s, err := Open(dir, Options{})
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		far := core.TimeMs(1_900_000_000_000)
		if err := s.Commit(context.Background(), far); err != nil {
			t.Fatal(err)
		}
		// The WAL holds the batches whose Apply returned, and maybe the next one.
		var mismatch error
		matched := -1
		for _, extra := range []int{0, 1} {
			if mismatch = compareWithOracle(s, seed, applied+extra, far); mismatch == nil {
				matched = applied + extra
				break
			}
		}
		s.Close()
		if matched < 0 {
			t.Fatalf("round %d: reopened store matches no oracle prefix (applied %d batches): %v", round, applied, mismatch)
		}
		t.Logf("round %d: killed after %d batches, recovered %d", round, applied, matched)
	}
}

func compareWithOracle(s *Store, seed int64, batches int, far core.TimeMs) error {
	ctx := context.Background()
	want := memstore.NewTerrain()
	next := crashOps(seed)
	for done := 0; done < batches; {
		op := next()
		if op.batch == nil {
			want.Commit(ctx, op.commit)
			continue
		}
		want.Apply(ctx, op.batch)
		done++
	}
	want.Commit(ctx, far)
	return compareStores(s, want, far)
}

func terrainKillChild() {
	dir := os.Getenv("TERRAIN_KILL_CHILD")
	seed, _ := strconv.ParseInt(os.Getenv("TERRAIN_KILL_SEED"), 10, 64)
	s, err := Open(dir, Options{SyncInterval: 20 * time.Millisecond, MaxChain: 6})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(2)
	}
	ctx := context.Background()
	next := crashOps(seed)
	for applied := 0; ; {
		op := next()
		if op.batch == nil {
			if err := s.Commit(ctx, op.commit); err != nil {
				os.Exit(3)
			}
			continue
		}
		if _, err := s.Apply(ctx, op.batch); err != nil {
			os.Exit(4)
		}
		applied++
		fmt.Printf("applied %d\n", applied)
	}
}

// TestCommitInterrupted fails a commit after some tiles were written (the horizon is
// not persisted), closes the store, reopens it and continues. The WAL replay plus the
// skip rule for windows already written must give exactly the oracle's versions.
func TestCommitInterrupted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	opt := Options{NoSync: true, Workers: 1, MaxChain: 5}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	want := memstore.NewTerrain()
	next := crashOpsWith(77, 20, 40)
	batches, commits, interrupted := 0, 0, 0
	for i := 0; i < 8000; i++ {
		op := next()
		if op.batch != nil {
			if _, err := s.Apply(ctx, op.batch); err != nil {
				t.Fatal(err)
			}
			want.Apply(ctx, op.batch)
			batches++
			continue
		}
		want.Commit(ctx, op.commit)
		commits++
		if commits%2 != 0 {
			if err := s.Commit(ctx, op.commit); err != nil {
				t.Fatal(err)
			}
			continue
		}
		// Let the first few tiles of this commit through, then fail.
		left := 1 + commits%3
		commitTileHook = func(string) error {
			if left == 0 {
				return fmt.Errorf("injected failure")
			}
			left--
			return nil
		}
		err := s.Commit(ctx, op.commit)
		commitTileHook = nil
		if err != nil {
			interrupted++
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if s, err = Open(dir, opt); err != nil {
			t.Fatal(err)
		}
		// The interrupted commit is retried by the next one, as a server would.
		if err := s.Commit(ctx, op.commit); err != nil {
			t.Fatal(err)
		}
	}
	far := core.TimeMs(1_900_000_000_000)
	want.Commit(ctx, far)
	if err := s.Commit(ctx, far); err != nil {
		t.Fatal(err)
	}
	if interrupted == 0 {
		t.Fatal("no commit was interrupted")
	}
	if err := compareStores(s, want, far); err != nil {
		t.Fatalf("after %d interrupted commits: %v", interrupted, err)
	}
	t.Logf("%d batches, %d interrupted commits", batches, interrupted)
}

// compareHistoryAndChanges compares History over sub-ranges (bounds at, between and
// around the version times) and ChangedChunks for every version pair of a tile (or a
// deterministic sample of pairs when there are many versions).
func compareHistoryAndChanges(s *Store, want *memstore.Terrain, ds core.DatastoreKey, ti core.TileInfo, hist []core.VersionPoint) error {
	ctx := context.Background()
	var bounds []core.TimeMs
	for _, p := range hist {
		bounds = append(bounds, p.T-1, p.T, p.T+1)
	}
	for i := 0; i+1 < len(bounds); i += 2 {
		t0, t1 := bounds[i], bounds[len(bounds)-1-i]
		hw, _ := want.History(ctx, ds, ti.Tile, t0, t1)
		hg, err := s.History(ctx, ds, ti.Tile, t0, t1)
		if err != nil {
			return err
		}
		if fmt.Sprint(hw) != fmt.Sprint(hg) {
			return fmt.Errorf("%v %v: history [%d,%d) %v, oracle %v", ds, ti.Tile, t0, t1, hg, hw)
		}
	}
	r := rand.New(rand.NewSource(int64(ti.Tile.X)*7919 + int64(ti.Tile.Y)))
	n := int(ti.Version)
	pairs := n * (n + 1) / 2
	for k := 0; k < min(pairs, 40); k++ {
		from := core.Version(r.Intn(n))
		to := from + 1 + core.Version(r.Intn(n-int(from)))
		cw, errW := want.ChangedChunks(ctx, ds, ti.Tile, from, to)
		cg, errG := s.ChangedChunks(ctx, ds, ti.Tile, from, to)
		if (errW == nil) != (errG == nil) {
			return fmt.Errorf("%v %v changes %d-%d: err %v, oracle %v", ds, ti.Tile, from, to, errG, errW)
		}
		if len(cw) != len(cg) {
			return fmt.Errorf("%v %v changes %d-%d: %d blocks, oracle %d", ds, ti.Tile, from, to, len(cg), len(cw))
		}
		for j := range cw {
			if cw[j].Block != cg[j].Block || cw[j].Pixels != cg[j].Pixels {
				return fmt.Errorf("%v %v changes %d-%d: block %d differs", ds, ti.Tile, from, to, cw[j].Block)
			}
		}
	}
	return nil
}

// TestDifferentialHistory feeds the crash workload (several datastores, flicker,
// deletes, late updates) to the store and the oracle without crashes and compares
// History (sub-ranges) and ChangedChunks (version pairs) as well as every version's
// content, after every few commits.
func TestDifferentialHistory(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, Options{MaxChain: 4, DecodedCacheBytes: 4 * imageBytes})
	defer s.Close()
	want := memstore.NewTerrain()
	next := crashOpsWith(21, 40, 12)
	commits := 0
	var last core.TimeMs
	for i := 0; i < 2000; i++ {
		op := next()
		if op.batch == nil {
			if err := s.Commit(ctx, op.commit); err != nil {
				t.Fatal(err)
			}
			want.Commit(ctx, op.commit)
			last = op.commit
			if commits++; commits%20 == 0 {
				if err := compareStores(s, want, last); err != nil {
					t.Fatalf("after %d commits: %v", commits, err)
				}
			}
			continue
		}
		a, err := s.Apply(ctx, op.batch)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := want.Apply(ctx, op.batch); a != b {
			t.Fatalf("apply %+v, oracle %+v", a, b)
		}
	}
	far := core.TimeMs(1_900_000_000_000)
	s.Commit(ctx, far)
	want.Commit(ctx, far)
	if err := compareStores(s, want, far); err != nil {
		t.Fatal(err)
	}
}

func compareStores(s *Store, want *memstore.Terrain, far core.TimeMs) error {
	ctx := context.Background()
	for _, ds := range crashKeys {
		wt, _ := want.ListTiles(ctx, ds, 0)
		gt, _ := s.ListTiles(ctx, ds, 0)
		if len(wt) != len(gt) {
			return fmt.Errorf("%v: %d tiles, oracle %d", ds, len(gt), len(wt))
		}
		for i := range wt {
			if wt[i] != gt[i] {
				return fmt.Errorf("%v: tile %+v, oracle %+v", ds, gt[i], wt[i])
			}
			hw, _ := want.History(ctx, ds, wt[i].Tile, 0, far+1)
			hg, _ := s.History(ctx, ds, wt[i].Tile, 0, far+1)
			if fmt.Sprint(hw) != fmt.Sprint(hg) {
				return fmt.Errorf("%v %v: history differs", ds, wt[i].Tile)
			}
			if err := compareHistoryAndChanges(s, want, ds, wt[i], hw); err != nil {
				return err
			}
			for v := core.Version(0); v <= wt[i].Version; v++ {
				a, _ := want.ReadVersion(ctx, ds, wt[i].Tile, v)
				b, err := s.ReadVersion(ctx, ds, wt[i].Tile, v)
				if err != nil {
					return err
				}
				if !a.Equal(b) {
					return fmt.Errorf("%v %v v%d: content differs", ds, wt[i].Tile, v)
				}
			}
		}
	}
	return nil
}

// TestApplyAfterInterruptedCommit: a commit fails after the first of two tiles was
// written, the store is reopened, and new updates for both tiles arrive in the window
// that commit was closing (a tailer resending). Both tiles must treat them the same way
// as the oracle, which saw the commit finish: late. Before the fix, the written tile
// dropped them through the replay skip rule while the other tile kept them.
func TestApplyAfterInterruptedCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	opt := Options{NoSync: true, Workers: 1}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	want := memstore.NewTerrain()
	ds := coretest.DS
	a := core.ChunkCoord{X: 0, Y: 0}
	b := core.ChunkCoord{X: 1000, Y: 1000} // another tile
	first := []core.ChunkUpdate{
		{DS: ds, Chunk: a, T: 10_000, Pixels: coretest.Fill(1)},
		{DS: ds, Chunk: b, T: 10_000, Pixels: coretest.Fill(1)},
	}
	for _, st := range []core.TerrainStore{s, want} {
		if _, err := st.Apply(ctx, first); err != nil {
			t.Fatal(err)
		}
	}
	left := 1
	commitTileHook = func(string) error {
		if left == 0 {
			return fmt.Errorf("injected failure")
		}
		left--
		return nil
	}
	err = s.Commit(ctx, core.Minute)
	commitTileHook = nil
	if err == nil {
		t.Fatal("commit was not interrupted")
	}
	want.Commit(ctx, core.Minute)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(dir, opt); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	second := []core.ChunkUpdate{
		{DS: ds, Chunk: a, T: 20_000, Pixels: coretest.Fill(2)},
		{DS: ds, Chunk: b, T: 20_000, Pixels: coretest.Fill(2)},
	}
	gr, err := s.Apply(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	wr, _ := want.Apply(ctx, second)
	if gr != wr {
		t.Fatalf("apply after reopen %+v, oracle %+v", gr, wr)
	}
	far := core.TimeMs(10 * core.Minute)
	if err := s.Commit(ctx, far); err != nil {
		t.Fatal(err)
	}
	want.Commit(ctx, far)
	for _, c := range []core.ChunkCoord{a, b} {
		tc, blk := c.Tile()
		gi, gs, err := s.ReadTile(ctx, ds, tc, far, false)
		if err != nil {
			t.Fatal(err)
		}
		wi, ws, _ := want.ReadTile(ctx, ds, tc, far, false)
		if !gi.Equal(wi) || gs != ws {
			t.Fatalf("tile %v: block %v state %+v, oracle block %v state %+v", tc, gi.Block(blk)[0], gs, wi.Block(blk)[0], ws)
		}
	}
	infos, _ := s.Datastores(ctx)
	if len(infos) != 1 || infos[0].Horizon != core.Minute*10 {
		t.Fatalf("datastores %+v", infos)
	}
}
