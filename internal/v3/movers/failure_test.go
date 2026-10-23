package movers

import (
	"context"
	"errors"
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

// TestCrashAroundCommit: a crash after a commit wrote meta.json but before it deleted the
// logs of the windows it closed, and a crash in a commit that failed before meta.json,
// both reopen to the state of a store that never crashed.
func TestCrashAroundCommit(t *testing.T) {
	ctx := context.Background()
	T0, W := coretest.T0, core.MoverWindowLen
	for _, mode := range []string{"after-meta", "before-meta"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			m, err := Open(dir, Options{SyncInterval: -1})
			if err != nil {
				t.Fatal(err)
			}
			ref := memstore.NewMovers()
			a, b := mover("a"), mover("b")
			batch := []core.MoverSample{at(a, T0+1000, 1), at(b, T0+1500, 2), at(b, T0+W+1000, 2), at(a, T0+W+2000, 3)}
			mustAppend(t, m, batch)
			ref.Append(ctx, batch)
			sfcDir := filepath.Join(dir, "movers", failSFC.ID())
			log0 := filepath.Join(sfcDir, "open", timeName(T0)+".wal")
			saved, err := os.ReadFile(log0)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "before-meta" {
				blocker := filepath.Join(sfcDir, "meta.json.tmp")
				if err := os.MkdirAll(blocker, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := m.Commit(ctx, T0+W); err == nil {
					t.Fatal("commit with an unwritable meta.json succeeded")
				}
				os.Remove(blocker)
			} else {
				ok(t, m.Commit(ctx, T0+W))
				ref.Commit(ctx, T0+W)
				// The crash came before the closed window's log was deleted.
				if err := os.WriteFile(log0, saved, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			crash(m)
			c, err := Open(dir, Options{SyncInterval: -1})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := os.Stat(log0); mode == "after-meta" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("log of a committed window kept: %v", err)
			}
			if mode == "before-meta" {
				ref.Commit(ctx, T0+W)
			}
			ok(t, c.Commit(ctx, T0+W))
			compareStores(t, ctx, c, ref, T0, T0+W)
			ok(t, c.Commit(ctx, T0+3*W))
			ref.Commit(ctx, T0+3*W)
			compareStores(t, ctx, c, ref, T0, T0+3*W)
		})
	}
}

// TestMigrateSingleWAL: a database written by a store with the single-file WAL
// (movers/wal/wal.log) opens with its open rows moved into the window logs.
func TestMigrateSingleWAL(t *testing.T) {
	ctx := context.Background()
	T0, W := coretest.T0, core.MoverWindowLen
	dir := t.TempDir()
	rows := []core.MoverSample{at(mover("a"), T0+1000, 1), at(mover("b"), T0+W+1500, 2), at(mover("a"), T0+2*W+1000, 3)}
	// The old log format: one frame with the rows of every open window.
	entries := make([]walEntry, len(rows))
	for i := range rows {
		r, _ := makeRow(&rows[i], &rows[i].Meta, 0)
		entries[i] = walEntry{sfc: failSFC, r: &r}
	}
	next := uint64(1)
	buf, fs := encodeEntries(make([]byte, 8), 0, entries, map[walMetaKey]uint64{}, &next, nil)
	frame(buf[fs:])
	walDir := filepath.Join(dir, "movers", "wal")
	if err := os.MkdirAll(walDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(walDir, "wal.log"), buf, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := Open(dir, Options{SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(walDir, "wal.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old WAL kept: %v", err)
	}
	if logs, _ := filepath.Glob(filepath.Join(dir, "movers", failSFC.ID(), "open", "*.wal")); len(logs) != 3 {
		t.Fatalf("window logs = %v", logs)
	}
	crash(m)
	c, err := Open(dir, Options{SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ref := memstore.NewMovers()
	ref.Append(ctx, rows)
	ok(t, c.Commit(ctx, T0+3*W))
	ref.Commit(ctx, T0+3*W)
	compareStores(t, ctx, c, ref, T0, T0+3*W)
}

// TestWALWriteFailure: when the WAL write fails nothing is applied, and later appends
// fail without changing memory.
func TestWALWriteFailure(t *testing.T) {
	ctx := context.Background()
	m, _ := openTemp(t, Options{SyncInterval: -1})
	defer m.Close()
	mustAppend(t, m, []core.MoverSample{at(mover("a"), coretest.T0+1000, 1)})
	crash(m) // every later write to the window's log fails
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

// TestClockBounds: samples far ahead of the clock are Invalid, samples before MinTime are
// Invalid, any time in between is accepted (however old: a backlog after a long
// downtime), and windows accepted before there was a clock that start far ahead of it are
// dropped by the first Commit.
func TestClockBounds(t *testing.T) {
	ctx := context.Background()
	T0, W := coretest.T0, core.MoverWindowLen
	const century = 100 * 365 * core.Day

	t.Run("AfterCommit", func(t *testing.T) {
		m, _ := openTemp(t, Options{SyncInterval: -1})
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
		ok(t, m.Commit(ctx, T0+W))
		// A new surface can start far behind the clock (a backlog), but not before MinTime.
		old := at(mover("o"), T0-90*core.Day, 1)
		old.Surface = core.SurfaceKey{Map: "gw-b.1", Surface: "nauvis"}
		ancient := old
		ancient.T = DefaultMinTime - 1
		if res, err := m.Append(ctx, []core.MoverSample{old, ancient}); err != nil || res.Accepted != 1 || res.Invalid != 1 || m.Stats().Behind != 1 {
			t.Fatalf("old samples on a new surface = %+v, %v", res, err)
		}
		// Its first commit writes at most MaxKeyframesPerCommit keyframes; the horizon
		// catches up over the next commits.
		ok(t, m.Commit(ctx, T0+W))
		info, _ := m.Surfaces(ctx)
		var h core.TimeMs
		for _, si := range info {
			if si.Key == old.Surface {
				h = si.Horizon
			}
		}
		if want := core.WindowStart(old.T, core.Hour) + MaxKeyframesPerCommit*core.Hour; h != want {
			t.Fatalf("horizon after the first commit = %d, want %d", h, want)
		}
	})

	t.Run("BeforeFirstCommit", func(t *testing.T) {
		m, _ := openTemp(t, Options{SyncInterval: -1})
		defer m.Close()
		res, err := m.Append(ctx, []core.MoverSample{at(mover("f"), T0+century, 1), at(mover("o"), 1000, 1), at(mover("n"), T0+1000, 1)})
		if err != nil || res.Accepted != 2 || res.Invalid != 1 {
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
		if err != nil || res.Accepted != 2 || res.Invalid != 1 || res.Late != 0 {
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
