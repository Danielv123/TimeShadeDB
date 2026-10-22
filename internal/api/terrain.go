package api

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"timeshadedb/internal/core"
	"timeshadedb/internal/tileenc"
)

const octet = "application/octet-stream"

// MaxViewTiles bounds the tile rectangle of one view request (API.md 2).
const MaxViewTiles = 4096

// dsInfoTTL bounds how stale the cached datastore list may be. A stale horizon only
// makes fewer answers immutable, and a stale LastT only matters when it is ahead of the
// clock, so a short TTL is safe; commits and new datastores invalidate it.
const dsInfoTTL = time.Second

// datastore returns the datastore's info, from a short-lived cache of
// TerrainStore.Datastores. Unknown datastores return ErrUnknownDatastore without
// touching the store.
func (s *Server) datastore(ctx context.Context, key core.DatastoreKey) (core.DatastoreInfo, error) {
	infos, err := s.datastores(ctx)
	if err != nil {
		return core.DatastoreInfo{}, err
	}
	info, ok := infos[key]
	if !ok {
		return core.DatastoreInfo{}, notFound("unknown datastore %s", key.ID())
	}
	return info, nil
}

func (s *Server) datastores(ctx context.Context) (map[core.DatastoreKey]core.DatastoreInfo, error) {
	s.dsMu.Lock()
	defer s.dsMu.Unlock()
	if s.dsInfos != nil && time.Since(s.dsAt) < dsInfoTTL {
		return s.dsInfos, nil
	}
	list, err := s.cfg.Terrain.Datastores(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[core.DatastoreKey]core.DatastoreInfo, len(list))
	for _, info := range list {
		m[info.Key] = info
	}
	s.dsInfos, s.dsAt = m, time.Now()
	return m, nil
}

func (s *Server) invalidateDatastores() {
	s.dsMu.Lock()
	s.dsInfos = nil
	s.dsMu.Unlock()
}

// finalBound returns the time below which answers at level z never change: the horizon
// at z0, min(horizon, BuiltUpTo) at z < 0 (API.md "view").
func (s *Server) finalBound(info core.DatastoreInfo, z int8) core.TimeMs {
	if z == 0 {
		return info.Horizon
	}
	b, ok := s.cfg.Tiles.BuiltUpTo(info.Key)
	if !ok {
		return math.MinInt64
	}
	return min(info.Horizon, b)
}

// GET palette/{ds}/{ver}.bin
func (s *Server) servePalette(ctx context.Context, w http.ResponseWriter, r *http.Request, tail string) (int, error) {
	parts, ok := splitN(tail, 2)
	if !ok {
		return 0, notFound("no route palette/%s", truncate(tail))
	}
	key, err := parseDS(parts[0])
	if err != nil {
		return 0, err
	}
	vs, ok := strings.CutSuffix(parts[1], ".bin")
	if !ok {
		return 0, notFound("no route palette/%s", truncate(tail))
	}
	v, err := parseVersion(vs)
	if err != nil {
		return 0, err
	}
	if _, err := s.datastore(ctx, key); err != nil {
		return 0, err
	}
	if v == 0 {
		return 0, notFound("palette version 0")
	}
	b, err := s.cfg.Renderer.Palette(ctx, key, uint32(v), tileenc.Negotiate(r.Header.Get("Accept-Encoding")))
	if err != nil {
		return 0, err
	}
	return s.writeBody(w, octet, cacheImmutable, b), nil
}

// GET view/{ds}/{z}?bbox=x0,y0,x1,y1&t=
func (s *Server) serveView(ctx context.Context, w http.ResponseWriter, r *http.Request, tail string) (int, error) {
	parts, ok := splitN(tail, 2)
	if !ok {
		return 0, notFound("no route view/%s", truncate(tail))
	}
	key, err := parseDS(parts[0])
	if err != nil {
		return 0, err
	}
	z, err := parseZ(parts[1])
	if err != nil {
		return 0, err
	}
	q := r.URL.Query()
	bbox, err := parseBBox(q.Get("bbox"))
	if err != nil {
		return 0, err
	}
	info, err := s.datastore(ctx, key)
	if err != nil {
		return 0, err
	}
	t, err := s.resolveT(q.Get("t"), info)
	if err != nil {
		return 0, err
	}
	resp := core.ViewResponse{DS: info.ID, Z: z, T: t, Horizon: info.Horizon, Tiles: []core.ViewTile{}}
	tiles, err := s.cfg.Tiles.ListTiles(ctx, key, z)
	if err != nil {
		return 0, err
	}
	for _, ti := range tiles {
		c := ti.Tile
		if c.X < bbox[0] || c.X > bbox[2] || c.Y < bbox[1] || c.Y > bbox[3] {
			continue
		}
		var st core.TileState
		switch {
		case !ti.Live && ti.Version > 0 && t >= ti.LastT && t >= info.Horizon:
			// Fast path: at or after the latest version, nothing open.
			st = core.TileState{Version: ti.Version}
		case !ti.Live && ti.Version > 0 && t < ti.FirstT:
			continue
		default:
			st, err = s.cfg.Tiles.VersionAt(ctx, key, c, t)
			if err != nil {
				return 0, err
			}
		}
		if st.Version == 0 && !st.Live {
			continue
		}
		resp.Tiles = append(resp.Tiles, core.ViewTile{X: c.X, Y: c.Y, TileState: st})
	}
	c := cacheLive
	if t < s.finalBound(info, z) {
		c = cacheImmutable
	}
	return s.writeJSON(w, r, c, resp), nil
}

