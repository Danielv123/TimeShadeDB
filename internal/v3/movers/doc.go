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
//	<surface id>/open/<start>.wal           samples of one open window (open.go)
//	wal/wal.log                             samples of the open windows (older stores;
//	                                        Open moves them into the window logs)
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
// Append quantizes samples (core.MoverSample.Quantized) into the open windows of their
// surface and logs them to each window's log as one CRC-framed batch (fsync per batch,
// or group commit with Options.SyncInterval > 0). Commit(upTo) (or CommitBy, with a
// target per surface) closes the windows that end at or before the target one at a time:
// it deduplicates the window's rows (for equal journey and time the last arrival wins,
// Stats.Duplicates), marks handoffs (a gone followed by a sample of the same journey
// within core.HandoffWindow among all rows known at that moment, also early in the next
// open window), writes the closed window, the keyframes at every UTC hour passed, and
// the day indexes, then meta.json, then deletes the closed windows' logs. A server should
// commit with a grace of at least core.HandoffWindow behind its clock, or gones at the
// end of a window whose journey resumes in the next one are closed before the resume
// arrives (TestTraceHandoffsStreaming).
//
// Append validates the whole batch and writes it to the window logs before it changes
// memory: a failed write applies nothing (the logs written are cut back). A batch that
// touches several windows is one write per window log, so a crash between them can leave
// it in some of the logs only; such a batch never returned, its sender sends it again,
// and the repeated rows are removed at commit (for equal journey and time the last
// arrival wins). A Commit
// builds the new live state on copies and installs it with the horizon only after
// meta.json is written, so a failed Commit changes nothing in memory and can be retried.
// A crash after meta.json leaves logs of closed windows, which Open deletes. Outside
// Windows every rename is followed by an fsync of its directory (unless SyncInterval <
// 0), so meta.json's new horizon is durable before the logs it closed are deleted.
//
// # Input bounds
//
// Samples with more than MaxCarriages carriages or with coordinates outside int32 units
// are Invalid. Samples more than Options.MaxAhead after the reference clock (the latest
// Commit upTo, or Options.Clock) are Invalid (a time in the future is impossible), and
// so are samples before Options.MinTime (2016); open windows accepted before any
// reference existed that start beyond MaxAhead are dropped by the first Commit
// (Stats.Ahead, Stats.Behind). Any time between is accepted on a surface without a
// horizon, however old: a backlog after a long downtime is stored. Bulk importers that
// stream one surface after another, or whose batches jump ahead of their commits, must
// pass MaxAhead: -1. WP6 should still clamp T to its own clock, and should commit once at
// start (or set Clock) so the reference exists before ingest resumes. After a long
// downtime, or for a surface whose first sample is old, a Commit writes a keyframe for
// every hour of the gap (FORMAT.md 5.5 has a keyframe at every hour; skipping empty ones
// would be a contract change), at most MaxKeyframesPerCommit per surface: the horizon
// then catches up over the next commits, so no single commit stalls the loop.
//
// # Memory
//
// No history is held in memory. Per surface the store keeps the open windows' rows
// (their accounted memory bounded across surfaces by Options.OpenBytes: beyond it whole
// windows are evicted to their logs, oldest first, keeping an index of each journey's
// newest row; Append never refuses rows for memory), the
// closed rows of the last core.LiveTrail before the horizon and the last row of every
// live journey, from which keyframes and Live are built, and for every live train a
// core.RailBody over its closed rows and one over its open rows (the rail pieces of about
// two train lengths behind it, at most core.MaxRailPieces + 128), which give rows the body that keyframes and Live put in front
// of a moving train's first row (FORMAT.md 7.4). A row keeps its body as a slice of its
// RailBody's pieces. The open RailBody follows rows as they arrive; a row at or before its
// journey's previous open row makes Append (or the WAL replay) give that journey's open
// rows their bodies again in time order (reading back an evicted window that holds rows
// of it). A commit follows the closed rows in time order, begins every RailBody again at
// each keyframe it writes, and redoes the open rows' bodies from the new state (reading
// back evicted windows only when it wrote a keyframe; otherwise a journey with evicted
// rows keeps the RailBody that already followed them). Options.MaxTrailRows and
// Options.MaxLiveMovers bound those two summed over all surfaces: each commit trims the
// surface it commits to what the other surfaces leave of the limit, or an equal share
// if that is more (Store.share). Opening rebuilds these from the newest keyframe, the windows after it
// and the WAL; nothing older is read, except the windows the trail reaches back into and,
// when that is past the keyframe (just after one was written), the keyframe before it and
// the windows since, for the bodies of the trail rows before the keyframe. A window or keyframe that does not decode is
// logged, counted (Stats.Corrupt) and skipped there, so one damaged file cannot stop
// the store opening; Window and Keyframe return an error for it.
//
// MaxTrailRows and MaxLiveMovers are not derived from a global --mem-budget (WP6/WP11).
// Open loads each surface with the whole limits and trims them to their shares once all
// surfaces are loaded, so the store briefly holds more while it opens. Open replays the
// window logs oldest first and evicts as it goes, so the newest windows end up resident.
// During a Commit the store briefly holds about 2-3 times the rows of the window being
// closed (the sorted and deduplicated row pointers, the MoverRow arrays and the encoded
// window body, and for an evicted window the rows read back) plus a copy of its live
// map; a window's rows are bounded by the data rate times 5 minutes, not by a backlog.
//
// Ingest is not limited by memory: a burst or a backlog that outruns the commits (a
// horizon held back for a source that is catching up, or map time running far faster
// than real time) is evicted to the logs and read back at commit, so the rate is bound
// by CPU and disk. The store-level throughput is in bench_test.go.
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
