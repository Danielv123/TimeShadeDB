package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"timeshadedb/internal/v3/core"
)

// Live events (API.md 5): a hub of SSE clients with bounded buffers. Publishing never
// blocks: a client whose queued bytes would exceed its buffer is disconnected and must
// reconnect and resync from the regular routes.

type hub struct {
	maxClients int
	maxBytes   int

	mu      sync.Mutex
	clients map[*client]struct{}
	dropped atomic.Int64
}

type client struct {
	ds, sfc map[string]bool

	mu      sync.Mutex
	queue   [][]byte
	bytes   int
	dropped bool
	wake    chan struct{}
}

func newHub(maxClients, maxBytes int) *hub {
	return &hub{maxClients: maxClients, maxBytes: maxBytes, clients: map[*client]struct{}{}}
}

func (h *hub) add(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= h.maxClients {
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// wants reports whether any client subscribes to the datastore or surface id.
func (h *hub) wants(ds, sfc string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if (ds != "" && c.ds[ds]) || (sfc != "" && c.sfc[sfc]) {
			return true
		}
	}
	return false
}

// publish queues an event for the clients subscribed to ds or sfc (both "" = everyone).
func (h *hub) publish(ds, sfc string, msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if ds == "" && sfc == "" || (ds != "" && c.ds[ds]) || (sfc != "" && c.sfc[sfc]) {
			if !c.push(msg, h.maxBytes) {
				h.dropped.Add(1)
			}
		}
	}
}

// push queues msg; it returns false when that drops the client.
func (c *client) push(msg []byte, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped {
		return true
	}
	if c.bytes+len(msg) > max {
		c.dropped = true
		c.queue, c.bytes = nil, 0
		c.signal()
		return false
	}
	c.queue = append(c.queue, msg)
	c.bytes += len(msg)
	c.signal()
	return true
}

func (c *client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// take returns the queued events.
func (c *client) take() ([][]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.queue
	c.queue, c.bytes = nil, 0
	return q, c.dropped
}

func event(name string, v any) []byte {
	b, _ := json.Marshal(v)
	out := make([]byte, 0, len(name)+len(b)+16)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, "\ndata: "...)
	out = append(out, b...)
	out = append(out, "\n\n"...)
	return out
}

// GET live?ds=&ds=&sfc=
func (s *Server) serveLive(w http.ResponseWriter, r *http.Request) int {
	fl, ok := w.(http.Flusher)
	if !ok {
		return s.writeError(w, badRequest("streaming unsupported"))
	}
	q := r.URL.Query()
	c := &client{ds: map[string]bool{}, sfc: map[string]bool{}, wake: make(chan struct{}, 1)}
	hello := map[string]core.TimeMs{}
	ctx := r.Context()
	infos, err := s.datastores(ctx)
	if err != nil {
		return s.writeError(w, err)
	}
	if len(q["ds"])+len(q["sfc"]) > 256 {
		return s.writeError(w, badRequest("too many topics"))
	}
	for _, id := range q["ds"] {
		k, err := parseDS(id)
		if err != nil {
			return s.writeError(w, err)
		}
		c.ds[id] = true
		if info, ok := infos[k]; ok {
			hello[id] = info.Horizon
		}
	}
	if len(q["sfc"]) > 0 {
		surfaces, err := s.cfg.Movers.Surfaces(ctx)
		if err != nil {
			return s.writeError(w, err)
		}
		for _, id := range q["sfc"] {
			if _, err := parseSurface(id); err != nil {
				return s.writeError(w, err)
			}
			c.sfc[id] = true
			for _, si := range surfaces {
				if si.ID == id {
					hello[id] = si.Horizon
				}
			}
		}
	}
	select {
	case <-s.closing:
		return s.writeError(w, &apiError{http.StatusServiceUnavailable, "overloaded", "shutting down"})
	default:
	}
	if !s.hub.add(c) {
		return s.writeError(w, &apiError{http.StatusServiceUnavailable, "overloaded", "too many live clients"})
	}
	defer s.hub.remove(c)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(event("hello", map[string]any{"now": s.cfg.Clock(), "horizon": hello})); err != nil {
		return http.StatusOK
	}
	fl.Flush()
	ping := time.NewTicker(s.cfg.LivePing)
	defer ping.Stop()
	rc := http.NewResponseController(w)
	for {
		select {
		case <-ctx.Done():
			return http.StatusOK
		case <-s.closing:
			return http.StatusOK
		case <-ping.C:
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return http.StatusOK
			}
			fl.Flush()
		case <-c.wake:
			msgs, dropped := c.take()
			if dropped {
				return http.StatusOK
			}
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			for _, m := range msgs {
				if _, err := w.Write(m); err != nil {
					return http.StatusOK
				}
			}
			fl.Flush()
		}
	}
}

