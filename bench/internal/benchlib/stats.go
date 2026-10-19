package benchlib

import (
	"math"
	"sort"
	"time"
)

// Summary is a sample's size, mean, maximum and nearest-rank percentiles.
type Summary struct {
	N                  int
	Mean, Max          float64
	P50, P90, P95, P99 float64
}

// Summarize sorts a copy of xs and computes its Summary. NaN values are dropped.
func Summarize(xs []float64) Summary {
	s := make([]float64, 0, len(xs))
	for _, x := range xs {
		if !math.IsNaN(x) {
			s = append(s, x)
		}
	}
	if len(s) == 0 {
		return Summary{}
	}
	sort.Float64s(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	return Summary{
		N:    len(s),
		Mean: sum / float64(len(s)),
		Max:  s[len(s)-1],
		P50:  Percentile(s, 0.50),
		P90:  Percentile(s, 0.90),
		P95:  Percentile(s, 0.95),
		P99:  Percentile(s, 0.99),
	}
}

// Percentile returns the nearest-rank percentile p (0..1) of an ascending slice.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if p <= 0 {
		return sorted[0]
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// Ms converts a duration to float milliseconds.
func Ms(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

// Median of a few values (startup repetitions), without modifying xs.
func Median(xs []float64) float64 { return Summarize(xs).P50 }

// LinearSlope fits y = a + b·x by least squares and returns b. It returns NaN with fewer
// than two distinct x values.
func LinearSlope(pts [][2]float64) float64 {
	n := float64(len(pts))
	if n < 2 {
		return math.NaN()
	}
	var sx, sy, sxx, sxy float64
	for _, p := range pts {
		sx += p[0]
		sy += p[1]
		sxx += p[0] * p[0]
		sxy += p[0] * p[1]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return math.NaN()
	}
	return (n*sxy - sx*sy) / den
}
