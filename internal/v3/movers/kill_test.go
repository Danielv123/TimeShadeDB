package movers

import (
	"bufio"
	"context"
	"fmt"
	"math/bits"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
	"timeshadedb/internal/v3/core/memstore"
)

// killOp is one step of the kill-test workload: an Append of a batch or a Commit.
type killOp struct {
	batch  []core.MoverSample
	commit core.TimeMs
}

// killOps generates the workload: batches of random samples over a clock that moves on
// (some late, some for windows that are evicted), and commits that trail it.
func killOps(seed int64) func() killOp {
	rng := rand.New(rand.NewSource(seed))
	now := coretest.T0
	n := 0
	return func() killOp {
		n++
		if n%7 == 0 {
			return killOp{commit: now - 6*core.Minute - core.TimeMs(rng.Int63n(int64(4*core.Minute)))}
		}
		now += core.TimeMs(rng.Int63n(int64(20 * core.Second)))
		return killOp{batch: randomSamples(rng, now-3*core.Minute, 4*core.Minute, 20+rng.Intn(60))}
	}
}

// TestKillDuringIngest runs the workload in a child process (window logs fsynced by a
// group commit every 20 ms, an open-window budget so small that windows are evicted all
// the time, commits that read evicted windows back) and kills it at random moments: in an
// Append, an eviction, a commit or between them. After each kill the store must open, and
// after a final commit it must equal memstore fed exactly the batches whose Append
// returned, plus, of the batch in flight when the kill came, the rows of some of the
// windows it touches: a batch is written to one log per window, so a kill between those
// writes leaves part of a batch that was never acknowledged (its sender retries it). No
// accepted sample is lost.
func TestKillDuringIngest(t *testing.T) {
	if os.Getenv("MOVERS_KILL_CHILD") != "" {
		moversKillChild()
		return
	}
	if testing.Short() {
		t.Skip("kill test is slow")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(12))
	for round := 0; round < 6; round++ {
		dir := filepath.Join(t.TempDir(), "db")
		seed := int64(200 + round)
		cmd := exec.Command(exe, "-test.run", "^TestKillDuringIngest$")
		cmd.Env = append(os.Environ(), "MOVERS_KILL_CHILD="+dir, "MOVERS_KILL_SEED="+strconv.FormatInt(seed, 10))
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
		// The batch in flight: every subset of the window logs it touches, from none
		// (the first candidate) to all.
		var logs []walKey
		for _, s := range killBatch(seed, applied) {
			k := walKey{s.Surface, core.WindowStart(s.T, core.MoverWindowLen)}
			if !slices.Contains(logs, k) {
				logs = append(logs, k)
			}
		}
		var mismatch error
		matched := -1
		for mask := 0; mask < 1<<len(logs); mask++ {
			in := map[walKey]bool{}
			for i, k := range logs {
				in[k] = mask&(1<<i) != 0
			}
			if mismatch = compareKilled(dir, seed, applied, in); mismatch == nil {
				matched = mask
				break
			}
		}
		if matched < 0 {
			t.Fatalf("round %d: reopened store matches no oracle (applied %d batches, the next touching %d logs): %v", round, applied, len(logs), mismatch)
		}
		t.Logf("round %d: killed after %d batches, recovered them and %d of the %d logs of the next", round, applied, bits.OnesCount(uint(matched)), len(logs))
	}
}

// walKey names a window log: a surface and a window start.
type walKey struct {
	sfc   core.SurfaceKey
	start core.TimeMs
}

// killBatch returns batch i (0-based) of the workload.
func killBatch(seed int64, i int) []core.MoverSample {
	next := killOps(seed)
	for done := 0; ; {
		op := next()
		if op.batch == nil {
			continue
		}
		if done == i {
			return op.batch
		}
		done++
	}
}

