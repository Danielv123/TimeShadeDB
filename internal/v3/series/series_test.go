package series

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
)

func TestContract(t *testing.T) {
	coretest.SeriesStore(t, func(t *testing.T) core.SeriesStore {
		s, err := Open(t.TempDir(), Options{NoSync: true})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// payloadFor returns a deterministic payload for record seq, of varying length.
func payloadFor(seq uint64) []byte {
	r := rand.New(rand.NewSource(int64(seq)))
	b := make([]byte, 1+r.Intn(3000))
	r.Read(b)
	return b
}

func recFor(seq uint64) core.Record {
	kind := core.RecordDelta
	if seq%7 == 3 {
		kind = core.RecordKeyframe
	}
	return core.Record{RecordMeta: core.RecordMeta{TStart: core.TimeMs(seq/2) * 60_000, TLen: 60_000, Kind: kind, Flags: core.RecordFlags(seq % 2)}, Payload: payloadFor(seq)}
}

func appendN(t *testing.T, s *Series, from, to uint64) {
	t.Helper()
	ctx := context.Background()
	for i := from; i < to; i++ {
		seq, err := s.Append(ctx, recFor(i))
		if err != nil {
			t.Fatal(err)
		}
		if seq != i {
			t.Fatalf("seq %d, want %d", seq, i)
		}
	}
}

// checkPrefix verifies that the series holds exactly records 0..n-1 as written.
func checkPrefix(t *testing.T, s *Series, n uint64) {
	t.Helper()
	ctx := context.Background()
	if s.Len() != n {
		t.Fatalf("len %d, want %d", s.Len(), n)
	}
	if n == 0 {
		return
	}
	recs, err := s.ReadRange(ctx, 0, n-1)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		want := recFor(uint64(i))
		if r.Kind != want.Kind || r.TStart != want.TStart || r.TLen != want.TLen || r.Flags != want.Flags || !bytes.Equal(r.Payload, want.Payload) {
			t.Fatalf("record %d differs", i)
		}
	}
}

func openOne(t *testing.T, dir, name string, create bool, opt Options) (*Store, *Series) {
	t.Helper()
	st, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.OpenSeries(name, create)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	return st, s
}

func TestReopenAndHelpers(t *testing.T) {
	dir := t.TempDir()
	st, s := openOne(t, dir, "ds/tiles/0_0", true, Options{})
	appendN(t, s, 0, 40)
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	appendN(t, s, 40, 50) // not synced: the index records are rebuilt on reopen
	s.Close()
	st.Close()

	var reps []RecoveryReport
	st, s = openOne(t, dir, "ds/tiles/0_0", false, Options{OnRecover: func(_ string, r RecoveryReport) { reps = append(reps, r) }})
	defer st.Close()
	checkPrefix(t, s, 50)
	if len(reps) != 0 {
		t.Fatalf("clean close needed recovery: %+v", reps)
	}
	// Versions count delta records.
	var deltas uint32
	for i := uint64(0); i < 50; i++ {
		if recFor(i).Kind == core.RecordDelta {
			deltas++
		}
	}
	if s.Versions() != deltas {
		t.Fatalf("versions %d, want %d", s.Versions(), deltas)
	}
	seq, ok := s.VersionSeq(1)
	if !ok || seq != 0 {
		t.Fatalf("VersionSeq(1) = %d %v", seq, ok)
	}
	if _, ok := s.VersionSeq(deltas + 1); ok {
		t.Fatal("VersionSeq past the end")
	}
	sum := s.Summary()
	if sum.Records != 50 || sum.Versions != deltas {
		t.Fatalf("summary %+v", sum)
	}
}

// TestTruncatedTail cuts .blk at every byte offset inside the last records and checks
// that reopening keeps exactly the complete records.
func TestTruncatedTail(t *testing.T) {
	base := t.TempDir()
	st, s := openOne(t, base, "a/b", true, Options{NoSync: true})
	appendN(t, s, 0, 6)
	s.Sync()
	sizes := make([]int64, 0, 7)
	s.f.mu.RLock()
	for _, e := range s.f.entries {
		sizes = append(sizes, e.end())
	}
	s.f.mu.RUnlock()
	s.Close()
	st.Close()
	blk, _ := os.ReadFile(filepath.Join(base, "a", "b.blk"))
	idx, _ := os.ReadFile(filepath.Join(base, "a", "b.idx"))
	r := rand.New(rand.NewSource(2))
	cuts := []int64{0, 1, 31, 32, 33}
	for i := 0; i < 60; i++ {
		cuts = append(cuts, sizes[3]+r.Int63n(int64(len(blk))-sizes[3]))
	}
	for _, cut := range cuts {
		for _, withIdx := range []bool{true, false} {
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, "a"), 0o755)
			os.WriteFile(filepath.Join(dir, "a", "b.blk"), blk[:cut], 0o644)
			if withIdx {
				os.WriteFile(filepath.Join(dir, "a", "b.idx"), idx, 0o644)
			}
			want := uint64(0)
			for _, e := range sizes {
				if e <= cut {
					want++
				}
			}
			st, s := openOne(t, dir, "a/b", false, Options{NoSync: true})
			checkPrefix(t, s, want)
			// The series accepts appends after recovery and they survive a reopen.
			appendN(t, s, want, want+2)
			s.Close()
			st.Close()
			st, s = openOne(t, dir, "a/b", false, Options{NoSync: true})
			checkPrefix(t, s, want+2)
			s.Close()
			st.Close()
		}
	}
}