// liveState tracks what live events have announced, to publish only changes.
type liveState struct {
	mu sync.Mutex
	// dirty holds z0 tiles with open-window updates since the last flush.
	dirty    map[tileKey]struct{}
	overflow map[core.DatastoreKey]struct{}
	// seen is the last published state of every tile (all levels).
	seen map[tileKey]core.TileState
	// horizons are the last published horizons per datastore and surface id.
	horizons map[string]core.TimeMs
	known    map[string]struct{} // datastore and surface ids in the last catalog
}

type tileKey struct {
	ds   core.DatastoreKey
	tile core.TileCoord
}

// maxDirty bounds the dirty set; beyond it a datastore is rescanned as a whole.
const maxDirty = 65536

func newLiveState() *liveState {
	return &liveState{dirty: map[tileKey]struct{}{}, overflow: map[core.DatastoreKey]struct{}{},
		seen: map[tileKey]core.TileState{}, horizons: map[string]core.TimeMs{}, known: map[string]struct{}{}}
}

// notifier implements ingest.Notifier for a Server.
type notifier Server

func (n *notifier) ChunksApplied(updates []core.ChunkUpdate) {
	ls := n.live
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for i := range updates {
		u := &updates[i]
		tile, _ := u.Chunk.Tile()
		k := tileKey{u.DS, tile}
		if _, ok := ls.dirty[k]; ok {
			continue
		}
		if len(ls.dirty) >= maxDirty {
			ls.overflow[u.DS] = struct{}{}
			continue
		}
		ls.dirty[k] = struct{}{}
	}
}

func (n *notifier) SamplesApplied(samples []core.MoverSample) {
	s := (*Server)(n)
	if s.hub.count() == 0 {
		return
	}
	now := s.cfg.Clock()
	type group struct {
		metas map[core.MoverMeta]int
		ev    moversEvent
	}
	groups := map[core.SurfaceKey]*group{}
	for i := range samples {
		q := samples[i].Quantized()
		g := groups[q.Surface]
		if g == nil {
			id := q.Surface.ID()
			if !s.hub.wants("", id) {
				continue
			}
			g = &group{metas: map[core.MoverMeta]int{}, ev: moversEvent{Sfc: id, Now: now, Movers: []core.MoverMeta{}, Rows: [][]any{}}}
			groups[q.Surface] = g
		}
		e, ok := g.metas[q.Meta]
		if !ok {
			e = len(g.ev.Movers)
			g.metas[q.Meta] = e
			g.ev.Movers = append(g.ev.Movers, q.Meta)
		}
		var tick any
		if q.Flags&core.FlagNoTick == 0 {
			tick = q.Tick
		}
		var cars any
		if len(q.Carriages) > 0 {
			cs := make([][3]float64, len(q.Carriages))
			for j, c := range q.Carriages {
				cs[j] = [3]float64{c.X, c.Y, c.Orientation}
			}
			cars = cs
		}
		g.ev.Rows = append(g.ev.Rows, []any{e, q.T, tick, q.X, q.Y, q.Orientation, int(q.Flags), cars})
	}
	for _, g := range groups {
		s.hub.publish("", g.ev.Sfc, event("movers", g.ev))
	}
}

func (n *notifier) MapsChanged() {
	s := (*Server)(n)
	s.invalidateCatalog()
	s.hub.publish("", "", event("catalog", map[string]any{"now": s.cfg.Clock()}))
}

type moversEvent struct {
	Sfc    string           `json:"sfc"`
	Now    core.TimeMs      `json:"now"`
	Movers []core.MoverMeta `json:"movers"`
	Rows   [][]any          `json:"rows"`
}

type tileEvent struct {
	DS   string       `json:"ds"`
	Z    int8         `json:"z"`
	X    int32        `json:"x"`
	Y    int32        `json:"y"`
	V    core.Version `json:"v"`
	Live bool         `json:"live,omitempty"`
	LS   uint64       `json:"ls,omitempty"`
	Now  core.TimeMs  `json:"now"`
}

// flushDirty publishes tile events for z0 tiles whose open-window content changed.
func (s *Server) flushDirty(ctx context.Context) {
	ls := s.live
	ls.mu.Lock()
	dirty, overflow := ls.dirty, ls.overflow
	ls.dirty, ls.overflow = map[tileKey]struct{}{}, map[core.DatastoreKey]struct{}{}
	ls.mu.Unlock()
	if len(dirty) == 0 && len(overflow) == 0 {
		return
	}
	infos, err := s.datastores(ctx)
	if err != nil {
		return
	}
	newDS := false
	for k := range dirty {
		if _, ok := infos[k.ds]; !ok {
			newDS = true
		}
	}
	for k := range overflow {
		if _, ok := infos[k]; !ok {
			newDS = true
		}
	}
	if newDS {
		s.invalidateDatastores()
		if infos, err = s.datastores(ctx); err != nil {
			return
		}
		s.checkCatalog(ctx)
	}
	for k := range overflow {
		tiles, err := s.cfg.Terrain.ListTiles(ctx, k, 0)
		if err != nil {
			continue
		}
		for _, ti := range tiles {
			if ti.Live {
				dirty[tileKey{k, ti.Tile}] = struct{}{}
			}
		}
	}
	now := s.cfg.Clock()
	for k := range dirty {
		info, ok := infos[k.ds]
		if !ok {
			continue
		}
		id := info.ID
		if !s.hub.wants(id, "") {
			continue
		}
		st, err := s.cfg.Terrain.VersionAt(ctx, k.ds, k.tile, max(now, info.LastT))
		if err != nil {
			continue
		}
		s.publishTile(k, id, st, now)
	}
}

