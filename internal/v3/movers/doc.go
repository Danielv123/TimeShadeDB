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
// # Memory
//
// No history is held in memory. Per surface the store keeps the open rows (bounded
// across surfaces by Options.MaxOpenRows; Append returns ErrOverloaded beyond it), the
// closed rows of the last core.LiveTrail before the horizon (Options.MaxTrailRows) and
// the last row of every live journey (Options.MaxLiveMovers), from which keyframes and
// Live are built. Opening rebuilds these from the newest keyframe, the windows after it
// and the WAL; nothing older is read.
//
// # Reads
//
// Window and Keyframe decode the files; Day reads the index; Live(now) is served from
// memory when now is at or after the horizon, and otherwise rebuilt from the keyframe
// before now - core.LiveTrail and the windows up to now.
//
// # Journeys and clock anchors
//
// Journeys (journeys.go) applies the owner rule, adoption, resync, gone meta and
// surface changes to decoded move messages; Anchors (anchor.go) maps game ticks to the
// map clock per source (PROTOCOL.md 4). Both are bounded and meant for the ingest
// decoder (WP6), which owns envelope decoding.
package movers
