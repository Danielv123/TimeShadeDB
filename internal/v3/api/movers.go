package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/core/wire"
	"timeshadedb/internal/v3/tileenc"
)

var (
	mvwDecOnce sync.Once
	mvwDec     *zstd.Decoder
)

// mvwDecoder decodes stored .mvw frames for clients that do not accept zstd.
func mvwDecoder() *zstd.Decoder {
	mvwDecOnce.Do(func() {
		mvwDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(256<<20))
	})
	return mvwDec
}

// GET movers/{sfc}/day/{date}.json | window/{start}.bin | keyframe/{t}.bin | live.bin
func (s *Server) serveMovers(ctx context.Context, w http.ResponseWriter, r *http.Request, tail string) (int, error) {
	id, rest, ok := strings.Cut(tail, "/")
	if !ok || id == "" {
		return 0, notFound("no route movers/%s", truncate(tail))
	}
	sfc, err := parseSurface(id)
	if err != nil {
		return 0, err
	}
	enc := tileenc.Negotiate(r.Header.Get("Accept-Encoding"))
	if rest == "live.bin" {
		mw, err := s.cfg.Movers.Live(ctx, sfc, s.cfg.Clock())
		if err != nil {
			return 0, err
		}
		raw, err := wire.EncodeMoverWindow(mw)
		if err != nil {
			return 0, err
		}
		b, err := s.cfg.Renderer.Compress(enc, raw)
		if err != nil {
			return 0, err
		}
		return s.writeBody(w, octet, cacheLive, b), nil
	}
	kind, file, ok := strings.Cut(rest, "/")
	if !ok || file == "" || strings.Contains(file, "/") {
		return 0, notFound("no route movers/%s", truncate(tail))
	}
	switch kind {
	case "day":
		date, ok := strings.CutSuffix(file, ".json")
		if !ok {
			return 0, notFound("no route movers/%s", truncate(tail))
		}
		if d, err := core.ParseUTCDay(date); err != nil || core.UTCDay(d) != date {
			return 0, badRequest("bad date %q", truncate(date))
		}
		day, err := s.cfg.Movers.Day(ctx, sfc, date)
		if err != nil {
			return 0, err
		}
		if day.Windows == nil {
			day.Windows = []core.WindowRef{}
		}
		if day.Keyframes == nil {
			day.Keyframes = []core.TimeMs{}
		}
		c := cacheShort
		if day.Final {
			c = cacheImmutable
		}
		return s.writeJSON(w, r, c, day), nil
	case "window", "keyframe":
		ts, ok := strings.CutSuffix(file, ".bin")
		if !ok {
			return 0, notFound("no route movers/%s", truncate(tail))
		}
		t, err := parseTime(ts)
		if err != nil {
			return 0, err
		}
		return s.serveMoverFile(ctx, w, sfc, kind, t, enc)
	}
	return 0, notFound("no route movers/%s", truncate(tail))
}

// serveMoverFile serves a closed window or a keyframe. Stores that keep them as zstd
// frames send them unchanged to zstd clients; other clients get the decoded body
// recompressed through the encoded-response cache.
func (s *Server) serveMoverFile(ctx context.Context, w http.ResponseWriter, sfc core.SurfaceKey, kind string, t core.TimeMs, enc tileenc.Encoding) (int, error) {
	if s.files != nil {
		var frame []byte
		var err error
		if kind == "window" {
			frame, err = s.files.WindowFile(ctx, sfc, t)
		} else {
			frame, err = s.files.KeyframeFile(ctx, sfc, t)
		}
		if err != nil {
			return 0, err
		}
		if enc == tileenc.Zstd {
			return s.writeBody(w, octet, cacheImmutable, tileenc.Body{Data: frame, Encoding: tileenc.Zstd}), nil
		}
		b, err := s.cfg.Renderer.Cached(ctx, "movers/"+sfc.ID()+"/"+kind+"/"+strconv.FormatInt(int64(t), 10), enc, func(context.Context) ([]byte, error) {
			return mvwDecoder().DecodeAll(frame, nil)
		})
		if err != nil {
			return 0, err
		}
		return s.writeBody(w, octet, cacheImmutable, b), nil
	}
	b, err := s.cfg.Renderer.Cached(ctx, "movers/"+sfc.ID()+"/"+kind+"/"+strconv.FormatInt(int64(t), 10), enc, func(ctx context.Context) ([]byte, error) {
		var mw *core.MoverWindow
		var err error
		if kind == "window" {
			mw, err = s.cfg.Movers.Window(ctx, sfc, t)
		} else {
			mw, err = s.cfg.Movers.Keyframe(ctx, sfc, t)
		}
		if err != nil {
			return nil, err
		}
		return wire.EncodeMoverWindow(mw)
	})
	if err != nil {
		return 0, err
	}
	return s.writeBody(w, octet, cacheImmutable, b), nil
}

// GET overlays/{ds}/{x}/{y}.json?t0=&t1=&kinds=
func (s *Server) serveOverlays(ctx context.Context, w http.ResponseWriter, r *http.Request, tail string) (int, error) {
	parts, ok := splitN(tail, 3)
	if !ok {
		return 0, notFound("no route overlays/%s", truncate(tail))
	}
	key, err := parseDS(parts[0])
	if err != nil {
		return 0, err
	}
	x, err := parseCoord(parts[1])
	if err != nil {
		return 0, err
	}
	ys, ok := strings.CutSuffix(parts[2], ".json")
	if !ok {
		return 0, notFound("no route overlays/%s", truncate(tail))
	}
	y, err := parseCoord(ys)
	if err != nil {
		return 0, err
	}
	tile := core.TileCoord{X: x, Y: y}
	if !tile.Valid() {
		return 0, &apiError{http.StatusBadRequest, "bad_tile", "tile outside the grid"}
	}
	q := r.URL.Query()
	oq := core.OverlayQuery{Tile: tile, T0: -1 << 62, T1: 1 << 62}
	if v := q.Get("t0"); v != "" {
		if oq.T0, err = parseTime(v); err != nil {
			return 0, err
		}
	}
	if v := q.Get("t1"); v != "" {
		if oq.T1, err = parseTime(v); err != nil {
			return 0, err
		}
	}
	if v := q.Get("kinds"); v != "" {
		for _, k := range strings.Split(v, ",") {
			kind := core.IntervalKind(k)
			if !kind.Valid() {
				return 0, badRequest("bad kind %q", truncate(k))
			}
			oq.Kinds = append(oq.Kinds, kind)
		}
	}
	ver, err := s.cfg.Overlays.Version(ctx, key, tile)
	if err != nil {
		return 0, err
	}
	etag := `"` + strconv.FormatUint(ver, 10) + `"`
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		h := w.Header()
		h.Set("ETag", etag)
		h.Set("Cache-Control", s.cacheControl(cacheShort))
		h.Set("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return http.StatusNotModified, nil
	}
	ivs, err := s.cfg.Overlays.Query(ctx, key, oq)
	if err != nil {
		return 0, err
	}
	if ivs == nil {
		ivs = []core.Interval{}
	}
	raw, err := jsonMarshal(core.OverlayResponse{DS: key.ID(), X: x, Y: y, Version: ver, Intervals: ivs})
	if err != nil {
		return 0, err
	}
	return s.writeRawJSON(w, r, cacheShort, raw, etag), nil
}
