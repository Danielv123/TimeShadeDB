package benchlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"timeshadedb/internal/v3/core"
)

// IngestOutcome is what the server reports it did with the rows of one body.
type IngestOutcome struct {
	Accepted, Late, Invalid, BadLines int
	Errors                            []string
}

// Add accumulates another outcome.
func (o *IngestOutcome) Add(x IngestOutcome) {
	o.Accepted += x.Accepted
	o.Late += x.Late
	o.Invalid += x.Invalid
	o.BadLines += x.BadLines
	if len(o.Errors) < 10 {
		o.Errors = append(o.Errors, x.Errors[:min(len(x.Errors), 10-len(o.Errors))]...)
	}
}

// Shortfall returns an error when the server did not store every row it was sent.
func (o IngestOutcome) Shortfall(rows int) error {
	if o.BadLines == 0 && o.Accepted >= rows {
		return nil
	}
	return fmt.Errorf("server stored %d of %d rows (late %d, invalid %d, bad lines %d; errors: %s)",
		o.Accepted, rows, o.Late, o.Invalid, o.BadLines, strings.Join(o.Errors, "; "))
}

// ParseIngest reads a 200 answer to an ingest body. A v3 server answers 200 even when it
// skipped lines or rejected messages (API.md 5), so the counts in core.IngestResponse are
// what says whether the rows were stored. v2 answers an error status for bad rows, so a
// 200 counts every row as accepted.
func ParseIngest(b Body, r Response) (IngestOutcome, error) {
	if !strings.HasPrefix(b.Path, "/api/v3/") {
		return IngestOutcome{Accepted: b.Rows}, nil
	}
	var ir core.IngestResponse
	if err := json.Unmarshal(r.Body, &ir); err != nil {
		return IngestOutcome{}, fmt.Errorf("ingest answer is not an IngestResponse: %v: %s", err, truncate(r.Body, 200))
	}
	c := ir.Chunks
	if b.Kind == "movers" {
		c = ir.Samples
	}
	return IngestOutcome{Accepted: c.Accepted, Late: c.Late, Invalid: c.Invalid, BadLines: ir.BadLines, Errors: ir.Errors}, nil
}

// MaxRetryWait caps the time PostIngest spends waiting on 503 answers for one body.
var MaxRetryWait = 2 * time.Minute

// ErrOverloaded is returned when a body was still refused with 503 after MaxRetryWait.
var ErrOverloaded = errors.New("server still overloaded (503) after the retry limit")

// PostIngest posts one body, retrying 503 answers after their Retry-After (default
// 100 ms) for at most maxWait in total and never past ctx. It returns the final
// response, the server's account of the rows, the number of retries and the time spent
// waiting. Any final status other than 200 is an error.
func PostIngest(ctx context.Context, c *Client, b Body, maxWait time.Duration) (Response, IngestOutcome, int, time.Duration, error) {
	var waited time.Duration
	retries := 0
	var lat time.Duration
	for {
		r, err := c.Post(ctx, b)
		if err != nil {
			return r, IngestOutcome{}, retries, waited, err
		}
		lat += r.Dur
		if r.Status == http.StatusServiceUnavailable {
			wait := 100 * time.Millisecond
			if ra, err := strconv.Atoi(r.Header.Get("Retry-After")); err == nil && ra > 0 {
				wait = time.Duration(ra) * time.Second
			}
			if waited+wait > maxWait {
				return r, IngestOutcome{}, retries, waited, fmt.Errorf("POST %s: %w (%d retries, %s waited)", b.Path, ErrOverloaded, retries, waited)
			}
			retries++
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return r, IngestOutcome{}, retries, waited, ctx.Err()
			case <-t.C:
			}
			waited += wait
			continue
		}
		r.Dur = lat
		if r.Status != http.StatusOK {
			return r, IngestOutcome{}, retries, waited, fmt.Errorf("POST %s: HTTP %d: %s", b.Path, r.Status, truncate(r.Body, 300))
		}
		o, err := ParseIngest(b, r)
		return r, o, retries, waited, err
	}
}

// PipelineOptions tune SendPipeline.
type PipelineOptions struct {
	// AllowRejected records rows the server did not store (late, invalid, bad lines)
	// instead of failing.
	AllowRejected bool
	// MaxRetryWait caps the 503 waiting per body (default MaxRetryWait).
	MaxRetryWait time.Duration
}

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
	// Retries counts 503 answers that were retried (v3 backpressure); RetryWait is the
	// time spent waiting for them.
	Retries   int
	RetryWait time.Duration
	// Outcome is the server's account of the rows (v3 IngestResponse counts).
	Outcome IngestOutcome
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

// StoredRate returns the rows the server reported as stored per second over Wall.
func (s SendStats) StoredRate() float64 {
	if s.Wall <= 0 {
		return 0
	}
	return float64(min(s.Rows, s.Outcome.Accepted)) / s.Wall.Seconds()
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
// is retried as PostIngest does. Unless opt.AllowRejected is set, a body whose rows the
// server did not all store ends the run with an error.
func SendPipeline[B any](ctx context.Context, c *Client, conc, workers int, opt PipelineOptions, gen func() (B, bool), encode func(B) ([]Body, error)) (SendStats, error) {
	if opt.MaxRetryWait <= 0 {
		opt.MaxRetryWait = MaxRetryWait
	}
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
				r, outcome, retries, waited, err := PostIngest(ctx, c, b, opt.MaxRetryWait)
				if err != nil {
					setErr(err)
					return
				}
				if !opt.AllowRejected {
					if err := outcome.Shortfall(b.Rows); err != nil {
						setErr(fmt.Errorf("POST %s: %w", b.Path, err))
						return
					}
				}
				lat := r.Dur
				stMu.Lock()
				if len(st.Progress) > 0 || st.Requests > 0 {
					st.Starved += starved
				}
				st.Rows += b.Rows
				st.Requests++
				st.Busy += lat
				st.Retries += retries
				st.RetryWait += waited
				st.Outcome.Add(outcome)
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
