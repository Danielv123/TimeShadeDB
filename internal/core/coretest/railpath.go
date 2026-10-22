package coretest

import (
	"math"
	"math/rand"

	"timeshadedb/internal/core"
)

// RailTrack is a random chain of connected curved pieces and ramps with straight rails
// between them (docs/RAIL-GEOMETRY.md 8.2), for tests of train bodies.
type RailTrack struct {
	Pieces []core.RailPiece
	// Entry and Exit are each piece's rail distance at its entry and exit end, from the
	// start of the track.
	Entry, Exit []float64
	// exits are each piece's exit end (x, y, direction); exits[-1] is the track's start.
	start [3]int32
	exits [][3]int32
}

// NewRailTrack makes a track of at least length tiles of rail distance from (0, 0) heading
// north: pieces chosen at random among the placements whose entry faces the path's exit,
// with straight rails of at least minGap tiles between them.
func NewRailTrack(rng *rand.Rand, length, minGap float64) *RailTrack {
	tr := &RailTrack{start: [3]int32{0, 0, 0}}
	ex := tr.start
	u := 0.0
	// pieces are kept 24 tiles apart (but from the one before), so the track never runs
	// over a piece twice: a real track that does is a loop, which RAIL-GEOMETRY.md 8.2 covers
	// and these tests do not
	clear := func(p core.RailPiece) bool {
		for i := 0; i+1 < len(tr.Pieces); i++ {
			q := tr.Pieces[i]
			if math.Hypot(float64(p.X-q.X), float64(p.Y-q.Y)) < 24 {
				return false
			}
		}
		return true
	}
	for tries := 0; u < length; tries++ {
		sx, sy := core.RailDirStep(uint8(ex[2]))
		step := math.Hypot(float64(sx), float64(sy))
		k := int32(math.Ceil(minGap/step)) + int32(rng.Intn(6+tries))
		gx, gy := ex[0]+k*sx, ex[1]+k*sy
		type cand struct {
			p    core.RailPiece
			exit [3]int32
		}
		var cands []cand
		for kind := uint8(core.RailKindCurveA); kind <= core.RailKindRamp; kind++ {
			for d := uint8(0); d < 16; d++ {
				p := core.RailPiece{Dir: d, Kind: kind}
				f, b := core.RailPieceEnds(p)
				for _, e := range [][2][3]int32{{f, b}, {b, f}} {
					if e[0][2] == (ex[2]+8)%16 {
						p.X, p.Y = gx-e[0][0], gy-e[0][1]
						if clear(p) {
							cands = append(cands, cand{p, [3]int32{p.X + e[1][0], p.Y + e[1][1], e[1][2]}})
						}
					}
				}
			}
		}
		if len(cands) == 0 {
			continue // a longer straight
		}
		tries = 0
		c := cands[rng.Intn(len(cands))]
		u += float64(k) * step
		tr.Pieces = append(tr.Pieces, c.p)
		tr.Entry = append(tr.Entry, u)
		u += core.RailPieceLength(c.p.Kind)
		tr.Exit = append(tr.Exit, u)
		tr.exits = append(tr.exits, c.exit)
		ex = c.exit
	}
	return tr
}

// At returns the point at rail distance u and the direction of travel there, for u on the
// straight rails between pieces (or after the last); ok is false inside a piece.
func (tr *RailTrack) At(u float64) (x, y, ux, uy float64, ok bool) {
	ex, from := tr.start, 0.0
	for i := range tr.Pieces {
		if u < tr.Entry[i] {
			break
		}
		if u < tr.Exit[i] {
			return 0, 0, 0, 0, false
		}
		ex, from = tr.exits[i], tr.Exit[i]
	}
	sx, sy := core.RailDirStep(uint8(ex[2]))
	l := math.Hypot(float64(sx), float64(sy))
	ux, uy = float64(sx)/l, float64(sy)/l
	return float64(ex[0]) + (u-from)*ux, float64(ex[1]) + (u-from)*uy, ux, uy, true
}

// RailRun is a train's rail path samples along a RailTrack with what each sample's body
// must be.
type RailRun struct {
	Samples []core.MoverSample
	// Bodies are the pieces under the train (rear first) that samples before each one
	// listed: the body a keyframe or live payload row of that sample carries.
	Bodies [][]core.RailPiece
}

// Run drives a train of meta.Carriages carriages along tr from rail distance u0, a sample
// every step to 2 step tiles (on the straight rails only), the first with RailStart and the
// pieces under the train, and a stop and a start (each listing the pieces under the train
// in front of those entered) every stopEvery samples (0: none). Times are 1 s apart from
// t0, ticks 60 a second.
func (tr *RailTrack) Run(rng *rand.Rand, sfc core.SurfaceKey, meta core.MoverMeta, t0 core.TimeMs, u0, step float64, n, stopEvery int) RailRun {
	var run RailRun
	trainLen := float64(7*meta.Carriages - 3)
	reach := trainLen + 4
	listed := map[int]bool{}
	u, prevU := u0, math.Inf(-1)
	stopped := false
	for i := 0; i < n; i++ {
		// on a straight, and not within 0.1 tile of where a piece leaves the reach (stores
		// round positions to 1/16 tile)
		x, y, ux, uy, ok := tr.At(u)
		for !ok || tr.nearReach(u, reach) {
			u += 0.25
			x, y, ux, uy, ok = tr.At(u)
		}
		s := core.MoverSample{Surface: sfc, Meta: meta, T: t0 + core.TimeMs(i)*1000, Tick: int64(i) * 60,
			X: x - 2*ux, Y: y - 2*uy, Orientation: math.Mod(math.Atan2(ux, -uy)/(2*math.Pi)+1, 1),
			Flags: core.FlagRail, Speed: 0.5}
		stop := stopEvery > 0 && i > 0 && i%stopEvery == 0
		whole := i == 0 || stop || stopped
		var body []core.RailPiece
		for k, p := range tr.Pieces {
			under := tr.Entry[k] <= u && tr.Exit[k] > u-trainLen
			entered := tr.Entry[k] > prevU && tr.Entry[k] <= u
			switch {
			case whole && under && !entered, entered:
				s.Rail = append(s.Rail, p)
			}
			if listed[k] && !entered && tr.Entry[k] <= prevU && u-tr.Exit[k] <= reach {
				body = append(body, p)
			}
		}
		if i == 0 {
			s.RailStart = true
		}
		if whole {
			body = nil // the sample lists the pieces under the train itself
		}
		if stop {
			s.Flags |= core.FlagStop
			s.Speed = 0
		}
		for k := range tr.Pieces {
			if tr.Entry[k] <= u && (tr.Entry[k] > prevU || (whole && tr.Exit[k] > u-trainLen)) {
				listed[k] = true
			}
		}
		run.Samples = append(run.Samples, s)
		run.Bodies = append(run.Bodies, body)
		stopped = stop
		prevU = u
		if !stop {
			u += step + rng.Float64()*step
		}
	}
	return run
}

func (tr *RailTrack) nearReach(u, reach float64) bool {
	for k := range tr.Pieces {
		if d := u - tr.Exit[k] - reach; d > -0.1 && d < 0.1 {
			return true
		}
	}
	return false
}
