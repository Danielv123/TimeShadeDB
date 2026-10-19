package minimap

import (
	"container/heap"
	"context"
	"math"
	"reflect"
	"sort"
	"testing"

	"timeshadedb/internal/v3/core"
)

// trainRows builds rows for one journey (the meta is set by fixJourney).
func trainRows(specs ...[6]float64) []srcRow {
	out := make([]srcRow, len(specs))
	for i, s := range specs {
		out[i] = srcRow{t: int64(s[0]), x: s[1], y: s[2], vx: s[3], vy: s[4], flags: uint8(s[5])}
	}
	return out
}

var trainMeta = []core.MoverMeta{{ID: "1.2.3", Kind: core.KindTrain, Carriages: 5}}

func newRep() *MoverReport {
	return &MoverReport{Retimed: map[string]int{}, Source: newCounts(), Imported: newCounts()}
}

func drain(s *moverSurface) []outSample {
	var out []outSample
	for s.out.Len() > 0 {
		out = append(out, heap.Pop(&s.out).(outSample))
	}
	return out
}

// fixJourney runs one journey's rows (sorted by t) through the streaming fixes with
// the whole journey known at once, and returns the emitted samples in time order.
func fixJourney(rows []srcRow, metas []core.MoverMeta, rep *MoverReport) []outSample {
	s := newMoverSurface(core.SurfaceKey{}, 300000, rep)
	for _, r := range rows {
		r.meta = &metas[0]
		s.addRow(r)
	}
	s.advance(math.MaxInt64, true)
	return drain(s)
}

// streamJourney feeds rows (sorted by t) the way run does: in "files" of fileMs, with
// a step at every file start. It returns the released samples in release order.
func streamJourney(t *testing.T, rows []srcRow, meta *core.MoverMeta, fileMs int64, rep *MoverReport) []outSample {
	t.Helper()
	ctx := context.Background()
	s := newMoverSurface(core.SurfaceKey{}, 300000, rep)
	var out []outSample
	s.send = func(c []outSample) error {
		out = append(out, c...)
		return nil
	}
	src := &SourceSurface{}
	for i := 0; i < len(rows); {
		M := rows[i].t / fileMs * fileMs
		if err := s.step(ctx, src, M, false); err != nil {
			t.Fatal(err)
		}
		for ; i < len(rows) && rows[i].t < M+fileMs; i++ {
			r := rows[i]
			r.meta = meta
			s.addRow(r)
		}
	}
	if err := s.step(ctx, src, math.MaxInt64, true); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFixJourneyAnchoredRun(t *testing.T) {
	b := float64(rowBoundary)
	// Moving at 10 tiles/s; two samples really taken at 1500 and 1800 were stamped 3001.
	rows := trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{3001, 5, 0, 10, 0, b},
		[6]float64{3001, 8, 0, 10, 0, b},
		[6]float64{3300, 23, 0, 10, 0, 0},
	)
	rep := newRep()
	got := fixJourney(rows, trainMeta, rep)
	if len(got) != 4 || got[1].t != 1500 || got[2].t != 1800 {
		t.Fatalf("re-timed rows: %+v", got)
	}
	if rep.Retimed["t"] != 2 || rep.BoundaryStamped != 2 || rep.DupSamplesDropped != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.TrainJumpsBefore != 1 || rep.TrainJumpsAfter != 0 {
		t.Fatalf("jumps before %d after %d, want 1 and 0", rep.TrainJumpsBefore, rep.TrainJumpsAfter)
	}
}

func TestFixJourneyForwardAndLimits(t *testing.T) {
	b := float64(rowBoundary)
	// No sample after the run: forward estimate from the previous sample.
	rows := trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{2001, 4, 0, 10, 0, b},
	)
	got := fixJourney(rows, trainMeta, newRep())
	if got[1].t != 1400 {
		t.Fatalf("forward estimate t = %d, want 1400", got[1].t)
	}
	// An estimate more than 5 s before the stamp is not trusted.
	rows = trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{60001, 10, 0, 10, 0, b},
	)
	rep := newRep()
	if got := fixJourney(rows, trainMeta, rep); got[1].t != 60001 || rep.Retimed["t"] != 0 {
		t.Fatalf("far estimate moved the sample: %+v", got)
	}
	// A stopped train is never re-timed; equal times keep the last row.
	rows = trainRows(
		[6]float64{1000, 0, 0, 0, 0, 0},
		[6]float64{3001, 0, 0, 0, 0, b},
		[6]float64{3001, 0.5, 0, 0, 0, b},
	)
	rep = newRep()
	got = fixJourney(rows, trainMeta, rep)
	if len(got) != 2 || got[1].x != 0.5 || rep.DupSamplesDropped != 1 {
		t.Fatalf("stopped/dup rows: %+v rep %+v", got, rep)
	}
}

