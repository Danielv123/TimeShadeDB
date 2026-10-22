package api

import (
	"bufio"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"timeshadedb/internal/v3/core"
	"timeshadedb/internal/v3/movers"
	"timeshadedb/internal/v3/overlays"
	"timeshadedb/internal/v3/terrain"
	"timeshadedb/internal/v3/tileenc"
)

type tileencStats = tileenc.LRUStats

// Route names: the metric label (API.md 7: the first path segment after /api/v3/).
const (
	routeOther = iota
	routeCatalog
	routePalette
	routeView
	routeTimeline
	routeTile
	routeTileDelta
	routeMovers
	routeOverlays
	routeLive
	routeIngest
	routeIcons
	routeHealth
	routeMetrics
	routeWeb
	numRoutes
)

var routeNames = [numRoutes]string{"other", "catalog", "palette", "view", "timeline", "tile", "tiledelta", "movers", "overlays", "live", "ingest", "icons", "healthz", "metrics", "web"}

func routeOf(seg string) int {
	switch seg {
	case "catalog", "catalog.json":
		return routeCatalog
	case "palette":
		return routePalette
	case "view":
		return routeView
	case "timeline":
		return routeTimeline
	case "tile":
		return routeTile
	case "tiledelta":
		return routeTileDelta
	case "movers":
		return routeMovers
	case "overlays":
		return routeOverlays
	case "live":
		return routeLive
	case "ingest":
		return routeIngest
	case "icons":
		return routeIcons
	}
	return routeOther
}

// Status codes with their own label; others count as "other".
var codeList = [...]int{200, 304, 400, 401, 404, 405, 413, 415, 500, 503}

const numCodes = len(codeList) + 1

func codeIndex(c int) int {
	for i, v := range codeList {
		if v == c {
			return i
		}
	}
	return len(codeList)
}

// Request duration histogram buckets, seconds.
var buckets = [...]float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 1}

type routeHist struct {
	counts [len(buckets) + 1]atomic.Uint64
	sumNs  atomic.Uint64
}

// metrics holds the request counters. Label sets are fixed, so memory is constant
// whatever the requests (PLAN.md §4.3 G2, G10).
type metrics struct {
	requests [numRoutes][numCodes]atomic.Uint64
	hist     [numRoutes]routeHist
	commits  atomic.Uint64
	// idleReleases counts Config.IdleRelease returns of memory to the OS.
	idleReleases atomic.Uint64
	// heldMaps and holdLag are the maps whose commits a source holds back more than a
	// minute behind the clock, and the largest such hold in ms (noteHolds).
	heldMaps, holdLag atomic.Int64
}

func newMetrics() *metrics { return &metrics{} }

func (m *metrics) observe(route, status int, d time.Duration) {
	if status == 0 {
		status = 200
	}
	m.requests[route][codeIndex(status)].Add(1)
	h := &m.hist[route]
	sec := d.Seconds()
	i := sort.SearchFloat64s(buckets[:], sec)
	h.counts[i].Add(1)
	h.sumNs.Add(uint64(d.Nanoseconds()))
}