// TestCorruptIndexAndPayload damages the index and a payload.
func TestCorruptIndexAndPayload(t *testing.T) {
	dir := t.TempDir()
	st, s := openOne(t, dir, "x", true, Options{NoSync: true})
	appendN(t, s, 0, 10)
	s.Close()
	st.Close()
	idxPath := filepath.Join(dir, "x.idx")
	blkPath := filepath.Join(dir, "x.blk")

	// Garbage appended to the index (torn index write): dropped, nothing lost.
	f, _ := os.OpenFile(idxPath, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(bytes.Repeat([]byte{0xAB}, 24*3+5))
	f.Close()
	var rep RecoveryReport
	st, s = openOne(t, dir, "x", false, Options{NoSync: true, OnRecover: func(_ string, r RecoveryReport) { rep = r }})
	checkPrefix(t, s, 10)
	if rep.DroppedIndex != 3 {
		t.Fatalf("report %+v", rep)
	}
	s.Close()
	st.Close()

	// A flipped payload byte in record 8: records 8 and 9 are cut.
	st, s = openOne(t, dir, "x", false, Options{NoSync: true})
	s.f.mu.RLock()
	off := s.f.entries[8].off + HeaderSize + 1
	s.f.mu.RUnlock()
	s.Close()
	st.Close()
	blk, _ := os.ReadFile(blkPath)
	blk[off] ^= 0xFF
	os.WriteFile(blkPath, blk, 0o644)
	os.Remove(idxPath) // force a full rebuild from .blk
	st, s = openOne(t, dir, "x", false, Options{NoSync: true})
	checkPrefix(t, s, 8)
	s.Close()
	st.Close()

	// A damaged record in the middle of an indexed series is reported on read, not on open.
	st, s = openOne(t, dir, "x", false, Options{NoSync: true})
	s.f.mu.RLock()
	off = s.f.entries[2].off + HeaderSize
	s.f.mu.RUnlock()
	s.Close()
	st.Close()
	blk, _ = os.ReadFile(blkPath)
	blk[off] ^= 0xFF
	os.WriteFile(blkPath, blk, 0o644)
	st, s = openOne(t, dir, "x", false, Options{NoSync: true})
	defer st.Close()
	if s.Len() != 8 {
		t.Fatalf("len %d", s.Len())
	}
	if _, err := s.Read(context.Background(), 2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("read of damaged record: %v", err)
	}
	if _, err := s.Read(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
}

func TestEvictionBoundsOpenSeries(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Options{MaxOpen: 4, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 20; i++ {
		s, err := st.OpenSeries(fmt.Sprintf("t/%d", i), true)
		if err != nil {
			t.Fatal(err)
		}
		appendN(t, s, 0, 3)
		s.Close()
	}
	stats := st.Stats()
	if stats.Open > 4 || stats.Evictions != 16 {
		t.Fatalf("stats %+v", stats)
	}
	for i := 0; i < 20; i++ {
		s, err := st.OpenSeries(fmt.Sprintf("t/%d", i), false)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, s, 3)
		s.Close()
	}
	// A handle in use is never evicted.
	held, _ := st.OpenSeries("t/0", false)
	for i := 1; i < 20; i++ {
		s, _ := st.OpenSeries(fmt.Sprintf("t/%d", i), false)
		s.Close()
	}
	checkPrefix(t, held, 3)
	held.Close()
}

// TestKillDuringAppend runs a child process that appends records as fast as it can
// (syncing every few records) and kills it at random moments. Every reopen must succeed
// and hold a prefix of the written records that includes every record the child
// reported as synced.
func TestKillDuringAppend(t *testing.T) {
	if os.Getenv("SERIES_KILL_CHILD") != "" {
		killChild()
		return
	}
	if testing.Short() {
		t.Skip("kill test is slow")
	}
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(7))
	rounds := 8
	for round := 0; round < rounds; round++ {
		cmd := exec.Command(exe, "-test.run", "^TestKillDuringAppend$")
		cmd.Env = append(os.Environ(), "SERIES_KILL_CHILD="+dir)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		synced := uint64(0)
		done := make(chan struct{})
		go func() {
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if n, err := strconv.ParseUint(strings.TrimPrefix(sc.Text(), "synced "), 10, 64); err == nil {
					synced = n
				}
			}
			close(done)
		}()
		time.Sleep(time.Duration(50+r.Intn(250)) * time.Millisecond)
		cmd.Process.Kill()
		<-done
		cmd.Wait()

		st, s := openOne(t, dir, "k/s", true, Options{})
		n := s.Len()
		if n < synced {
			t.Fatalf("round %d: %d records after kill, child had synced %d", round, n, synced)
		}
		checkPrefix(t, s, n)
		s.Close()
		st.Close()
		t.Logf("round %d: killed after %d synced, reopened with %d records", round, synced, n)
	}
}

func killChild() {
	dir := os.Getenv("SERIES_KILL_CHILD")
	st, err := Open(dir, Options{})
	if err != nil {
		os.Exit(2)
	}
	s, err := st.OpenSeries("k/s", true)
	if err != nil {
		os.Exit(2)
	}
	ctx := context.Background()
	for i := s.Len(); ; i++ {
		if _, err := s.Append(ctx, recFor(i)); err != nil {
			os.Exit(3)
		}
		if i%5 == 4 {
			if err := s.Sync(); err != nil {
				os.Exit(4)
			}
			fmt.Printf("synced %d\n", i+1)
		}
	}
}