func TestFixJourneyAfterBoundaryGone(t *testing.T) {
	b := float64(rowBoundary)
	g := float64(rowGone | rowBoundary)
	// A handoff at a window floor: the gone and the next sample were both clamped to
	// 3001; the sample's true time (from the next one) is 2700, the gone moves before it.
	rows := trainRows(
		[6]float64{2000, 0, 0, 10, 0, 0},
		[6]float64{3001, 0, 0, 0, 0, g},
		[6]float64{3001, 10, 0, 10, 0, b},
		[6]float64{3300, 16, 0, 10, 0, 0},
	)
	rep := newRep()
	got := fixJourney(rows, trainMeta, rep)
	if got[1].flags&core.FlagGone == 0 || got[1].t != 2699 || got[2].t != 2700 {
		t.Fatalf("rows: %+v", got)
	}
	if rep.RetimedGones != 1 || rep.Retimed["t"] != 1 || rep.Handoffs != 1 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestFixJourneyEqualStampsStayEqual(t *testing.T) {
	b := float64(rowBoundary)
	// Two samples clamped to the same floor, neither of which can be re-timed (the
	// estimates land within 50 ms of the stamp): they keep the stamp, and the duplicate
	// rule keeps the later arrival instead of pushing it 1 ms later.
	rows := trainRows(
		[6]float64{299000, 0, 0, 10, 0, 0},
		[6]float64{300001, 10, 0, 10, 0, b},
		[6]float64{300001, 10.2, 0, 10, 0, b},
	)
	rep := newRep()
	got := fixJourney(rows, trainMeta, rep)
	if len(got) != 2 || got[0].t != 299000 || got[1].t != 300001 || got[1].x != 10.2 {
		t.Fatalf("rows: %+v", got)
	}
	if rep.DupSamplesDropped != 1 || rep.Retimed["t"] != 0 {
		t.Fatalf("report: %+v", rep)
	}
	// The same when the first estimate is too far back to trust (more than 5 s).
	rows = trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{60001, 10, 0, 10, 0, b},
		[6]float64{60001, 10.5, 0, 10, 0, b},
	)
	rep = newRep()
	got = fixJourney(rows, trainMeta, rep)
	if len(got) != 2 || got[1].t != 60001 || got[1].x != 10.5 || rep.DupSamplesDropped != 1 {
		t.Fatalf("rows: %+v rep %+v", got, rep)
	}
}

func TestFixJourneyEqualStampsOneRetimed(t *testing.T) {
	b := float64(rowBoundary)
	// The first clamped sample is re-timed (5 tiles at 10 tiles/s after the sample at
	// 1000); the second one's estimate is within 50 ms of the stamp, so it keeps it.
	rows := trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{3001, 5, 0, 10, 0, b},
		[6]float64{3001, 19.8, 0, 10, 0, b},
	)
	rep := newRep()
	got := fixJourney(rows, trainMeta, rep)
	if len(got) != 3 || got[1].t != 1500 || got[2].t != 3001 {
		t.Fatalf("rows: %+v", got)
	}
	if rep.Retimed["t"] != 1 || rep.DupSamplesDropped != 0 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestFixJourneyNeverMovesLater(t *testing.T) {
	b := float64(rowBoundary)
	g := float64(rowGone | rowBoundary)
	// A mix of clamped runs, gones and equal stamps: no row may end up later than its
	// stamp, and the result stays strictly increasing.
	rows := trainRows(
		[6]float64{1000, 0, 0, 10, 0, 0},
		[6]float64{3001, 5, 0, 10, 0, b},
		[6]float64{3001, 5, 0, 10, 0, b},
		[6]float64{3001, 7, 0, 10, 0, b},
		[6]float64{3001, 0, 0, 0, 0, g},
		[6]float64{3001, 9, 0, 10, 0, b},
		[6]float64{3001, 9.1, 0, 10, 0, b},
		[6]float64{3100, 10, 0, 10, 0, 0},
		[6]float64{6001, 30, 0, 10, 0, b},
		[6]float64{6001, 30, 0, 10, 0, b},
	)
	stamps := map[float64]int64{}
	for _, r := range rows {
		stamps[r.x] = r.t
	}
	got := fixJourney(rows, trainMeta, newRep())
	for i, r := range got {
		if r.flags&core.FlagGone == 0 && r.t > stamps[r.x] {
			t.Fatalf("row %d moved later: %+v (stamp %d)", i, r, stamps[r.x])
		}
		if i > 0 && got[i-1].t >= r.t {
			t.Fatalf("rows not strictly increasing at %d: %+v", i, got)
		}
	}
}