func (s *Server) serveMetrics(w http.ResponseWriter) int {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	m := s.metrics
	typ := func(name, t string) { fmt.Fprintf(bw, "# TYPE %s %s\n", name, t) }
	val := func(name, labels string, v any) {
		if labels != "" {
			fmt.Fprintf(bw, "%s{%s} %v\n", name, labels, v)
		} else {
			fmt.Fprintf(bw, "%s %v\n", name, v)
		}
	}
	if in := s.cfg.Ingest; in != nil {
		st := in.Stats()
		typ(core.MetricIngestEnvelopes, "counter")
		val(core.MetricIngestEnvelopes, "", st.Envelopes)
		typ(core.MetricIngestBytes, "counter")
		val(core.MetricIngestBytes, "", st.Bytes)
		typ(core.MetricIngestRejected, "counter")
		val(core.MetricIngestRejected, `reason="bad_line"`, st.BadLines)
		val(core.MetricIngestRejected, `reason="bad_message"`, st.BadMessages)
		val(core.MetricIngestRejected, `reason="overloaded"`, st.Overloaded)
		val(core.MetricIngestRejected, `reason="clamped"`, st.Clamped)
		val(core.MetricIngestRejected, `reason="owner"`, st.OwnerDrops)
		val(core.MetricIngestRejected, `reason="unknown_gone"`, st.UnknownGones)
		val(core.MetricIngestRejected, `reason="quarantined"`, st.Quarantined)
		val(core.MetricIngestRejected, `reason="duplicate"`, st.Duplicates)
		val(core.MetricIngestRejected, `reason="one_recv"`, st.OneRecv)
		typ(core.MetricIngestCaughtUp, "counter")
		val(core.MetricIngestCaughtUp, "", st.CaughtUp)
		typ("tsdb_ingest_resolved_gones_total", "counter")
		val("tsdb_ingest_resolved_gones_total", "", st.ResolvedGones)
		for _, x := range []struct {
			name string
			r    core.ApplyResult
		}{{core.MetricChunkUpdates, st.Chunks}, {core.MetricMoverSamples, st.Samples}, {core.MetricOverlayEvents, st.Overlays}} {
			typ(x.name, "counter")
			val(x.name, `result="accepted"`, x.r.Accepted)
			val(x.name, `result="late"`, x.r.Late)
			val(x.name, `result="invalid"`, x.r.Invalid)
		}
		typ(core.MetricClockAnchorOffset, "gauge")
		in.AnchorOffsets(func(src string, off float64) {
			val(core.MetricClockAnchorOffset, `src=`+strconv.Quote(src), strconv.FormatFloat(off, 'f', 0, 64))
		})
	}
	typ(core.MetricCommits, "counter")
	val(core.MetricCommits, "", m.commits.Load())
	typ(core.MetricIdleReleases, "counter")
	val(core.MetricIdleReleases, "", m.idleReleases.Load())
	typ(core.MetricHeldMaps, "gauge")
	val(core.MetricHeldMaps, "", m.heldMaps.Load())
	typ(core.MetricHoldLag, "gauge")
	val(core.MetricHoldLag, "", m.holdLag.Load())
	if ts, ok := s.cfg.Terrain.(*terrain.Store); ok {
		st := ts.Stats()
		typ(core.MetricBlocksWritten, "counter")
		val(core.MetricBlocksWritten, `kind="delta"`, st.DeltasWritten)
		val(core.MetricBlocksWritten, `kind="keyframe"`, st.KeyframesWritten)
		typ(core.MetricTerrainBytes, "counter")
		val(core.MetricTerrainBytes, "", st.RecordBytes)
		typ(core.MetricOpenWindowBytes, "gauge")
		val(core.MetricOpenWindowBytes, "", st.OpenWindowBytes)
		typ(core.MetricSpilledWindows, "gauge")
		val(core.MetricSpilledWindows, `store="terrain"`, st.SpilledWindows)
		typ(core.MetricSpills, "counter")
		val(core.MetricSpills, `store="terrain"`, st.Spills)
		typ(core.MetricDecodedCacheBytes, "gauge")
		val(core.MetricDecodedCacheBytes, `cache="terrain"`, st.DecodedCacheBytes)
		typ(core.MetricCacheHits, "counter")
		val(core.MetricCacheHits, `cache="decoded"`, st.CacheHits)
		typ(core.MetricCacheMisses, "counter")
		val(core.MetricCacheMisses, `cache="decoded"`, st.CacheMisses)
	}
	if ps, ok := s.cfg.Tiles.(interface{ CacheStats() tileencStats }); ok {
		cs := ps.CacheStats()
		val(core.MetricDecodedCacheBytes, `cache="pyramid"`, cs.Bytes)
		val(core.MetricCacheHits, `cache="pyramid"`, cs.Hits)
		val(core.MetricCacheMisses, `cache="pyramid"`, cs.Misses)
	}
	rs := s.cfg.Renderer.CacheStats()
	typ(core.MetricEncodedCacheBytes, "gauge")
	val(core.MetricEncodedCacheBytes, "", rs.Bytes)
	val(core.MetricCacheHits, `cache="encoded"`, rs.Hits)
	val(core.MetricCacheMisses, `cache="encoded"`, rs.Misses)
	if ms, ok := s.cfg.Movers.(*movers.Store); ok {
		st := ms.Stats()
		typ("tsdb_mover_open_rows", "gauge")
		val("tsdb_mover_open_rows", "", st.OpenRows)
		typ("tsdb_mover_open_bytes", "gauge")
		val("tsdb_mover_open_bytes", "", st.OpenBytes)
		typ(core.MetricSpilledWindows, "gauge")
		val(core.MetricSpilledWindows, `store="movers"`, st.SpilledWindows)
		typ(core.MetricSpills, "counter")
		val(core.MetricSpills, `store="movers"`, st.Evictions)
		typ("tsdb_mover_store_rejected_total", "counter")
		val("tsdb_mover_store_rejected_total", `reason="ahead"`, st.Ahead)
		val("tsdb_mover_store_rejected_total", `reason="behind"`, st.Behind)
		val("tsdb_mover_store_rejected_total", `reason="duplicate"`, st.Duplicates)
		typ("tsdb_mover_live_evicted_total", "counter")
		val("tsdb_mover_live_evicted_total", "", st.Evicted)
		val("tsdb_mover_store_rejected_total", `reason="corrupt"`, st.Corrupt)
		val("tsdb_mover_store_rejected_total", `reason="wal_skipped"`, st.WALSkipped)
	}
	if os, ok := s.cfg.Overlays.(*overlays.Store); ok {
		st := os.Stats()
		typ("tsdb_overlay_open_intervals", "gauge")
		val("tsdb_overlay_open_intervals", "", st.OpenIntervals)
		typ("tsdb_overlay_torn_bytes_total", "counter")
		val("tsdb_overlay_torn_bytes_total", "", st.TornBytes)
	}
	typ(core.MetricHTTPRequests, "counter")
	for r := range numRoutes {
		for c := range numCodes {
			n := m.requests[r][c].Load()
			if n == 0 {
				continue
			}
			code := "other"
			if c < len(codeList) {
				code = strconv.Itoa(codeList[c])
			}
			val(core.MetricHTTPRequests, `route="`+routeNames[r]+`",code="`+code+`"`, n)
		}
	}
	typ(core.MetricHTTPDuration, "histogram")
	for r := range numRoutes {
		h := &m.hist[r]
		var cum uint64
		total := uint64(0)
		for i := range h.counts {
			total += h.counts[i].Load()
		}
		if total == 0 {
			continue
		}
		for i, b := range buckets {
			cum += h.counts[i].Load()
			val(core.MetricHTTPDuration+"_bucket", `route="`+routeNames[r]+`",le="`+strconv.FormatFloat(b, 'g', -1, 64)+`"`, cum)
		}
		val(core.MetricHTTPDuration+"_bucket", `route="`+routeNames[r]+`",le="+Inf"`, total)
		val(core.MetricHTTPDuration+"_sum", `route="`+routeNames[r]+`"`, strconv.FormatFloat(float64(h.sumNs.Load())/1e9, 'g', -1, 64))
		val(core.MetricHTTPDuration+"_count", `route="`+routeNames[r]+`"`, total)
	}
	typ(core.MetricLiveClients, "gauge")
	val(core.MetricLiveClients, "", s.hub.count())
	typ(core.MetricLiveDropped, "counter")
	val(core.MetricLiveDropped, "", s.hub.dropped.Load())
	typ(core.MetricMemBudgetBytes, "gauge")
	val(core.MetricMemBudgetBytes, "", s.cfg.MemBudget)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	typ("go_memstats_heap_inuse_bytes", "gauge")
	val("go_memstats_heap_inuse_bytes", "", ms.HeapInuse)
	typ("go_memstats_heap_alloc_bytes", "gauge")
	val("go_memstats_heap_alloc_bytes", "", ms.HeapAlloc)
	typ("go_memstats_sys_bytes", "gauge")
	val("go_memstats_sys_bytes", "", ms.Sys)
	typ("go_goroutines", "gauge")
	val("go_goroutines", "", runtime.NumGoroutine())
	return http.StatusOK
}
