package movers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
	"timeshadedb/internal/v3/core/memstore"
)

// These tests cover failures and hostile input: a WAL rewrite or a commit that fails
// halfway, a WAL write error, damaged window files, oversized samples and samples far
// outside the clock.

var failSFC = core.SurfaceKey{Map: "gw-a.1", Surface: "nauvis"}

func mover(id string) core.MoverMeta {
	return core.MoverMeta{ID: id, Kind: core.KindVehicle, Force: "player", Name: "car"}
}

func at(meta core.MoverMeta, t core.TimeMs, x float64) core.MoverSample {
	return core.MoverSample{Surface: failSFC, Meta: meta, T: t, Tick: int64(t-coretest.T0) * 60 / 1000, X: x, Y: 1}
}

// liveX returns the X of every journey's newest row in w.
func liveX(w *core.MoverWindow) map[string]float64 {
	out := map[string]float64{}
	last := map[string]core.TimeMs{}
	for _, r := range w.Rows {
		id := w.Movers[r.Mover].ID
		if t, ok := last[id]; !ok || r.T >= t {
			out[id], last[id] = r.X, r.T
		}
	}
	return out
}

// TestWALRewriteFailure: a failed rewrite must not change the meta numbering that later
// appends use, or replay gives their samples to another journey.
func TestWALRewriteFailure(t *testing.T) {
	ctx := context.Background()
	T0, W := coretest.T0, core.MoverWindowLen
	dir := t.TempDir()
	m, err := Open(dir, Options{SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	a, b := mover("a"), mover("b")
	// In the log: a = 1, b = 2. Only b keeps an open row after the commit.
	mustAppend(t, m, []core.MoverSample{at(a, T0+1000, 1), at(b, T0+1500, 2), at(b, T0+W+1000, 2)})
	tmp := filepath.Join(dir, "movers", "wal", "wal.log.tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(ctx, T0+W); err == nil {
		t.Fatal("commit with an unwritable WAL rewrite succeeded")
	}
	// The old log is still in use: b's next sample must reference b's number in it.
	if res, err := m.Append(ctx, []core.MoverSample{at(b, T0+W+2000, 3)}); err != nil || res.Accepted != 1 {
		t.Fatalf("append after a failed rewrite = %+v, %v", res, err)
	}
	m.wal.f.Close() // crash: no Close
	os.Remove(tmp)
	c, err := Open(dir, Options{SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w, err := c.Live(ctx, failSFC, T0+W+3000)
	if err != nil {
		t.Fatal(err)
	}
	if got := liveX(w); got["b"] != 3 || got["a"] != 1 {
		t.Fatalf("live after replay = %v, want a at 1 and b at 3", got)
	}
}

// TestWALWriteFailure: when the WAL write fails nothing is applied, and later appends
// fail without changing memory.
func TestWALWriteFailure(t *testing.T) {
	ctx := context.Background()
	m, _ := openTemp(t, Options{SyncInterval: -1})
	defer m.Close()
	mustAppend(t, m, []core.MoverSample{at(mover("a"), coretest.T0+1000, 1)})
	m.wal.f.Close() // every later write fails
	for i := 0; i < 2; i++ {
		res, err := m.Append(ctx, []core.MoverSample{at(mover("b"), coretest.T0+2000, 2)})
		if err == nil || res != (core.ApplyResult{}) {
			t.Fatalf("append %d with a broken WAL = %+v, %v", i, res, err)
		}
		if st := m.Stats(); st.OpenRows != 1 || st.Accepted != 1 {
			t.Fatalf("append %d changed memory: %+v", i, st)
		}
	}
}

// TestCommitFailureLeavesSurfaceUnchanged: a commit that fails after writing windows
// and keyframes (meta.json cannot be written) changes nothing in memory; the retried
// commit gives the same result as a store that never failed.
func TestCommitFailureLeavesSurfaceUnchanged(t *testing.T) {
	ctx := context.Background()
	T0 := coretest.T0
	dir := t.TempDir()
	m, err := Open(dir, Options{SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ref := memstore.NewMovers()
	first := []core.MoverSample{at(mover("a"), T0+1000, 1), at(mover("b"), T0+2000, 2)}
	mustAppend(t, m, first)
	ref.Append(ctx, first)
	ok(t, m.Commit(ctx, T0+core.Hour+core.MoverWindowLen))
	ref.Commit(ctx, T0+core.Hour+core.MoverWindowLen)
	second := []core.MoverSample{
		at(mover("a"), T0+2*core.Hour, 5),
		{Surface: failSFC, Meta: mover("b"), T: T0 + 2*core.Hour + 1000, Flags: core.FlagGone},
		at(mover("c"), T0+2*core.Hour+2000, 7),
	}
	mustAppend(t, m, second)
	ref.Append(ctx, second)
	blocker := filepath.Join(dir, "movers", failSFC.ID(), "meta.json.tmp")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	before := m.Stats()
	if err := m.Commit(ctx, T0+3*core.Hour); err == nil {
		t.Fatal("commit with an unwritable meta.json succeeded")
	}
	s := m.sfc[failSFC]
	if s.horizon != T0+core.Hour+core.MoverWindowLen || len(s.live) != 2 || int64(s.live["a"].x) != core.QuantizePos(1) {
		t.Fatalf("failed commit changed the surface: horizon %d, live %d", s.horizon, len(s.live))
	}
	if st := m.Stats(); st.OpenRows != before.OpenRows {
		t.Fatalf("open rows %d -> %d", before.OpenRows, st.OpenRows)
	}
	os.Remove(blocker)
	ok(t, m.Commit(ctx, T0+3*core.Hour))
	ref.Commit(ctx, T0+3*core.Hour)
	compareStores(t, ctx, m, ref, T0, T0+3*core.Hour)
}

// TestTooManyCarriages: a sample with more carriages than the wire format reads back
// is Invalid, so it cannot make windows (or the store) unreadable.
func TestTooManyCarriages(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m, err := Open(dir, Options{SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	s := at(mover("t"), coretest.T0+1000, 1)
	s.Meta.Kind = core.KindTrain
	s.Carriages = make([]core.Carriage, MaxCarriages+1)
	ok2 := at(mover("t"), coretest.T0+2000, 2)
	ok2.Meta.Kind = core.KindTrain
	ok2.Carriages = make([]core.Carriage, MaxCarriages)
	res, err := m.Append(ctx, []core.MoverSample{s, ok2})
	if err != nil || res.Invalid != 1 || res.Accepted != 1 {
		t.Fatalf("append = %+v, %v", res, err)
	}
	ok(t, m.Commit(ctx, coretest.T0+core.MoverWindowLen))
	w, err := m.Window(ctx, failSFC, coretest.T0)
	if err != nil || len(w.Rows) != 1 || len(w.Rows[0].Carriages) != MaxCarriages {
		t.Fatalf("window = %v, %v", w, err)
	}
	ok(t, m.Close())
	if m, err = Open(dir, Options{SyncInterval: -1}); err != nil {
		t.Fatal(err)
	}
	m.Close()
}

// TestDamagedFiles: a window or keyframe that does not decode is skipped (and counted)
// when Open rebuilds the live state and in Live; it does not stop the store opening.
func TestDamagedFiles(t *testing.T) {
	ctx := context.Background()
	T0, W := coretest.T0, core.MoverWindowLen
	dir := t.TempDir()
	m, err := Open(dir, Options{SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, m, []core.MoverSample{at(mover("a"), T0+1000, 1), at(mover("a"), T0+core.Hour+1000, 2), at(mover("b"), T0+core.Hour+2000, 3)})
	ok(t, m.Commit(ctx, T0+core.Hour+W))
	ok(t, m.Close())
	sfcDir := filepath.Join(dir, "movers", failSFC.ID())
	for _, p := range []string{
		filepath.Join(sfcDir, "keyframes", core.UTCDay(T0+core.Hour), timeName(T0+core.Hour)+".mvw"),
		filepath.Join(sfcDir, "windows", core.UTCDay(T0+core.Hour), timeName(T0+core.Hour)+".mvw"),
	} {
		if err := os.WriteFile(p, []byte("not a zstd frame"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var logged []string
	m, err = Open(dir, Options{SyncInterval: -1, Logf: func(f string, args ...any) { logged = append(logged, f) }})
	if err != nil {
		t.Fatalf("open with damaged files: %v", err)
	}
	defer m.Close()
	if st := m.Stats(); st.Corrupt != 2 || len(logged) != 2 {
		t.Fatalf("corrupt = %d, logged %d", st.Corrupt, len(logged))
	}
	// The live state falls back to the windows before the damaged keyframe: a is known
	// from its first window; b only existed in the damaged one.
	w, err := m.Live(ctx, failSFC, T0+core.Hour+W)
	if err != nil {
		t.Fatal(err)
	}
	if got := liveX(w); got["a"] != 1 || len(got) != 1 {
		t.Fatalf("live = %v", got)
	}
	if _, err := m.Window(ctx, failSFC, T0+core.Hour); err == nil {
		t.Fatal("damaged window decoded")
	}
	// Live before the horizon reads the disk and skips the damaged files too.
	if _, err := m.Live(ctx, failSFC, T0+core.Hour+3000); err != nil {
		t.Fatalf("live from disk: %v", err)
	}
	if m.Stats().Corrupt < 3 {
		t.Fatalf("live from disk did not count the damaged files")
	}
}

// TestClockBounds: samples far ahead of the clock are Invalid, samples far behind it on
// a surface without a horizon are Late, and rows accepted before there was a clock are
// dropped by the first Commit.
func TestClockBounds(t *testing.T) {
	ctx := context.Background()
	T0, W := coretest.T0, core.MoverWindowLen
	const century = 100 * 365 * core.Day

	t.Run("AfterCommit", func(t *testing.T) {
		m, _ := openTemp(t, Options{SyncInterval: -1, MaxOpenRows: 10})
		defer m.Close()
		ok(t, m.Commit(ctx, T0))
		future := make([]core.MoverSample, 10)
		for i := range future {
			future[i] = at(mover("f"), T0+century+core.TimeMs(i), 1)
		}
		res, err := m.Append(ctx, future)
		if err != nil || res.Invalid != 10 || m.Stats().Ahead != 10 {
			t.Fatalf("future append = %+v, %v, %+v", res, err, m.Stats())
		}
		normal := make([]core.MoverSample, 10)
		for i := range normal {
			normal[i] = at(mover("n"), T0+core.TimeMs(i)*1000, 1)
		}
		if res, err := m.Append(ctx, normal); err != nil || res.Accepted != 10 {
			t.Fatalf("append after future samples = %+v, %v", res, err)
		}
		// A new surface cannot start far behind the clock.
		old := at(mover("o"), T0-90*core.Day, 1)
		old.Surface = core.SurfaceKey{Map: "gw-b.1", Surface: "nauvis"}
		ok(t, m.Commit(ctx, T0+W))
		if res, err := m.Append(ctx, []core.MoverSample{old}); err != nil || res.Late != 1 || m.Stats().Behind != 1 {
			t.Fatalf("old sample on a new surface = %+v, %v", res, err)
		}
	})

	t.Run("BeforeFirstCommit", func(t *testing.T) {
		m, _ := openTemp(t, Options{SyncInterval: -1})
		defer m.Close()
		res, err := m.Append(ctx, []core.MoverSample{at(mover("f"), T0+century, 1), at(mover("o"), 1000, 1), at(mover("n"), T0+1000, 1)})
		if err != nil || res.Accepted != 3 {
			t.Fatalf("append = %+v, %v", res, err)
		}
		ok(t, m.Commit(ctx, T0+W))
		st := m.Stats()
		if st.OpenRows != 0 || st.Ahead != 1 || st.Behind != 1 || st.KeyframesWritten != 0 || st.WindowsWritten != 1 {
			t.Fatalf("stats after the first commit = %+v", st)
		}
		info, _ := m.Surfaces(ctx)
		if len(info) != 1 || info[0].FirstT != T0+1000 || info[0].LastT != T0+1000 {
			t.Fatalf("surfaces = %+v", info)
		}
	})

	t.Run("Clock", func(t *testing.T) {
		m, _ := openTemp(t, Options{SyncInterval: -1, Clock: func() core.TimeMs { return T0 }})
		defer m.Close()
		res, err := m.Append(ctx, []core.MoverSample{at(mover("f"), T0+2*core.Hour, 1), at(mover("o"), T0-2*core.Day, 1), at(mover("n"), T0+1000, 1)})
		if err != nil || res.Accepted != 1 || res.Invalid != 1 || res.Late != 1 {
			t.Fatalf("append with a clock = %+v, %v", res, err)
		}
	})

	t.Run("ReopenKeepsReference", func(t *testing.T) {
		dir := t.TempDir()
		m, err := Open(dir, Options{SyncInterval: -1})
		if err != nil {
			t.Fatal(err)
		}
		mustAppend(t, m, []core.MoverSample{at(mover("n"), T0+1000, 1)})
		ok(t, m.Commit(ctx, T0+W))
		ok(t, m.Close())
		m, err = Open(dir, Options{SyncInterval: -1})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		if res, err := m.Append(ctx, []core.MoverSample{at(mover("f"), T0+century, 1)}); err != nil || res.Invalid != 1 {
			t.Fatalf("future sample after reopen = %+v, %v", res, err)
		}
	})
}

// mustAppend appends setup samples and fails the test if any is not accepted.
func mustAppend(t *testing.T, m *Store, samples []core.MoverSample) {
	t.Helper()
	res, err := m.Append(context.Background(), samples)
	if err != nil || res.Accepted != len(samples) {
		t.Fatalf("append = %+v, %v", res, err)
	}
}