func TestEndSwitch(t *testing.T) {
	meta := []core.MoverMeta{{ID: "1.2.3", Kind: core.KindTrain, Carriages: 5}}
	rows := trainRows(
		[6]float64{1000, 0, 0, 0, 0, 0},
		[6]float64{3000, 28.5, 0, 0, 0, 0}, // a 5-carriage train is 28 tiles long
		[6]float64{4000, 28.5, 0, 1, 0, 0},
		[6]float64{5000, 80, 0, 1, 0, 0}, // a real jump, not an end switch
	)
	rep := newRep()
	got := fixJourney(rows, meta, rep)
	if rep.EndSwitches != 1 || got[1].flags&core.FlagEndSwitch == 0 || got[3].flags&core.FlagEndSwitch != 0 {
		t.Fatalf("end switches: %d, rows %+v", rep.EndSwitches, got)
	}
	if rep.TrainJumpsAfter != 1 || rep.TrainJumpsBefore != 2 {
		t.Fatalf("jumps before %d after %d, want 2 and 1", rep.TrainJumpsBefore, rep.TrainJumpsAfter)
	}
}

func TestEndSwitchNotAfterGap(t *testing.T) {
	meta := []core.MoverMeta{{ID: "1.2.3", Kind: core.KindTrain, Carriages: 5}}
	// A stationary train that reappears a train length away after a long data gap is
	// not an end switch.
	rows := trainRows(
		[6]float64{1000, 0, 0, 0, 0, 0},
		[6]float64{601000, 28.5, 0, 0, 0, 0},
	)
	rep := newRep()
	got := fixJourney(rows, meta, rep)
	if rep.EndSwitches != 0 || got[1].flags&core.FlagEndSwitch != 0 {
		t.Fatalf("end switch flagged after a 600 s gap: %+v", got)
	}
}

func TestDupPairsResetAtGone(t *testing.T) {
	g := float64(rowGone)
	rows := trainRows(
		[6]float64{1, 0, 0, 0, 0, 0},
		[6]float64{1, 0, 0, 0, 0, 0},
		[6]float64{2, 0, 0, 0, 0, g},
		[6]float64{2, 0, 0, 0, 0, 0},
		[6]float64{2, 0, 0, 0, 0, 0},
	)
	rep := newRep()
	fixJourney(rows, trainMeta, rep)
	if rep.SourceDupPairs != 2 {
		t.Fatalf("dup pairs = %d, want 2", rep.SourceDupPairs)
	}
	// The gone at 2 is dropped (a later row has its time), as are the first rows at 1
	// and 2.
	if rep.DupSamplesDropped != 2 || rep.DupGonesDropped != 1 || rep.Imported.Samples["t"] != 2 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestVehicleHeadingIsFlaggedEstimate(t *testing.T) {
	meta := []core.MoverMeta{{ID: "1.2.v3", Kind: core.KindVehicle}}
	rows := trainRows(
		[6]float64{1000, 0, 0, 0, 0, 0},
		[6]float64{2000, 0, 0, 1, 0, 0},
		[6]float64{3000, 1, 0, 0, 0, 0},
	)
	rep := newRep()
	got := fixJourney(rows, meta, rep)
	for i, r := range got {
		if r.flags&core.FlagNoHeading == 0 {
			t.Fatalf("vehicle row %d without FlagNoHeading: %+v", i, r)
		}
	}
	if got[1].o != 0.25 || got[2].o != 0.25 || rep.HeadingsDerived != 2 || rep.NoHeading != 3 {
		t.Fatalf("headings: %+v rep %+v", got, rep)
	}
}

// genJourney builds a pseudo-random train journey: samples every 150-750 ms at
// 10 tiles/s with occasional pauses, late samples clamped to the next window floor + 1,
// gones (some clamped too) and equal-stamp duplicates.
func genJourney(n int, seed uint32) []srcRow {
	rng := seed
	rnd := func(k int) int {
		rng = rng*1664525 + 1013904223
		return int(rng>>8) % k
	}
	const W = 300000
	var rows []srcRow
	t, x := int64(1_000_000), 0.0
	for len(rows) < n {
		dt := int64(150 + rnd(600))
		if rnd(300) == 0 {
			dt += int64(rnd(20)) * 60000
		}
		t += dt
		x += 10 * float64(dt) / 1000
		stamp := t
		if nf := (t/W + 1) * W; nf-t < 4000 && rnd(3) != 0 {
			stamp = nf + 1
		}
		r := srcRow{t: stamp, x: x, vx: 10}
		switch rnd(40) {
		case 0:
			rows = append(rows, srcRow{t: stamp, flags: rowGone})
			r.t = stamp + int64(rnd(3))
		case 1:
			rows = append(rows, srcRow{t: stamp, x: x - 0.3, vx: 10})
		}
		rows = append(rows, r)
	}
	for i := range rows {
		if rows[i].t%W <= 2 {
			rows[i].flags |= rowBoundary
		}
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].t < rows[b].t })
	return rows
}

