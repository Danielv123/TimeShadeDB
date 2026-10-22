package core_test

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/coretest"
)

func railRowOf(s core.MoverSample) core.RailRow {
	q := s.Quantized() // as a store keeps it
	return core.RailRow{T: q.T, X: q.X, Y: q.Y, Orientation: q.Orientation, Flags: q.Flags, Rail: q.Rail, RailStart: q.RailStart,
		Carriages: q.Meta.Carriages}
}

func samePieces(a, b []core.RailPiece) bool {
	return len(a) == len(b) && (len(a) == 0 || reflect.DeepEqual(a, b))
}

// TestRailBodyRuns drives trains of several lengths along random tracks of real piece
// placements and checks every sample's body against the pieces earlier samples listed that
// still lie under the train.
func TestRailBodyRuns(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		cars := []int{1, 4, 12, 108}[seed%4]
		meta := coretest.Train
		meta.Carriages = cars
		tr := coretest.NewRailTrack(rng, 3000+float64(cars*14), 1+float64(rng.Intn(20)))
		run := tr.Run(rng, coretest.SFC, meta, coretest.T0, float64(cars*7)+rng.Float64()*20, 3+rng.Float64()*20, 120, []int{0, 7, 25}[seed%3])
		var rb core.RailBody
		nonEmpty := 0
		for i, s := range run.Samples {
			got := rb.Add(railRowOf(s))
			if !samePieces(got, run.Bodies[i]) {
				t.Fatalf("seed %d (%d carriages) sample %d: body\n got  %v\n want %v", seed, cars, i, got, run.Bodies[i])
			}
			if len(got) > 0 {
				nonEmpty++
			}
		}
		t.Logf("seed %d cars %d: %d of %d samples with a body, %d pieces", seed, cars, nonEmpty, len(run.Samples), len(tr.Pieces))
		if nonEmpty == 0 && cars > 1 {
			t.Fatalf("seed %d: no sample has a body", seed)
		}
	}
}

// TestRailBodySeed: a RailBody begun at a row with its body in front of its own pieces (a
// keyframe row) gives the later rows the same bodies as one that saw every row.
func TestRailBodySeed(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	meta := coretest.Train
	meta.Carriages = 30
	tr := coretest.NewRailTrack(rng, 4000, 2)
	run := tr.Run(rng, coretest.SFC, meta, coretest.T0, 250, 8, 150, 40)
	for at := 1; at < len(run.Samples)-1; at += 7 {
		var full core.RailBody
		var body []core.RailPiece
		for i := 0; i <= at; i++ {
			body = full.Add(railRowOf(run.Samples[i]))
		}
		r := railRowOf(run.Samples[at])
		r.Rail = core.RailWithBody(body, r.Rail)
		var seeded core.RailBody
		seeded.Seed(r)
		for i := at + 1; i < len(run.Samples); i++ {
			a, b := full.Add(railRowOf(run.Samples[i])), seeded.Add(railRowOf(run.Samples[i]))
			if !samePieces(a, b) {
				t.Fatalf("seeded at %d, sample %d: %v, full history %v", at, i, b, a)
			}
		}
	}
}

func TestRailBodyLostAndOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	meta := coretest.Train
	meta.Carriages = 20
	tr := coretest.NewRailTrack(rng, 3000, 3)
	run := tr.Run(rng, coretest.SFC, meta, coretest.T0, 200, 6, 100, 30)
	first := -1
	for i, b := range run.Bodies {
		if len(b) > 0 && i > 2 && first < 0 {
			first = i
		}
	}
	if first < 0 {
		t.Fatal("no body in the run")
	}
	// a duplicate row gets the previous body; an older row loses the path until a row
	// that lists the whole train (the next stop)
	var rb core.RailBody
	for i := 0; i < first; i++ {
		rb.Add(railRowOf(run.Samples[i]))
	}
	want := rb.Add(railRowOf(run.Samples[first]))
	if got := rb.Add(railRowOf(run.Samples[first])); !samePieces(got, want) || len(got) == 0 {
		t.Fatalf("duplicate: %v, want %v", got, want)
	}
	if got := rb.Add(railRowOf(run.Samples[first-1])); got != nil {
		t.Fatalf("older row: %v", got)
	}
	stop := -1
	for i := first + 1; i < len(run.Samples); i++ {
		got := rb.Add(railRowOf(run.Samples[i]))
		if run.Samples[i].Flags&core.FlagStop != 0 && stop < 0 {
			stop = i
		}
		if stop < 0 && got != nil {
			t.Fatalf("sample %d after the path was lost: %v", i, got)
		}
		if stop >= 0 && !samePieces(got, run.Bodies[i]) {
			t.Fatalf("sample %d after the stop at %d: %v, want %v", i, stop, got, run.Bodies[i])
		}
	}
	if stop < 0 {
		t.Fatal("no stop after the lost path")
	}
	// a piece that neither continues the path nor is on it loses it too
	var rb2 core.RailBody
	for i := 0; i < first; i++ {
		rb2.Add(railRowOf(run.Samples[i]))
	}
	r := railRowOf(run.Samples[first])
	r.Rail = append([]core.RailPiece{{X: 9999, Y: 9999, Dir: 3, Kind: core.RailKindCurveB}}, r.Rail...)
	if got := rb2.Add(r); got != nil {
		t.Fatalf("missed piece: %v", got)
	}
	if got := rb2.Add(railRowOf(run.Samples[first+1])); got != nil && run.Samples[first].Flags&core.FlagStop == 0 {
		t.Fatalf("after a missed piece: %v", got)
	}
	// a gone ends the run; the next run starts afresh
	var rb3 core.RailBody
	for i := 0; i <= first; i++ {
		rb3.Add(railRowOf(run.Samples[i]))
	}
	g := railRowOf(run.Samples[first+1])
	g.Flags = core.FlagGone
	if rb3.Add(g) != nil || rb3.Add(railRowOf(run.Samples[first+2])) != nil {
		t.Fatal("a row after a gone has a body")
	}
}

func TestRailWithBody(t *testing.T) {
	p := func(i int) core.RailPiece { return core.RailPiece{X: int32(i), Kind: core.RailKindRamp} }
	if got := core.RailWithBody(nil, []core.RailPiece{p(1)}); !reflect.DeepEqual(got, []core.RailPiece{p(1)}) {
		t.Fatal(got)
	}
	got := core.RailWithBody([]core.RailPiece{p(1), p(2)}, []core.RailPiece{p(3)})
	if !reflect.DeepEqual(got, []core.RailPiece{p(1), p(2), p(3)}) {
		t.Fatal(got)
	}
	var body, rail []core.RailPiece
	for i := 0; i < core.MaxRailPieces; i++ {
		body = append(body, p(i))
	}
	rail = []core.RailPiece{p(-1), p(-2)}
	got = core.RailWithBody(body, rail)
	if len(got) != core.MaxRailPieces || got[0] != p(2) || got[len(got)-1] != p(-2) {
		t.Fatal(fmt.Sprint(len(got), got[0], got[len(got)-1]))
	}
}

// TestRailBodyRepeats: a row given again (a retried sample) repeats and gets the same body;
// a row at the same time with anything RailBody reads changed does not repeat.
func TestRailBodyRepeats(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	meta := coretest.Train
	meta.Carriages = 20
	tr := coretest.NewRailTrack(rng, 3000, 2)
	run := tr.Run(rng, coretest.SFC, meta, coretest.T0, 160, 6, 100, 30)
	var rb core.RailBody
	if rb.Repeats(railRowOf(run.Samples[0])) {
		t.Fatal("an empty RailBody repeats a row")
	}
	for i, s := range run.Samples {
		r := railRowOf(s)
		body := rb.Add(r)
		if !rb.Repeats(r) {
			t.Fatalf("sample %d does not repeat itself", i)
		}
		if again := rb.Add(r); !samePieces(again, body) {
			t.Fatalf("sample %d again: body %v, first %v", i, again, body)
		}
		moved := r
		moved.X += 0.5
		extra := r
		extra.Rail = append(append([]core.RailPiece(nil), r.Rail...), core.RailPiece{X: 1, Y: 2, Kind: core.RailKindRamp})
		later := r
		later.T++
		if rb.Repeats(moved) || rb.Repeats(extra) || rb.Repeats(later) {
			t.Fatalf("sample %d: a changed row repeats", i)
		}
	}
}
