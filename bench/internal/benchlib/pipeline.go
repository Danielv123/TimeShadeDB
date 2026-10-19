package benchlib

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// SendStats is what SendPipeline measured.
type SendStats struct {
	Rows     int
	Requests int
	// Wall is from the first request start to the last response.
	Wall time.Duration
	// Busy is the summed request latency (all senders).
	Busy time.Duration
	// Starved is how long senders waited for the encoder; a large value means the
	// client, not the server, limited throughput.
	Starved time.Duration
	// Retries counts 503 answers that were retried (v3 backpressure).
	Retries int
	// Progress has one (cumulative rows, seconds since start) point per completed request.
	Progress [][2]float64
	// Latencies of each request in ms.
	Latencies []float64
}

// Rate returns rows per second over Wall.
func (s SendStats) Rate() float64 {
	if s.Wall <= 0 {
		return 0
	}
	return float64(s.Rows) / s.Wall.Seconds()
}

// SliceRates splits the progress into n equal row slices and returns rows/s for each.
func (s SendStats) SliceRates(n int) []float64 {
	if n < 1 || len(s.Progress) == 0 || s.Rows == 0 {
		return nil
	}
	out := make([]float64, 0, n)
	prevRows, prevT := 0.0, 0.0
	j := 0
	for k := 1; k <= n; k++ {
		limit := float64(s.Rows) * float64(k) / float64(n)
		for j < len(s.Progress)-1 && s.Progress[j][0] < limit {
			j++
		}
		rows, t := s.Progress[j][0], s.Progress[j][1]
		if t > prevT && rows > prevRows {
			out = append(out, (rows-prevRows)/(t-prevT))
		} else {
			out = append(out, 0)
		}
		prevRows, prevT = rows, t
	}
	return out
}

// Flatness is the throughput of the last slice divided by that of slice 1 (slice 0 is
// warm-up, which also holds the initial charting of every chunk): about 1 when ingest
// does not slow down as history grows, below 1 when it does.
func Flatness(rates []float64) float64 {
	if len(rates) < 3 || rates[1] == 0 {
		return 0
	}
	return rates[len(rates)-1] / rates[1]
}

// SendPipeline takes batches from gen (called sequentially; ok=false ends the stream),
// encodes them with encode on `workers` goroutines, keeps their order, and posts them
// with `conc` senders (conc 1 keeps the stream in order, as a tailer does). A 503 answer
// is retried after its Retry-After (default 100 ms).
func SendPipeline[B any](ctx context.Context, c *Client, conc, workers int, gen func() (B, bool), encode func(B) ([]Body, error)) (SendStats, error) {
	if workers <= 0 {
		workers = max(1, runtime.NumCPU()/2)
	}
	if conc <= 0 {
		conc = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type job struct {
		idx    int
		bodies []Body
		err    error
	}
	jobs := make(chan job, workers*2)
	var genMu sync.Mutex
	idx := 0
	done := false
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				genMu.Lock()
				if done || ctx.Err() != nil {
					genMu.Unlock()
					return
				}
				batch, ok := gen()
				my := idx
				idx++
				if !ok {
					done = true
				}
				genMu.Unlock()
				var bodies []Body
				var err error
				if ok {
					bodies, err = encode(batch)
					if err == nil && bodies == nil {
						bodies = []Body{}
					}
				}
				if err != nil || bodies == nil {
					genMu.Lock()
					done = true
					genMu.Unlock()
				}
				select {
				case jobs <- job{idx: my, bodies: bodies, err: err}:
				case <-ctx.Done():
					return
				}
				if err != nil || bodies == nil {
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(jobs)
	}()
	// Reorder.
	ordered := make(chan Body, 64)
	var firstErr error
	var errMu sync.Mutex
	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}
	go func() {
		defer close(ordered)
		pending := map[int]job{}
		want := 0
		finished := false
		for j := range jobs {
			pending[j.idx] = j
			for !finished {
				p, ok := pending[want]
				if !ok {
					break
				}
				delete(pending, want)
				want++
				if p.err != nil {
					setErr(p.err)
					finished = true
					break
				}
				if p.bodies == nil {
					finished = true
					break
				}
				for _, b := range p.bodies {
					select {
					case ordered <- b:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	var st SendStats
	var stMu sync.Mutex
	var start time.Time
	var startOnce sync.Once
	var senders sync.WaitGroup
	for s := 0; s < conc; s++ {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for {
				w0 := time.Now()
				b, ok := <-ordered
				if !ok {
					return
				}
				startOnce.Do(func() { start = time.Now() })
				starved := time.Since(w0)
				var lat time.Duration
				retries := 0
				for {
					r, err := c.Post(ctx, b)
					if err != nil {
						setErr(err)
						return
					}
					lat += r.Dur
					if r.Status == http.StatusServiceUnavailable {
						retries++
						wait := 100 * time.Millisecond
						if ra, err := strconv.Atoi(r.Header.Get("Retry-After")); err == nil && ra > 0 {
							wait = time.Duration(ra) * time.Second
						}
						time.Sleep(wait)
						continue
					}
					if r.Status != http.StatusOK {
						setErr(fmt.Errorf("POST %s: HTTP %d: %s", b.Path, r.Status, truncate(r.Body, 300)))
						return
					}
					break
				}
				stMu.Lock()
				if len(st.Progress) > 0 || st.Requests > 0 {
					st.Starved += starved
				}
				st.Rows += b.Rows
				st.Requests++
				st.Busy += lat
				st.Retries += retries
				st.Latencies = append(st.Latencies, Ms(lat))
				st.Progress = append(st.Progress, [2]float64{float64(st.Rows), time.Since(start).Seconds()})
				st.Wall = time.Since(start)
				stMu.Unlock()
			}
		}()
	}
	senders.Wait()
	cancel()
	return st, firstErr
}
