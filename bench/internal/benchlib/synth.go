package benchlib

import (
	"math"
	"math/rand"
	"sort"

	"timeshadedb/internal/core"
)

// SynthConfig describes the synthetic workload modelled on the illisis test data
// (PLAN.md §5): a charted area of Chunks chunks, terrain updates where HotShare of the
// updates hit HotFraction of the chunks (Zipf inside the hot set, like biters and belts
// flickering), and Trains trains moving on closed loops.
type SynthConfig struct {
	Seed    int64
	Map     string
	Surface string
	Force   string
	// Chunks is the number of charted chunks (a square around the origin).
	Chunks int
	// HotFraction of the chunks receives HotShare of the updates. Defaults 0.004 / 0.97.
	HotFraction, HotShare float64
	// UpdateRate is chunk updates per data second after the initial charting.
	UpdateRate float64
	// Trains moving at once, sampled every TrainSampleMs data ms.
	Trains        int
	TrainSampleMs int64
	// Start is the first data time (ms).
	Start int64
	// Colors is the palette size of the terrain (<= 235 on the test data).
	Colors int
}

// DefaultSynth returns the default workload.
func DefaultSynth() SynthConfig {
	return SynthConfig{
		Seed: 1, Map: "synth", Surface: "nauvis", Force: "player",
		Chunks: 8000, HotFraction: 0.004, HotShare: 0.97,
		UpdateRate: 200, Trains: 25, TrainSampleMs: 300,
		Start: 1791630000000, Colors: 200,
	}
}

// Synth generates the workload in data-time order.
type Synth struct {
	Cfg    SynthConfig
	rng    *rand.Rand
	coords []core.ChunkCoord
	state  []core.ChunkPixels
	hot    []int
	cold   []int
	zipf   *rand.Zipf
	pal    []uint16
	// next initial chunk to chart
	charted int
	t       float64 // data ms
	trains  []synthTrain
	nextMov float64
	movIdx  int
}

type synthTrain struct {
	id         string
	cx, cy, r  float64 // loop centre and radius (tiles)
	phase, spd float64 // radians, radians per second
}

