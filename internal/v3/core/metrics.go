package core

// Prometheus metric names exported at GET /metrics (WP6) and read by the benchmark
// suite (WP11). Names are fixed here so both sides agree; docs/v3/API.md lists them
// with their labels. Labels: ds = datastore id, sfc = surface id, route = API route
// name (the first path segment after /api/v3/), code = HTTP status.
const (
	MetricIngestEnvelopes      = "tsdb_ingest_envelopes_total"       // counter
	MetricIngestBytes          = "tsdb_ingest_bytes_total"           // counter
	MetricIngestRejected       = "tsdb_ingest_rejected_total"        // counter {reason}
	MetricChunkUpdates         = "tsdb_chunk_updates_total"          // counter {result=accepted|late|invalid}
	MetricMoverSamples         = "tsdb_mover_samples_total"          // counter {result}
	MetricOverlayEvents        = "tsdb_overlay_events_total"         // counter {result}
	MetricCommits              = "tsdb_commits_total"                // counter
	MetricIdleReleases         = "tsdb_idle_releases_total"          // counter: freed memory returned to the OS after idle
	MetricBlocksWritten        = "tsdb_terrain_blocks_written_total" // counter {kind=delta|keyframe}
	MetricTerrainBytes         = "tsdb_terrain_bytes_written_total"  // counter
	MetricOpenWindowBytes      = "tsdb_open_window_bytes"            // gauge
	MetricDecodedCacheBytes    = "tsdb_decoded_cache_bytes"          // gauge
	MetricEncodedCacheBytes    = "tsdb_encoded_cache_bytes"          // gauge
	MetricCacheHits            = "tsdb_cache_hits_total"             // counter {cache=decoded|encoded}
	MetricCacheMisses          = "tsdb_cache_misses_total"           // counter {cache}
	MetricHTTPRequests         = "tsdb_http_requests_total"          // counter {route, code}
	MetricHTTPDuration         = "tsdb_http_request_seconds"         // histogram {route}
	MetricLiveClients          = "tsdb_live_clients"                 // gauge
	MetricLiveDropped          = "tsdb_live_clients_dropped_total"   // counter
	MetricClockAnchorOffset    = "tsdb_clock_anchor_offset_ms"       // gauge {src}
	MetricMemBudgetBytes       = "tsdb_mem_budget_bytes"             // gauge
	MetricPublishQueue         = "tsdb_publish_queue"                // gauge (WP12)
	MetricPublishUploadedBytes = "tsdb_publish_uploaded_bytes_total" // counter (WP12)
)
