package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"timeshadedb/internal/v3/tileenc"
)

// Options configure a Publisher.
type Options struct {
	// Encoding is the Content-Encoding of stored objects: "gzip" (default), "zstd" or
	// "identity". Images and bodies under 256 bytes are stored as they are.
	Encoding string
	// GzipLevel is the gzip level (default 6; objects are compressed once and served
	// many times).
	GzipLevel int
	// KeyframeEvery N > 1 exports a z0 tile version as a full tile only when it is 0, 1,
	// the newest, or a multiple of N; the others are reachable through the consecutive
	// tile deltas only (README.md "Tile versions"). Default 1: every version.
	KeyframeEvery int
	// NoMovers and NoOverlays leave those out.
	NoMovers   bool
	NoOverlays bool
	// Web is the viewer bundle (nil: no viewer files).
	Web fs.FS
	// LiveMs is written to the catalog's live_ms (0: keep the server's).
	LiveMs int
	// Workers is the number of objects read and stored at once (default 8).
	Workers int
	// MaxAttempts bounds the attempts to store one object when the target fails with a
	// network error, 5xx or 429 (default 6; negative: unbounded). When they run out
	// the round stops, before the catalog, and the queue is kept for the next round.
	MaxAttempts int
	// Backoff is the first retry delay (default 500 ms), doubled per attempt up to 30 s.
	Backoff time.Duration
	// Now is the clock that names today's timeline (default time.Now).
	Now func() time.Time
	// Logf receives progress and warnings (default: discard).
	Logf func(format string, args ...any)
}

// Stats count what a round did.
type Stats struct {
	Planned   int   // objects in the round
	Resumed   int   // objects taken over from a stopped round's queue
	Uploaded  int   // objects stored
	Immutable int   // of those, immutable
	Bytes     int64 // bytes stored
	Skipped   int   // immutable objects the target already holds (not read)
	Unchanged int   // mutable objects whose bytes did not change (read, not stored)
	Missing   int   // objects the source no longer has (404)
	Empty     int   // overlay files without intervals that were not stored
	Rejected  int   // objects the target refused (4xx other than 401, 403, 408, 429)
	Failed    int   // objects the source could not read (other errors)
	Retries   int   // failed attempts that were retried
	Duration  time.Duration
}

func (s Stats) String() string {
	return fmt.Sprintf("%d objects (%d resumed): %d stored (%d immutable, %d bytes), %d already stored, %d unchanged, %d missing, %d empty, %d rejected, %d failed, %d retries, %v",
		s.Planned, s.Resumed, s.Uploaded, s.Immutable, s.Bytes, s.Skipped, s.Unchanged, s.Missing, s.Empty, s.Rejected, s.Failed, s.Retries, s.Duration.Round(time.Millisecond))
}

// Publisher writes a static export of a Source to a Target, remembering in a State
// what the target holds.
type Publisher struct {
	src   Source
	dst   Target
	state *State
	opt   Options
	enc   tileenc.Encoding
	comp  *tileenc.Compressor
}

// ErrAuth is returned when the target refuses the credentials (401 or 403).
var ErrAuth = errors.New("target refused the credentials")