func parseBBox(s string) ([4]int32, error) {
	var b [4]int32
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return b, badRequest("bbox needs x0,y0,x1,y1")
	}
	for i, p := range parts {
		v, err := parseCoord(p)
		if err != nil {
			return b, badRequest("bad bbox %q", truncate(s))
		}
		b[i] = v
	}
	if b[0] > b[2] || b[1] > b[3] {
		return b, badRequest("empty bbox %q", truncate(s))
	}
	if (int64(b[2])-int64(b[0])+1)*(int64(b[3])-int64(b[1])+1) > MaxViewTiles {
		return b, badRequest("bbox covers more than %d tiles", MaxViewTiles)
	}
	return b, nil
}

// GET timeline/{ds}/{z}/{date}.json
func (s *Server) serveTimeline(ctx context.Context, w http.ResponseWriter, r *http.Request, tail string) (int, error) {
	parts, ok := splitN(tail, 3)
	if !ok {
		return 0, notFound("no route timeline/%s", truncate(tail))
	}
	key, err := parseDS(parts[0])
	if err != nil {
		return 0, err
	}
	z, err := parseZ(parts[1])
	if err != nil {
		return 0, err
	}
	date, ok := strings.CutSuffix(parts[2], ".json")
	if !ok {
		return 0, notFound("no route timeline/%s", truncate(tail))
	}
	start, err := core.ParseUTCDay(date)
	if err != nil || core.UTCDay(start) != date {
		return 0, badRequest("bad date %q", truncate(date))
	}
	info, err := s.datastore(ctx, key)
	if err != nil {
		return 0, err
	}
	end := start + core.Day
	final := s.finalBound(info, z) >= end
	build := func(ctx context.Context) ([]byte, error) {
		resp, err := s.timeline(ctx, info, z, date, start, final)
		if err != nil {
			return nil, err
		}
		return jsonMarshal(resp)
	}
	enc := tileenc.Negotiate(r.Header.Get("Accept-Encoding"))
	if final {
		b, err := s.cfg.Renderer.Cached(ctx, "timeline/"+info.ID+"/"+parts[1]+"/"+date, enc, build)
		if err != nil {
			return 0, err
		}
		return s.writeBody(w, "application/json", cacheImmutable, b), nil
	}
	raw, err := build(ctx)
	if err != nil {
		return 0, err
	}
	return s.writeRawJSON(w, r, cacheShort, raw, ""), nil
}

func (s *Server) timeline(ctx context.Context, info core.DatastoreInfo, z int8, date string, start core.TimeMs, final bool) (core.TimelineResponse, error) {
	end := start + core.Day
	resp := core.TimelineResponse{DS: info.ID, Z: z, Date: date, Start: start, Final: final, Tiles: []core.TimelineTile{}}
	tiles, err := s.cfg.Tiles.ListTiles(ctx, info.Key, z)
	if err != nil {
		return resp, err
	}
	for _, ti := range tiles {
		if ti.Version == 0 || ti.FirstT >= end {
			continue // nothing committed by the end of the day
		}
		tt := core.TimelineTile{X: ti.Tile.X, Y: ti.Tile.Y, Points: [][2]int64{}}
		if ti.LastT <= start {
			tt.V0 = ti.Version
		} else {
			st, err := s.cfg.Tiles.VersionAt(ctx, info.Key, ti.Tile, start)
			if err != nil {
				return resp, err
			}
			tt.V0 = st.Version
			pts, err := s.cfg.Tiles.History(ctx, info.Key, ti.Tile, start, end)
			if err != nil {
				return resp, err
			}
			for _, p := range pts {
				tt.Points = append(tt.Points, [2]int64{int64(p.T), int64(p.Version)})
			}
		}
		if tt.V0 == 0 && len(tt.Points) == 0 {
			continue
		}
		resp.Tiles = append(resp.Tiles, tt)
	}
	return resp, nil
}