// compareKilled opens a copy of the killed store, commits everything and compares it
// with memstore fed the first batches of the workload and the samples of the next batch
// that fall in the window logs marked in partial.
func compareKilled(dir string, seed int64, batches int, partial map[walKey]bool) error {
	ctx := context.Background()
	cp := dir + ".check"
	if err := os.CopyFS(cp, os.DirFS(dir)); err != nil {
		return err
	}
	defer os.RemoveAll(cp)
	m, err := Open(cp, Options{SyncInterval: -1, MaxAhead: -1, MinTime: -1, Logf: func(string, ...any) {}})
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	defer m.Close()
	want := memstore.NewMovers()
	next := killOps(seed)
	var last core.TimeMs
	for done := 0; done <= batches; {
		op := next()
		if op.batch == nil {
			if done < batches {
				want.Commit(ctx, op.commit)
			}
			continue
		}
		batch := op.batch
		if done == batches {
			batch = nil
			for _, s := range op.batch {
				if partial[walKey{s.Surface, core.WindowStart(s.T, core.MoverWindowLen)}] {
					batch = append(batch, s)
				}
			}
		}
		want.Append(ctx, batch)
		for _, s := range batch {
			last = max(last, s.T)
		}
		done++
	}
	far := last + core.Hour
	if err := m.Commit(ctx, far); err != nil {
		return err
	}
	want.Commit(ctx, far)
	return compareErr(ctx, m, want, coretest.T0-core.Hour, far)
}

func moversKillChild() {
	dir := os.Getenv("MOVERS_KILL_CHILD")
	seed, _ := strconv.ParseInt(os.Getenv("MOVERS_KILL_SEED"), 10, 64)
	m, err := Open(dir, Options{SyncInterval: 20 * time.Millisecond, OpenBytes: 8 << 10, MaxAhead: -1, MinTime: -1})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(2)
	}
	ctx := context.Background()
	next := killOps(seed)
	for applied := 0; ; {
		op := next()
		if op.batch == nil {
			if err := m.Commit(ctx, op.commit); err != nil {
				os.Exit(3)
			}
			continue
		}
		if _, err := m.Append(ctx, op.batch); err != nil {
			os.Exit(4)
		}
		applied++
		fmt.Printf("applied %d\n", applied)
	}
}

// compareErr is compareStores without the live payloads, returning the first difference:
// surfaces, day indexes, every closed window and keyframe.
func compareErr(ctx context.Context, a, b core.MoverStore, t0, t1 core.TimeMs) error {
	sa, err := a.Surfaces(ctx)
	if err != nil {
		return err
	}
	sb, _ := b.Surfaces(ctx)
	if !reflect.DeepEqual(sa, sb) {
		return fmt.Errorf("surfaces:\n got  %+v\n want %+v", sa, sb)
	}
	norm := func(w *core.MoverWindow) *core.MoverWindow {
		c := memstore.CloneWindow(w)
		for i := range c.Rows {
			if len(c.Rows[i].Carriages) == 0 {
				c.Rows[i].Carriages = nil
			}
			if len(c.Rows[i].Rail) == 0 {
				c.Rows[i].Rail = nil
			}
		}
		if len(c.Movers) == 0 {
			c.Movers = nil
		}
		if len(c.Rows) == 0 {
			c.Rows = nil
		}
		return c
	}
	for _, info := range sb {
		for _, day := range info.Days {
			da, err := a.Day(ctx, info.Key, day)
			if err != nil {
				return fmt.Errorf("day %s: %w", day, err)
			}
			db, _ := b.Day(ctx, info.Key, day)
			if !reflect.DeepEqual(da, db) {
				return fmt.Errorf("day %s:\n got  %+v\n want %+v", day, da, db)
			}
			for _, ref := range db.Windows {
				wa, err := a.Window(ctx, info.Key, ref.Start)
				if err != nil {
					return fmt.Errorf("window %d: %w", ref.Start, err)
				}
				wb, _ := b.Window(ctx, info.Key, ref.Start)
				if !reflect.DeepEqual(norm(wa), norm(wb)) {
					return fmt.Errorf("window %v %d differs", info.Key, ref.Start)
				}
			}
			for _, k := range db.Keyframes {
				ka, err := a.Keyframe(ctx, info.Key, k)
				if err != nil {
					return fmt.Errorf("keyframe %d: %w", k, err)
				}
				kb, _ := b.Keyframe(ctx, info.Key, k)
				if !reflect.DeepEqual(norm(ka), norm(kb)) {
					return fmt.Errorf("keyframe %v %d differs", info.Key, k)
				}
			}
		}
	}
	return nil
}