// New returns a publisher. The caller owns state and closes it.
func New(src Source, dst Target, state *State, opt Options) (*Publisher, error) {
	if opt.Encoding == "" {
		opt.Encoding = "gzip"
	}
	var enc tileenc.Encoding
	switch opt.Encoding {
	case "gzip":
		enc = tileenc.Gzip
	case "zstd":
		enc = tileenc.Zstd
	case "identity":
		enc = tileenc.Identity
	default:
		return nil, fmt.Errorf("publish: bad encoding %q (gzip, zstd or identity)", opt.Encoding)
	}
	if opt.GzipLevel == 0 {
		opt.GzipLevel = 6
	}
	if opt.KeyframeEvery < 1 {
		opt.KeyframeEvery = 1
	}
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	if opt.MaxAttempts == 0 {
		opt.MaxAttempts = 6
	}
	if opt.Backoff <= 0 {
		opt.Backoff = 500 * time.Millisecond
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	comp, err := tileenc.NewCompressor(tileenc.CompressorOptions{GzipLevel: opt.GzipLevel})
	if err != nil {
		return nil, err
	}
	return &Publisher{src: src, dst: dst, state: state, opt: opt, enc: enc, comp: comp}, nil
}

// Close releases the compressor.
func (p *Publisher) Close() error { return p.comp.Close() }

// Round publishes once: it finishes the queue a stopped round left, then plans the
// current objects, persists them as the queue and stores every one the target does not
// hold yet, phase by phase. It returns ErrAuth (wrapped) when the target refuses the
// credentials, and an error when an object could not be stored after MaxAttempts; the
// queue is kept in both cases.
func (p *Publisher) Round(ctx context.Context) (st Stats, err error) {
	t0 := time.Now()
	defer func() { st.Duration = time.Since(t0) }()
	old, err := p.state.LoadQueue()
	if err != nil {
		return st, err
	}
	if len(old) > 0 {
		st.Resumed = len(old)
		p.opt.Logf("publish: resuming %d queued objects", len(old))
		if err := p.run(ctx, old, &st); err != nil {
			return st, err
		}
		if err := p.state.SaveQueue(nil); err != nil {
			return st, err
		}
	}
	pl := &planner{src: p.src, opt: &p.opt, stored: func(key string) bool {
		r, ok := p.state.Lookup(key)
		return ok && r.Immutable
	}}
	items, err := pl.plan(ctx)
	if err != nil {
		return st, err
	}
	st.Planned = len(items)
	// Persist what still has to be stored (immutable objects the state holds are left
	// out, which keeps the queue small on later rounds).
	queue := make([]Item, 0, len(items))
	for _, it := range items {
		if it.Immutable {
			if r, ok := p.state.Lookup(it.Key); ok && r.Immutable {
				continue
			}
		}
		queue = append(queue, it)
	}
	if err := p.state.SaveQueue(queue); err != nil {
		return st, err
	}
	if err := p.run(ctx, items, &st); err != nil {
		return st, err
	}
	if err := p.state.Sync(); err != nil {
		return st, err
	}
	return st, p.state.SaveQueue(nil)
}

// run stores items phase by phase: a phase starts only when the previous one is done.
func (p *Publisher) run(ctx context.Context, items []Item, st *Stats) error {
	var counters struct {
		uploaded, immutable, skipped, unchanged, missing, empty, rejected, failed, retries atomic.Int64
		bytes                                                                              atomic.Int64
	}
	defer func() {
		st.Uploaded += int(counters.uploaded.Load())
		st.Immutable += int(counters.immutable.Load())
		st.Skipped += int(counters.skipped.Load())
		st.Unchanged += int(counters.unchanged.Load())
		st.Missing += int(counters.missing.Load())
		st.Empty += int(counters.empty.Load())
		st.Rejected += int(counters.rejected.Load())
		st.Failed += int(counters.failed.Load())
		st.Retries += int(counters.retries.Load())
		st.Bytes += counters.bytes.Load()
	}()
	for phase := 0; phase < numPhases; phase++ {
		if phase == phaseIndexes && counters.rejected.Load()+counters.failed.Load()+counters.missing.Load() > 0 {
			// A data object of this round is not stored (refused, unreadable, or gone
			// from the source since planning): storing a final index now would name it
			// for good and keep the next rounds from planning it again.
			p.opt.Logf("publish: final timelines and day indexes deferred to the next round (objects were refused, unreadable or missing)")
			continue
		}
		var batch []*Item
		for i := range items {
			if items[i].Phase == phase {
				batch = append(batch, &items[i])
			}
		}
		if len(batch) == 0 {
			continue
		}
		pctx, cancel := context.WithCancel(ctx)
		var (
			wg       sync.WaitGroup
			next     atomic.Int64
			errOnce  sync.Once
			phaseErr error
		)
		fail := func(err error) {
			errOnce.Do(func() {
				phaseErr = err
				cancel()
			})
		}
		for w := 0; w < min(p.opt.Workers, len(batch)); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for pctx.Err() == nil {
					i := int(next.Add(1)) - 1
					if i >= len(batch) {
						return
					}
					it := batch[i]
					res, n, retries, err := p.store(pctx, it)
					counters.retries.Add(int64(retries))
					switch res {
					case resUploaded:
						counters.uploaded.Add(1)
						counters.bytes.Add(int64(n))
						if it.Immutable {
							counters.immutable.Add(1)
						}
					case resSkipped:
						counters.skipped.Add(1)
					case resUnchanged:
						counters.unchanged.Add(1)
					case resMissing:
						counters.missing.Add(1)
						p.opt.Logf("publish: %s: missing at the source, skipped", it.Key)
					case resEmpty:
						counters.empty.Add(1)
					case resRejected:
						counters.rejected.Add(1)
						p.opt.Logf("publish: %v (skipped)", err)
					case resFailed:
						counters.failed.Add(1)
						p.opt.Logf("publish: %s: %v (skipped)", it.Key, err)
					case resFatal:
						fail(err)
						return
					}
				}
			}()
		}
		wg.Wait()
		cancel()
		if err := p.state.Sync(); err != nil && phaseErr == nil {
			phaseErr = err
		}
		if phaseErr == nil && ctx.Err() != nil {
			phaseErr = ctx.Err()
		}
		if phaseErr != nil {
			return phaseErr
		}
	}
	return nil
}