// GET tile/{ds}/{z}/{x}/{y}/{ver}.bin | live.bin | {ver}.png
func (s *Server) serveTile(ctx context.Context, w http.ResponseWriter, r *http.Request, tail string) (int, error) {
	parts, ok := splitN(tail, 5)
	if !ok {
		return 0, notFound("no route tile/%s", truncate(tail))
	}
	key, err := parseDS(parts[0])
	if err != nil {
		return 0, err
	}
	z, err := parseZ(parts[1])
	if err != nil {
		return 0, err
	}
	x, err := parseCoord(parts[2])
	if err != nil {
		return 0, err
	}
	y, err := parseCoord(parts[3])
	if err != nil {
		return 0, err
	}
	tile := core.TileCoord{Z: z, X: x, Y: y}
	if !tile.Valid() {
		return 0, &apiError{http.StatusBadRequest, "bad_tile", "tile outside the grid"}
	}
	file := parts[4]
	png := false
	vs, ok := strings.CutSuffix(file, ".bin")
	if !ok {
		if vs, ok = strings.CutSuffix(file, ".png"); !ok {
			return 0, notFound("no route tile/%s", truncate(tail))
		}
		png = true
	}
	info, err := s.datastore(ctx, key)
	if err != nil {
		return 0, err
	}
	enc := tileenc.Negotiate(r.Header.Get("Accept-Encoding"))
	if vs == "live" && !png {
		t := max(s.cfg.Clock(), info.LastT)
		b, _, err := s.cfg.Renderer.LiveTile(ctx, key, tile, t, enc)
		if err != nil {
			return 0, err
		}
		return s.writeBody(w, octet, cacheNone, b), nil
	}
	v, err := parseVersion(vs)
	if err != nil {
		return 0, err
	}
	if png {
		b, err := s.cfg.Renderer.PNG(ctx, key, tile, v)
		if err != nil {
			return 0, err
		}
		return s.writeBody(w, "image/png", cacheImmutable, b), nil
	}
	b, err := s.cfg.Renderer.Tile(ctx, key, tile, v, enc)
	if err != nil {
		return 0, err
	}
	return s.writeBody(w, octet, cacheImmutable, b), nil
}

// GET tiledelta/{ds}/{x}/{y}/{from}-{to}.bin
func (s *Server) serveTileDelta(ctx context.Context, w http.ResponseWriter, r *http.Request, tail string) (int, error) {
	parts, ok := splitN(tail, 4)
	if !ok {
		return 0, notFound("no route tiledelta/%s", truncate(tail))
	}
	key, err := parseDS(parts[0])
	if err != nil {
		return 0, err
	}
	x, err := parseCoord(parts[1])
	if err != nil {
		return 0, err
	}
	y, err := parseCoord(parts[2])
	if err != nil {
		return 0, err
	}
	tile := core.TileCoord{X: x, Y: y}
	if !tile.Valid() {
		return 0, &apiError{http.StatusBadRequest, "bad_tile", "tile outside the grid"}
	}
	span, ok := strings.CutSuffix(parts[3], ".bin")
	if !ok {
		return 0, notFound("no route tiledelta/%s", truncate(tail))
	}
	fs, ts, ok := strings.Cut(span, "-")
	if !ok {
		return 0, badRequest("bad delta span %q", truncate(span))
	}
	from, err := parseVersion(fs)
	if err != nil {
		return 0, err
	}
	to, err := parseVersion(ts)
	if err != nil {
		return 0, err
	}
	if from >= to {
		return 0, badRequest("delta needs from < to")
	}
	if _, err := s.datastore(ctx, key); err != nil {
		return 0, err
	}
	b, err := s.cfg.Renderer.Delta(ctx, key, tile, from, to, tileenc.Negotiate(r.Header.Get("Accept-Encoding")))
	if err != nil {
		return 0, err
	}
	return s.writeBody(w, octet, cacheImmutable, b), nil
}

// levels returns the levels of a datastore that have tiles, from 0 down.
func (s *Server) levels(ctx context.Context, info core.DatastoreInfo) []int8 {
	out := []int8{}
	if info.Tiles == 0 {
		return out
	}
	out = append(out, 0)
	for z := int8(-1); z >= core.MinZ; z-- {
		tiles, err := s.cfg.Tiles.ListTiles(ctx, info.Key, z)
		if err != nil || len(tiles) == 0 {
			break
		}
		out = append(out, z)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