func (s *Server) publishTile(k tileKey, id string, st core.TileState, now core.TimeMs) {
	ls := s.live
	ls.mu.Lock()
	prev, ok := ls.seen[k]
	if ok && prev == st {
		ls.mu.Unlock()
		return
	}
	ls.seen[k] = st
	ls.mu.Unlock()
	s.hub.publish(id, "", event("tile", tileEvent{DS: id, Z: k.tile.Z, X: k.tile.X, Y: k.tile.Y, V: st.Version, Live: st.Live, LS: st.LiveSeq, Now: now}))
}

// afterCommit publishes horizon and tile events after a commit, and a catalog event
// when datastores or surfaces appeared.
func (s *Server) afterCommit(ctx context.Context) {
	s.invalidateDatastores()
	s.invalidateCatalog()
	infos, err := s.cfg.Terrain.Datastores(ctx)
	if err != nil {
		return
	}
	now := s.cfg.Clock()
	ls := s.live
	for _, info := range infos {
		ls.mu.Lock()
		prev, ok := ls.horizons[info.ID]
		ls.horizons[info.ID] = info.Horizon
		ls.mu.Unlock()
		if ok && prev != info.Horizon {
			s.hub.publish(info.ID, "", event("horizon", map[string]any{"ds": info.ID, "horizon": info.Horizon, "now": now}))
		}
		for z := int8(0); z >= core.MinZ; z-- {
			tiles, err := s.cfg.Tiles.ListTiles(ctx, info.Key, z)
			if err != nil || len(tiles) == 0 {
				break
			}
			for _, ti := range tiles {
				k := tileKey{info.Key, ti.Tile}
				st := core.TileState{Version: ti.Version, Live: ti.Live}
				ls.mu.Lock()
				prev, ok := ls.seen[k]
				ls.mu.Unlock()
				if ok && prev.Version == st.Version && prev.Live == st.Live && !st.Live {
					continue
				}
				if st.Live || (ok && prev.Live) {
					// The live sequence needs a VersionAt at the latest instant.
					full, err := s.cfg.Tiles.VersionAt(ctx, info.Key, ti.Tile, max(now, info.LastT))
					if err != nil {
						continue
					}
					st = full
				}
				s.publishTile(k, info.ID, st, now)
			}
		}
	}
	surfaces, err := s.cfg.Movers.Surfaces(ctx)
	if err == nil {
		for _, si := range surfaces {
			ls.mu.Lock()
			prev, ok := ls.horizons[si.ID]
			ls.horizons[si.ID] = si.Horizon
			ls.mu.Unlock()
			if ok && prev != si.Horizon {
				s.hub.publish("", si.ID, event("horizon", map[string]any{"sfc": si.ID, "horizon": si.Horizon, "now": now}))
			}
		}
	}
	s.checkCatalog(ctx)
}

// checkCatalog publishes a catalog event when the set of datastores or surfaces changed.
func (s *Server) checkCatalog(ctx context.Context) {
	infos, err := s.cfg.Terrain.Datastores(ctx)
	if err != nil {
		return
	}
	surfaces, err := s.cfg.Movers.Surfaces(ctx)
	if err != nil {
		return
	}
	ids := make(map[string]struct{}, len(infos)+len(surfaces))
	for _, info := range infos {
		ids["ds:"+info.ID] = struct{}{}
	}
	for _, si := range surfaces {
		ids["sfc:"+si.ID] = struct{}{}
	}
	ls := s.live
	ls.mu.Lock()
	changed := false
	first := true
	for k := range ls.known {
		if len(k) > 3 && (k[:3] == "ds:" || k[:4] == "sfc:") {
			first = false
			if _, ok := ids[k]; !ok {
				changed = true
			}
		}
	}
	for k := range ids {
		if _, ok := ls.known[k]; !ok {
			changed = true
			ls.known[k] = struct{}{}
		}
	}
	ls.mu.Unlock()
	if changed {
		s.invalidateCatalog()
		if !first {
			s.hub.publish("", "", event("catalog", map[string]any{"now": s.cfg.Clock()}))
		}
	}
}