// TestStreamingMatchesWholeJourney checks that fixing a journey in watermark steps, as
// the import does, gives exactly what fixing the whole journey at once gives.
func TestStreamingMatchesWholeJourney(t *testing.T) {
	meta := &core.MoverMeta{ID: "9.9.9", Kind: core.KindTrain, Carriages: 2}
	var retimed, gones, dups, handoffs int
	for _, seed := range []uint32{1, 2, 3, 4, 5, 6, 7, 8} {
		rows := genJourney(3000, seed)
		wantRep := newRep()
		want := fixJourney(rows, []core.MoverMeta{*meta}, wantRep)
		retimed += wantRep.Retimed["t"]
		gones += wantRep.RetimedGones
		dups += wantRep.DupSamplesDropped
		handoffs += wantRep.Handoffs
		for _, fileMs := range []int64{500, 1000, 3000, 60000, 300000} {
			rep := newRep()
			got := streamJourney(t, rows, meta, fileMs, rep)
			if len(got) != len(want) {
				t.Fatalf("seed %d file %d: %d samples, whole journey gives %d", seed, fileMs, len(got), len(want))
			}
			for i := range got {
				a, b := got[i], want[i]
				if a.t != b.t || a.x != b.x || a.flags != b.flags {
					t.Fatalf("seed %d file %d: sample %d = %+v, whole journey gives %+v", seed, fileMs, i, a, b)
				}
				if i > 0 && got[i-1].t > a.t {
					t.Fatalf("seed %d file %d: released out of order at %d", seed, fileMs, i)
				}
			}
			if rep.OpenRunCuts != 0 {
				t.Fatalf("seed %d file %d: %d open-run cuts", seed, fileMs, rep.OpenRunCuts)
			}
			if !reflect.DeepEqual(rep, wantRep) {
				t.Fatalf("seed %d file %d: report\n%+v\nwhole journey gives\n%+v", seed, fileMs, rep, wantRep)
			}
		}
	}
	if retimed < 10 || gones == 0 || dups == 0 || handoffs == 0 {
		t.Fatalf("generated journeys exercise too little: %d re-timed, %d re-timed gones, %d dups, %d handoffs", retimed, gones, dups, handoffs)
	}
	t.Logf("%d re-timed, %d re-timed gones, %d dups, %d handoffs", retimed, gones, dups, handoffs)
}

func TestOpenRunCut(t *testing.T) {
	meta := &core.MoverMeta{ID: "9.9.9", Kind: core.KindTrain, Carriages: 2}
	b := rowBoundary
	// A clamped sample whose next row comes 2 h later is fixed (forward from the sample
	// before it) once the watermark is openRunLag past it, and released in order.
	rows := []srcRow{
		{t: 299000, x: 0, vx: 10},
		{t: 300001, x: 4, vx: 10, flags: b},
		{t: 7500000, x: 50, vx: 0},
		{t: 7500500, x: 50, vx: 0},
	}
	rep := newRep()
	got := streamJourney(t, rows, meta, 300000, rep)
	if len(got) != 4 || got[1].t != 299400 || rep.OpenRunCuts != 1 || rep.Retimed["t"] != 1 {
		t.Fatalf("rows %+v report %+v", got, rep)
	}
}
