package benchlib

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"timeshadedb/internal/v3/core"
)

// ErrUnsupported is returned by a target for a workload its API cannot take (for
// example mover samples on v2, which has no mover store).
var ErrUnsupported = errors.New("not supported by this target")

// ChunkRow is one chunk state update, as a capture would send it.
type ChunkRow struct {
	// T is Unix milliseconds on the map clock (the test data's clock). v2 stores it as
	// its "tick" (to_stream.py --tick ms).
	T                   int64
	Map, Surface, Force string
	CX, CY              int32
	// Pixels is the chunk chart. Nil with Deleted.
	Pixels  *core.ChunkPixels
	Deleted bool
}

// MoverRow is one mover sample or gone.
type MoverRow struct {
	T            int64
	Map, Surface string
	Entry        core.MoveEntry
}

// Body is one prepared ingest request.
type Body struct {
	Path        string
	ContentType string
	Data        []byte
	Rows        int
}

// DSInfo is one terrain datastore as a target's catalog describes it.
type DSInfo struct {
	// ID is the v3 datastore id, or "uuid|force|surface" for v2.
	ID                  string
	Map, Surface, Force string
	First, Last         int64
	// Bounds is the z0 tile box [minX, minY, maxX, maxY], inclusive, in the target's own
	// grid (v2 has no -256 origin).
	Bounds [4]int32
	Chunks int
}

// TileRef is one tile request a view needs.
type TileRef struct {
	Z, X, Y int32
	URL     string
}

// Target is an HTTP API under test.
type Target interface {
	Kind() string
	// EncodeChunks builds the request bodies for rows (rows may mix maps).
	EncodeChunks(rows []ChunkRow) ([]Body, error)
	// EncodeMovers builds the request bodies for mover rows.
	EncodeMovers(rows []MoverRow) ([]Body, error)
	// Datastores lists the terrain datastores.
	Datastores(ctx context.Context) ([]DSInfo, error)
	// Tiles lists the tile requests that cover ds's bounds at level z at time t (nil:
	// latest). For v3 this calls the view route; the returned duration is its latency
	// (0 for v2).
	Tiles(ctx context.Context, ds DSInfo, z int, t *int64) ([]TileRef, time.Duration, error)
	// HostileURL returns a random request that must be rejected cheaply: random levels
	// and coordinates, unknown datastores, missing versions.
	HostileURL(rng *rand.Rand, known []DSInfo) string
}

// Client wraps an HTTP client and a base URL.
type Client struct {
	Base string
	HTTP *http.Client
	// Token is sent as a bearer token on v3 ingest (and reads when set).
	Token string
}

// NewClient returns a client with keep-alive connections sized for conc parallel
// requests.
func NewClient(base string, conc int) *Client {
	if conc < 2 {
		conc = 2
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        conc * 2,
		MaxIdleConnsPerHost: conc * 2,
		MaxConnsPerHost:     conc * 2,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Transport: tr, Timeout: 10 * time.Minute}}
}

// Response is the outcome of one request.
type Response struct {
	Status int
	Header http.Header
	Bytes  int64
	Dur    time.Duration
	Body   []byte
}

// Get fetches base+path. keep controls whether the body is kept (otherwise only
// counted).
func (c *Client) Get(ctx context.Context, path string, keep bool, hdr map[string]string) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return Response{}, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return c.do(req, keep)
}

// Post sends a prepared body.
func (c *Client) Post(ctx context.Context, b Body) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+b.Path, bytes.NewReader(b.Data))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", b.ContentType)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return c.do(req, true)
}

func (c *Client) do(req *http.Request, keep bool) (Response, error) {
	start := Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	var r Response
	r.Status = resp.StatusCode
	r.Header = resp.Header
	if keep {
		r.Body, err = io.ReadAll(resp.Body)
		r.Bytes = int64(len(r.Body))
	} else {
		r.Bytes, err = io.Copy(io.Discard, resp.Body)
	}
	r.Dur = Since(start)
	return r, err
}