// NewSynth prepares the generator.
func NewSynth(cfg SynthConfig) *Synth {
	d := DefaultSynth()
	if cfg.HotFraction == 0 {
		cfg.HotFraction = d.HotFraction
	}
	if cfg.HotShare == 0 {
		cfg.HotShare = d.HotShare
	}
	if cfg.Colors == 0 {
		cfg.Colors = d.Colors
	}
	if cfg.TrainSampleMs == 0 {
		cfg.TrainSampleMs = d.TrainSampleMs
	}
	if cfg.Map == "" {
		cfg.Map, cfg.Surface, cfg.Force = d.Map, d.Surface, d.Force
	}
	s := &Synth{Cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed)), t: float64(cfg.Start), nextMov: float64(cfg.Start)}
	side := int(math.Ceil(math.Sqrt(float64(cfg.Chunks))))
	for i := 0; i < cfg.Chunks; i++ {
		s.coords = append(s.coords, core.ChunkCoord{X: int32(i%side - side/2), Y: int32(i/side - side/2)})
	}
	for i := 0; i < cfg.Colors; i++ {
		s.pal = append(s.pal, uint16(s.rng.Intn(65535)+1))
	}
	s.state = make([]core.ChunkPixels, cfg.Chunks)
	for i := range s.state {
		s.fillTerrain(&s.state[i])
	}
	perm := s.rng.Perm(cfg.Chunks)
	nHot := max(1, int(math.Round(cfg.HotFraction*float64(cfg.Chunks))))
	s.hot, s.cold = perm[:nHot], perm[nHot:]
	if nHot > 1 {
		s.zipf = rand.NewZipf(s.rng, 1.1, 1, uint64(nHot-1))
	}
	for i := 0; i < cfg.Trains; i++ {
		r := 40 + s.rng.Float64()*200
		s.trains = append(s.trains, synthTrain{
			id: "0.0." + itoa(i+1),
			cx: (s.rng.Float64() - 0.5) * float64(side) * 16, cy: (s.rng.Float64() - 0.5) * float64(side) * 16,
			r: r, phase: s.rng.Float64() * 2 * math.Pi,
			// 10-60 tiles per second along the loop.
			spd: (10 + s.rng.Float64()*50) / r,
		})
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// fillTerrain draws blocky terrain: a few colour patches, like a chart.
func (s *Synth) fillTerrain(px *core.ChunkPixels) {
	base := s.pal[s.rng.Intn(len(s.pal))]
	for i := range px {
		px[i] = base
	}
	for p := 0; p < 3; p++ {
		c := s.pal[s.rng.Intn(len(s.pal))]
		x0, y0 := s.rng.Intn(32), s.rng.Intn(32)
		w, h := 1+s.rng.Intn(16), 1+s.rng.Intn(16)
		for y := y0; y < min(32, y0+h); y++ {
			for x := x0; x < min(32, x0+w); x++ {
				px[y*32+x] = c
			}
		}
	}
}

// MoverTime is the data time of the next mover sample round.
func (s *Synth) MoverTime() int64 { return int64(s.nextMov) }

// ChunkCount is the number of charted chunks.
func (s *Synth) ChunkCount() int { return len(s.coords) }

// DataTime is the current data time (ms).
func (s *Synth) DataTime() int64 { return int64(s.t) }

// NextChunks returns the next n chunk updates. The first Chunks updates chart every
// chunk once (1 ms apart); after that updates arrive at UpdateRate per data second.
func (s *Synth) NextChunks(n int) []ChunkRow {
	out := make([]ChunkRow, 0, n)
	for len(out) < n {
		var i int
		if s.charted < len(s.coords) {
			i = s.charted
			s.charted++
			s.t++
		} else {
			if s.rng.Float64() < s.Cfg.HotShare || len(s.cold) == 0 {
				k := 0
				if s.zipf != nil {
					k = int(s.zipf.Uint64())
				}
				i = s.hot[k]
			} else {
				i = s.cold[s.rng.Intn(len(s.cold))]
			}
			// Flicker: a small group of pixels toggles to an enemy-red dot or back.
			px := &s.state[i]
			p := s.rng.Intn(core.ChunkPixelCount)
			for k := 0; k < 1+s.rng.Intn(6); k++ {
				q := (p + k) % core.ChunkPixelCount
				if px[q] == 0xF8C3 {
					px[q] = s.pal[s.rng.Intn(len(s.pal))]
				} else {
					px[q] = 0xF8C3
				}
			}
			if s.Cfg.UpdateRate > 0 {
				s.t += 1000 / s.Cfg.UpdateRate
			} else {
				s.t++
			}
		}
		c := s.coords[i]
		pix := s.state[i]
		out = append(out, ChunkRow{T: int64(s.t), Map: s.Cfg.Map, Surface: s.Cfg.Surface, Force: s.Cfg.Force, CX: c.X, CY: c.Y, Pixels: &pix})
	}
	return out
}

// NextMovers returns the next n mover samples: every train sampled every TrainSampleMs
// of data time, so the data time advances by TrainSampleMs per Trains samples. Mover data
// time is independent of the chunk stream's.
func (s *Synth) NextMovers(n int) []MoverRow {
	out := make([]MoverRow, 0, n)
	if len(s.trains) == 0 {
		return out
	}
	for len(out) < n {
		t := s.nextMov
		tr := &s.trains[s.movIdx]
		a := tr.phase + tr.spd*(t-float64(s.Cfg.Start))/1000
		x, y := tr.cx+tr.r*math.Cos(a), tr.cy+tr.r*math.Sin(a)
		// Direction of travel as orientation 0..1, 0 north and 0.25 east (y grows south).
		dx, dy := -math.Sin(a), math.Cos(a)
		h := math.Mod(math.Atan2(dx, -dy)/(2*math.Pi)+1, 1)
		out = append(out, MoverRow{T: int64(t), Map: s.Cfg.Map, Surface: s.Cfg.Surface, Entry: core.MoveEntry{
			J: tr.id, K: "t", F: s.Cfg.Force, Name: "locomotive",
			X: math.Round(x*16) / 16, Y: math.Round(y*16) / 16, H: &h, N: 4, O: "LCCl", C: "ff8800",
		}})
		s.movIdx++
		if s.movIdx == len(s.trains) {
			s.movIdx = 0
			s.nextMov += float64(s.Cfg.TrainSampleMs)
		}
	}
	return out
}

// AlignMovers makes the mover samples continue from the chunk stream's current data
// time when that is later. A v3 server on the data clock (--clock data) has moved its
// clock with the chunks, so movers starting back at Start would arrive behind it.
func (s *Synth) AlignMovers() {
	if s.t > s.nextMov {
		s.nextMov = math.Ceil(s.t)
	}
}

// HotChunks returns the coordinates of the hot set (for tests).
func (s *Synth) HotChunks() []core.ChunkCoord {
	out := make([]core.ChunkCoord, len(s.hot))
	for i, h := range s.hot {
		out[i] = s.coords[h]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].X < out[j].X || (out[i].X == out[j].X && out[i].Y < out[j].Y) })
	return out
}