type result int

const (
	resUploaded result = iota
	resSkipped
	resUnchanged
	resMissing
	resEmpty
	resRejected
	resFailed
	resFatal
)

// store reads one item and stores it unless the target already holds it.
func (p *Publisher) store(ctx context.Context, it *Item) (result, int, int, error) {
	prev, had := p.state.Lookup(it.Key)
	checker, _ := p.dst.(Checker)
	if had && checker != nil && !checker.Has(it.Key, prev.Size) {
		had = false // gone from the target: store it again
	}
	if it.Immutable && had && prev.Immutable {
		return resSkipped, 0, 0, nil
	}
	raw, ctype, err := p.read(ctx, it)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return resMissing, 0, 0, nil
		}
		if ctx.Err() != nil {
			return resFatal, 0, 0, ctx.Err()
		}
		return resFailed, 0, 0, err
	}
	if it.SkipEmpty && !had && emptyOverlay(raw) {
		return resEmpty, 0, 0, nil
	}
	body, enc, err := p.encode(raw, ctype)
	if err != nil {
		return resFailed, 0, 0, err
	}
	hash := objectHash(enc, ctype, it.Cache, body)
	if had && prev.Hash == hash && prev.Immutable == it.Immutable {
		return resUnchanged, 0, 0, nil
	}
	m := Meta{ContentType: ctype, ContentEncoding: enc, CacheControl: it.Cache}
	retries := 0
	backoff := p.opt.Backoff
	for attempt := 1; ; attempt++ {
		err = p.dst.Put(ctx, it.Key, body, m)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return resFatal, 0, retries, ctx.Err()
		}
		switch classify(err) {
		case putAuth:
			return resFatal, 0, retries, fmt.Errorf("%w: %v", ErrAuth, err)
		case putRejected:
			return resRejected, 0, retries, err
		}
		if p.opt.MaxAttempts > 0 && attempt >= p.opt.MaxAttempts {
			return resFatal, 0, retries, fmt.Errorf("storing %s failed %d times: %w", it.Key, attempt, err)
		}
		retries++
		p.opt.Logf("publish: %s: %v (retrying in %v)", it.Key, err, backoff)
		select {
		case <-ctx.Done():
			return resFatal, 0, retries, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	if err := p.state.Record(it.Key, record{Hash: hash, Size: int64(len(body)), Immutable: it.Immutable}); err != nil {
		return resFatal, 0, retries, err
	}
	return resUploaded, len(body), retries, nil
}

// read returns an item's body without Content-Encoding, and its Content-Type.
func (p *Publisher) read(ctx context.Context, it *Item) ([]byte, string, error) {
	if it.body != nil {
		b, ct := it.body, it.ctype
		it.body = nil // used once; a resumed or later round reads it again
		return b, ct, nil
	}
	switch it.Src {
	case srcCatalog:
		r, err := p.src.Get(ctx, "catalog")
		if err != nil {
			return nil, "", err
		}
		b, err := staticCatalog(r.Body, p.opt.LiveMs)
		return b, "application/json", err
	case srcWeb:
		if p.opt.Web == nil {
			return nil, "", fmt.Errorf("get %s: %w", it.Path, ErrNotFound)
		}
		b, err := fs.ReadFile(p.opt.Web, it.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("get %s: %w", it.Path, ErrNotFound)
		}
		if err != nil {
			return nil, "", err
		}
		if it.Path == "index.html" {
			b = staticIndex(b)
		}
		return b, webContentType(it.Path), nil
	}
	r, err := p.src.Get(ctx, it.Path)
	if err != nil {
		return nil, "", err
	}
	return r.Body, r.ContentType, nil
}

// encode compresses a body with the configured encoding, unless it is an image or
// small.
func (p *Publisher) encode(raw []byte, ctype string) ([]byte, string, error) {
	if p.enc == tileenc.Identity || len(raw) < 256 || strings.HasPrefix(ctype, "image/") && !strings.Contains(ctype, "svg") || strings.Contains(ctype, "woff") {
		return raw, "", nil
	}
	b, err := p.comp.Compress(p.enc, raw)
	if err != nil {
		return nil, "", err
	}
	return b, p.enc.Header(), nil
}

// objectHash identifies the stored bytes and headers.
func objectHash(enc, ctype, cache string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(enc + "\x00" + ctype + "\x00" + cache + "\x00"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// emptyOverlay reports whether an overlay response has no intervals.
func emptyOverlay(raw []byte) bool {
	var r struct {
		Intervals []json.RawMessage `json:"intervals"`
	}
	return json.Unmarshal(raw, &r) == nil && len(r.Intervals) == 0
}
