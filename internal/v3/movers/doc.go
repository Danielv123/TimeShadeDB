// Package movers is the on-disk core.MoverStore of TimeShadeDB v3 (PLAN.md §4.4,
// docs/v3/FORMAT.md 5.5), plus the journey rules and clock anchors that ingest applies
// before samples reach it.
//
// # Layout
//
// Under <db>/movers/ (FORMAT.md 5):
//
//	<surface id>/meta.json                  {"key":{...},"format_version":1,
//	                                         "horizon":t,"first":t,"last":t}
//	<surface id>/windows/<day>/<start>.mvw  closed 5-minute windows
//	<surface id>/keyframes/<day>/<t>.mvw    hourly keyframes
//	<surface id>/days/<day>.json            day index (core.DayIndex without "final")
//	wal/wal.log                             samples of the open windows
//
// A .mvw file is one zstd frame of an MVW3 body (wire.EncodeMoverWindow), with a window
// of at most 4 MiB, so the HTTP API can send it unchanged with Content-Encoding: zstd
// (Store.WindowFile, Store.KeyframeFile). meta.json carries the horizon and the time
// range in addition to the FORMAT.md fields (additive). "final" in day indexes is
// computed on read from the horizon. Every file is written as tmp + rename, fsynced
// unless Options.SyncInterval < 0; meta.json, which raises the horizon, is written last.
//
// # Write path
//
// Append quantizes samples (core.MoverSample.Quantized) into the open rows of their
// surface and logs them to the WAL as one CRC-framed batch (fsync per batch, or group
// commit with Options.SyncInterval > 0). Commit(upTo) deduplicates the open rows (for
// equal journey and time the last arrival wins), marks handoffs (a gone followed by a
// sample of the same journey within core.HandoffWindow among all rows known at that
// moment), writes the closed windows, the keyframes at every UTC hour passed, and the
// day indexes, then rewrites the WAL with the rows still open. A server should commit
// with a grace of at least core.HandoffWindow behind its clock, or gones at the end of
// a window whose journey resumes in the next one are closed before the resume arrives
// (TestTraceHandoffsStreaming).
//
// Append validates the whole batch and writes it to the WAL before it changes memory:
// a failed WAL write applies nothing. A Commit builds the new live state on copies and
// installs it with the horizon only after meta.json is written, so a failed Commit
// changes nothing in memory and can be retried. A failed WAL rewrite leaves the old log
// and its meta numbering in use. Outside Windows every rename is followed by an fsync of
// its directory (unless SyncInterval < 0), so meta.json's new horizon is durable before
// the WAL rewrite drops the rows it closed.
//
// # Input bounds
//
// Samples with more than MaxCarriages carriages or with coordinates outside int32 units
// are Invalid. Samples more than Options.MaxAhead after the reference clock (the latest
// Commit upTo, or Options.Clock) are Invalid, and samples more than Options.MaxBehind
// before it on a surface without a horizon are Late; open rows accepted before any
// reference existed are dropped by the first Commit (Stats.Ahead, Stats.Behind). This
// keeps a broken clock from holding MaxOpenRows forever, or from making one Commit write
// a keyframe for every hour since a sample near 1970. Bulk importers that stream one
// surface after another, or whose batches jump ahead of their commits, must pass
// MaxAhead: -1 and MaxBehind: -1. WP6 should still clamp T to its own clock, and should
// commit once at start (or set Clock) so the reference exists before ingest resumes.
// After a long downtime the first Commit still writes a keyframe per surface for every
// hour of the gap (FORMAT.md 5.5 has a keyframe at every hour; skipping empty ones would
// be a contract change).
//
// # Memory
//
// No history is held in memory. Per surface the store keeps the open rows (bounded
// across surfaces by Options.MaxOpenRows; Append returns ErrOverloaded beyond it), the
// closed rows of the last core.LiveTrail before the horizon and the last row of every
// live journey, from which keyframes and Live are built, and for every live train a
// core.RailBody over its closed rows and one over its open rows (the rail pieces of about
// two train lengths behind it, at most core.MaxRailPieces + 128), which give rows the body that keyframes and Live put in front
// of a moving train's first row (FORMAT.md 7.4). A row keeps its body as a slice of its
// RailBody's pieces. The open RailBody follows rows as they arrive; a row at or before its
// journey's previous open row makes Append (or the WAL replay) give that journey's open
// rows their bodies again in time order. A commit follows the closed rows in time order,
// begins every RailBody again at each keyframe it writes, and redoes the open rows' bodies
// from the new state. Options.MaxTrailRows and
// Options.MaxLiveMovers bound those two summed over all surfaces: each commit trims the
// surface it commits to what the other surfaces leave of the limit, or an equal share
// if that is more (Store.share). Opening rebuilds these from the newest keyframe, the windows after it
// and the WAL; nothing older is read, except the windows the trail reaches back into and,
// when that is past the keyframe (just after one was written), the keyframe before it and
// the windows since, for the bodies of the trail rows before the keyframe. A window or keyframe that does not decode is
// logged, counted (Stats.Corrupt) and skipped there, so one damaged file cannot stop
// the store opening; Window and Keyframe return an error for it.
//
// The limits are not yet derived from a global --mem-budget (WP6/WP11). Open loads
// each surface with the whole limits and trims them to their shares once all surfaces
// are loaded, so the store briefly holds more while it opens. During a Commit the store briefly holds about 2-3 times the open rows of the surface
// being committed (the sorted and deduplicated row pointers, the rows that stay open,
// the MoverRow arrays and the encoded window bodies) plus a copy of its live map.
//
// Open rows span up to one 5-minute window plus the commit grace (about 315 s), so in
// real time the sustained ingest rate is bounded by MaxOpenRows / 315 s: about 3,300
// samples/s at the default of 1,048,576 rows (about 125 MB at about 120 bytes per row
// without carriages). The store-level throughput in
// bench_test.go (500k+ samples/s) is measured with map time running far faster than
// real time; a server expecting more live samples must raise MaxOpenRows (from its
// memory budget) accordingly.
//
// # Reads
//
// Window and Keyframe decode the files; Day reads the index; Live(now) is served from
// memory when now is at or after the horizon, and otherwise rebuilt from the keyframe
// before now - core.LiveTrail and the windows up to now (skipping damaged files), with the
// bodies followed from that keyframe's rows (and again from each later keyframe passed).
//
// # Journeys and clock anchors
//
// Journeys (journeys.go) applies the owner rule, adoption, resync, gone meta and
// surface changes to decoded move messages; Anchors (anchor.go) maps game ticks to the
// map clock per source (PROTOCOL.md 4). Both are bounded and meant for the ingest
// decoder (WP6), which owns envelope decoding.
package movers