// NewTarget returns the target for kind "v2" or "v3".
func NewTarget(kind string, c *Client) (Target, error) {
	switch kind {
	case "v2":
		return &V2{C: c}, nil
	case "v3":
		return &V3{C: c, Src: "standalone:bench"}, nil
	}
	return nil, fmt.Errorf("unknown target %q (want v2 or v3)", kind)
}

// ---------------------------------------------------------------- v2 (TimeShadeDB master)

// V2 talks to the legacy chunk API of TimeShadeDB master (cmd/timeshadedb/serve.go):
// POST /api/ingest/chunk/{uuid} with TSV rows, GET /api/chunk/saves and
// GET /api/chunk/tiles/{uuid}/{force}/{surface}/{z}/{x}/{y}.png?tick=. The map key is
// used as the savefile UUID, as research/scripts/to_stream.py does.
type V2 struct{ C *Client }

func (v *V2) Kind() string { return "v2" }

// V2Line formats a chunk row as the 5-field TSV row chunk.go parses.
func V2Line(r ChunkRow) string {
	var hx string
	if r.Pixels != nil {
		var raw [2 * core.ChunkPixelCount]byte
		for i, p := range r.Pixels {
			raw[2*i] = byte(p)
			raw[2*i+1] = byte(p >> 8)
		}
		hx = hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%d\t%s\t%d,%d\t%s\t%s", r.T, r.Surface, r.CX, r.CY, r.Force, hx)
}

func (v *V2) EncodeChunks(rows []ChunkRow) ([]Body, error) {
	byMap := map[string]*bytes.Buffer{}
	counts := map[string]int{}
	var order []string
	for _, r := range rows {
		if r.Deleted {
			continue // v2 has no deletion
		}
		b := byMap[r.Map]
		if b == nil {
			b = &bytes.Buffer{}
			byMap[r.Map] = b
			order = append(order, r.Map)
		}
		b.WriteString(V2Line(r))
		b.WriteByte('\n')
		counts[r.Map]++
	}
	out := make([]Body, 0, len(order))
	for _, m := range order {
		out = append(out, Body{
			Path:        "/api/ingest/chunk/" + url.PathEscape(m),
			ContentType: "text/tab-separated-values",
			Data:        byMap[m].Bytes(),
			Rows:        counts[m],
		})
	}
	return out, nil
}

// EncodeV2Raw wraps already formatted TSV lines (all of one map) in a body.
func EncodeV2Raw(mapKey string, lines []string) Body {
	return Body{
		Path:        "/api/ingest/chunk/" + url.PathEscape(mapKey),
		ContentType: "text/tab-separated-values",
		Data:        []byte(strings.Join(lines, "\n") + "\n"),
		Rows:        len(lines),
	}
}

func (v *V2) EncodeMovers([]MoverRow) ([]Body, error) { return nil, ErrUnsupported }

type v2Catalog struct {
	Saves []struct {
		SavefileUUID string `json:"savefile_uuid"`
		Datastores   []struct {
			Surface    string `json:"surface"`
			Force      string `json:"force"`
			LatestTick int64  `json:"latest_tick"`
			ChunkCount int    `json:"chunk_count"`
			MinTileX   int32  `json:"min_tile_x"`
			MaxTileX   int32  `json:"max_tile_x"`
			MinTileY   int32  `json:"min_tile_y"`
			MaxTileY   int32  `json:"max_tile_y"`
		} `json:"datastores"`
	} `json:"saves"`
}

func (v *V2) Datastores(ctx context.Context) ([]DSInfo, error) {
	r, err := v.C.Get(ctx, "/api/chunk/saves", true, nil)
	if err != nil {
		return nil, err
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("saves: HTTP %d", r.Status)
	}
	var cat v2Catalog
	if err := json.Unmarshal(r.Body, &cat); err != nil {
		return nil, err
	}
	var out []DSInfo
	for _, s := range cat.Saves {
		for _, d := range s.Datastores {
			out = append(out, DSInfo{
				ID:  s.SavefileUUID + "|" + d.Force + "|" + d.Surface,
				Map: s.SavefileUUID, Surface: d.Surface, Force: d.Force,
				Last:   d.LatestTick,
				Bounds: [4]int32{d.MinTileX, d.MinTileY, d.MaxTileX, d.MaxTileY},
				Chunks: d.ChunkCount,
			})
		}
	}
	sortDS(out)
	return out, nil
}

func (v *V2) Tiles(ctx context.Context, ds DSInfo, z int, t *int64) ([]TileRef, time.Duration, error) {
	tick := ds.Last
	if t != nil {
		tick = *t
	}
	var out []TileRef
	for _, tc := range tilesAtLevel(ds.Bounds, z) {
		out = append(out, TileRef{Z: int32(z), X: tc[0], Y: tc[1], URL: fmt.Sprintf("/api/chunk/tiles/%s/%s/%s/%d/%d/%d.png?tick=%d",
			url.PathEscape(ds.Map), url.PathEscape(ds.Force), url.PathEscape(ds.Surface), z, tc[0], tc[1], tick)})
	}
	return out, 0, nil
}

func (v *V2) HostileURL(rng *rand.Rand, known []DSInfo) string {
	z := -rng.Intn(21)
	x, y := rng.Int31()-1<<30, rng.Int31()-1<<30
	if rng.Intn(4) == 0 || len(known) == 0 {
		return fmt.Sprintf("/api/chunk/tiles/nosuchsave-%d/player/nauvis/%d/%d/%d.png?tick=%d", rng.Intn(1<<20), z, x, y, rng.Intn(1<<30))
	}
	ds := known[rng.Intn(len(known))]
	return fmt.Sprintf("/api/chunk/tiles/%s/%s/%s/%d/%d/%d.png?tick=%d", url.PathEscape(ds.Map), url.PathEscape(ds.Force),
		url.PathEscape(ds.Surface), z, x, y, rng.Intn(1<<30))
}

// ---------------------------------------------------------------- v3

// V3 talks to the v3 API (docs/v3/API.md) and posts capture envelopes
// (docs/v3/PROTOCOL.md).
type V3 struct {
	C   *Client
	Src string
	// T0 anchors the synthetic game tick: tick = (T - T0) * 60 / 1000. Set from the first
	// row encoded.
	T0    int64
	t0Set bool
	seq   int64
}

func (v *V3) Kind() string { return "v3" }

// tickOf converts map-clock ms to a 60 UPS tick relative to T0, so the server's clock
// anchor (PROTOCOL.md 4) maps it back to T within rounding (at most 8 ms).
func (v *V3) tickOf(t int64) int64 {
	if !v.t0Set {
		v.T0, v.t0Set = t, true
	}
	d := t - v.T0
	if d < 0 {
		return 0
	}
	return (d*60 + 500) / 1000
}

func (v *V3) envelope(mapKey string, lastT int64, msgs []json.RawMessage) ([]byte, error) {
	v.seq++
	seq := v.seq
	env := core.Envelope{
		V: 1, Tick: v.tickOf(lastT), Msgs: msgs,
		Src: v.Src + ":" + mapKey, Map: mapKey, RecvMs: core.TimeMs(lastT), Seq: &seq,
	}
	return json.Marshal(env)
}

func (v *V3) EncodeChunks(rows []ChunkRow) ([]Body, error) {
	type group struct {
		msgs  []json.RawMessage
		lastT int64
		n     int
	}
	groups := map[string]*group{}
	var order []string
	for _, r := range rows {
		g := groups[r.Map]
		if g == nil {
			g = &group{}
			groups[r.Map] = g
			order = append(order, r.Map)
		}
		tick := v.tickOf(r.T)
		m := core.ChunkMsg{Type: "chunk", S: r.Surface, F: r.Force, X: r.CX, Y: r.CY, Tick: &tick}
		if !r.Deleted && r.Pixels != nil {
			d := core.EncodeChart(r.Pixels)
			m.D = &d
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		g.msgs = append(g.msgs, b)
		if r.T > g.lastT {
			g.lastT = r.T
		}
		g.n++
	}
	return v.bodies(order, func(m string) ([]json.RawMessage, int64, int) {
		g := groups[m]
		return g.msgs, g.lastT, g.n
	})
}

func (v *V3) bodies(order []string, get func(string) ([]json.RawMessage, int64, int)) ([]Body, error) {
	var buf bytes.Buffer
	rows := 0
	for _, m := range order {
		msgs, lastT, n := get(m)
		line, err := v.envelope(m, lastT, msgs)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
		rows += n
	}
	if rows == 0 {
		return nil, nil
	}
	return []Body{{Path: "/api/v3/ingest", ContentType: "application/x-ndjson", Data: buf.Bytes(), Rows: rows}}, nil
}

func (v *V3) EncodeMovers(rows []MoverRow) ([]Body, error) {
	type key struct{ m, s string }
	type group struct {
		entries []core.MoveEntry
		lastT   int64
	}
	groups := map[key]*group{}
	var order []key
	for _, r := range rows {
		k := key{r.Map, r.Surface}
		g := groups[k]
		if g == nil {
			g = &group{}
			groups[k] = g
			order = append(order, k)
		}
		e := r.Entry
		tick := v.tickOf(r.T)
		e.Tick = &tick
		g.entries = append(g.entries, e)
		if r.T > g.lastT {
			g.lastT = r.T
		}
	}
	// One envelope per map, one move message per surface.
	byMap := map[string][]json.RawMessage{}
	lastT := map[string]int64{}
	counts := map[string]int{}
	var maps []string
	for _, k := range order {
		g := groups[k]
		b, err := json.Marshal(core.MoveMsg{Type: "move", S: k.s, M: g.entries})
		if err != nil {
			return nil, err
		}
		if _, ok := byMap[k.m]; !ok {
			maps = append(maps, k.m)
		}
		byMap[k.m] = append(byMap[k.m], b)
		lastT[k.m] = max(lastT[k.m], g.lastT)
		counts[k.m] += len(g.entries)
	}
	return v.bodies(maps, func(m string) ([]json.RawMessage, int64, int) { return byMap[m], lastT[m], counts[m] })
}

func (v *V3) Datastores(ctx context.Context) ([]DSInfo, error) {
	r, err := v.C.Get(ctx, "/api/v3/catalog", true, v.readHeader())
	if err != nil {
		return nil, err
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("catalog: HTTP %d", r.Status)
	}
	var cat core.Catalog
	if err := json.Unmarshal(r.Body, &cat); err != nil {
		return nil, err
	}
	var out []DSInfo
	for _, m := range cat.Maps {
		for _, s := range m.Surfaces {
			for _, d := range s.Datastores {
				out = append(out, DSInfo{
					ID: d.ID, Map: m.Key, Surface: s.Name, Force: d.Force,
					First: int64(d.First), Last: int64(d.Last), Bounds: d.Bounds,
				})
			}
		}
	}
	sortDS(out)
	return out, nil
}

func (v *V3) readHeader() map[string]string {
	if v.C.Token == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + v.C.Token}
}

// MaxViewTiles is the view route's per-request limit (API.md 2).
const MaxViewTiles = 4096

func (v *V3) Tiles(ctx context.Context, ds DSInfo, z int, t *int64) ([]TileRef, time.Duration, error) {
	tq := "latest"
	if t != nil {
		tq = strconv.FormatInt(*t, 10)
	}
	b := levelBounds(ds.Bounds, z)
	var out []TileRef
	var total time.Duration
	// Split the box into row bands of at most MaxViewTiles tiles.
	w := int64(b[2]-b[0]) + 1
	rowsPer := max(int64(1), MaxViewTiles/w)
	for y0 := int64(b[1]); y0 <= int64(b[3]); y0 += rowsPer {
		y1 := min(int64(b[3]), y0+rowsPer-1)
		path := fmt.Sprintf("/api/v3/view/%s/%d?bbox=%d,%d,%d,%d&t=%s", ds.ID, z, b[0], y0, b[2], y1, tq)
		r, err := v.C.Get(ctx, path, true, v.readHeader())
		if err != nil {
			return nil, total, err
		}
		total += r.Dur
		if r.Status != 200 {
			return nil, total, fmt.Errorf("view %s: HTTP %d: %s", path, r.Status, truncate(r.Body, 200))
		}
		var vr core.ViewResponse
		if err := json.Unmarshal(r.Body, &vr); err != nil {
			return nil, total, err
		}
		for _, tile := range vr.Tiles {
			ref := TileRef{Z: int32(z), X: tile.X, Y: tile.Y}
			if tile.Live && z == 0 {
				ref.URL = fmt.Sprintf("/api/v3/tile/%s/0/%d/%d/live.bin", ds.ID, tile.X, tile.Y)
			} else {
				ref.URL = fmt.Sprintf("/api/v3/tile/%s/%d/%d/%d/%d.bin", ds.ID, z, tile.X, tile.Y, tile.Version)
			}
			out = append(out, ref)
		}
	}
	return out, total, nil
}

func (v *V3) HostileURL(rng *rand.Rand, known []DSInfo) string {
	z := -rng.Intn(21)
	x, y := rng.Int31()-1<<30, rng.Int31()-1<<30
	if rng.Intn(2) == 0 {
		// In-grid coordinates without data are the expensive case to reject.
		lim := int32(2048 >> min(-z, 11))
		x, y = rng.Int31n(2*lim+1)-lim, rng.Int31n(2*lim+1)-lim
		z = max(z, -10)
	}
	switch {
	case rng.Intn(4) == 0 || len(known) == 0:
		return fmt.Sprintf("/api/v3/tile/nosuch-%d.nauvis.player/%d/%d/%d/%d.bin", rng.Intn(1<<20), z, x, y, rng.Intn(1<<20))
	case rng.Intn(3) == 0:
		ds := known[rng.Intn(len(known))]
		return fmt.Sprintf("/api/v3/tiledelta/%s/%d/%d/%d-%d.bin", ds.ID, x, y, rng.Intn(1<<16), 1<<16+rng.Intn(1<<16))
	default:
		ds := known[rng.Intn(len(known))]
		return fmt.Sprintf("/api/v3/tile/%s/%d/%d/%d/%d.bin", ds.ID, z, x, y, rng.Intn(1<<20))
	}
}

// ---------------------------------------------------------------- helpers

func sortDS(ds []DSInfo) {
	sort.Slice(ds, func(i, j int) bool { return ds[i].ID < ds[j].ID })
}

func floorDiv(a, b int32) int32 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// levelBounds maps an inclusive z0 tile box to level z (z <= 0).
func levelBounds(b [4]int32, z int) [4]int32 {
	if z >= 0 {
		return b
	}
	s := int32(1) << uint(-z)
	return [4]int32{floorDiv(b[0], s), floorDiv(b[1], s), floorDiv(b[2], s), floorDiv(b[3], s)}
}

func tilesAtLevel(b [4]int32, z int) [][2]int32 {
	lb := levelBounds(b, z)
	var out [][2]int32
	for y := lb[1]; y <= lb[3]; y++ {
		for x := lb[0]; x <= lb[2]; x++ {
			out = append(out, [2]int32{x, y})
		}
	}
	return out
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// FindDS returns the datastore matching map, surface and force (empty strings match
// anything), preferring the one with the widest bounds.
func FindDS(all []DSInfo, mapKey, surface, force string) (DSInfo, bool) {
	var best DSInfo
	found := false
	area := func(d DSInfo) int64 {
		return int64(d.Bounds[2]-d.Bounds[0]+1) * int64(d.Bounds[3]-d.Bounds[1]+1)
	}
	for _, d := range all {
		if (mapKey == "" || d.Map == mapKey) && (surface == "" || d.Surface == surface) && (force == "" || d.Force == force) {
			if !found || area(d) > area(best) {
				best, found = d, true
			}
		}
	}
	return best, found
}
