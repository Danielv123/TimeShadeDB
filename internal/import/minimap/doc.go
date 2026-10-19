// Package minimap imports an illisis/minimap export (the controller's minimap database
// directory or its S3 mirror) into TimeShadeDB v3 stores (PLAN.md §6 WP5).
//
// Portions of this package (the MMC1/MMS1 codec, the key and area arithmetic, the
// window, keyframe and version tuple shapes) are ported from illisis-minimap
// (shared/codec.ts, shared/keys.ts, shared/replay.ts, store/movers.ts, contracts.ts),
// which is licensed under the Blue Oak Model License 1.0.0:
// https://blueoakcouncil.org/license/1.0.0
//
// # What is read
//
// The export root holds maps.json.gz (the catalog) and one directory per map:
//
//	<map>/<surface>/<force>/0/<ax>_<ay>/index.json.gz   {snaps, changes:[[first,last]..]}
//	<map>/<surface>/<force>/0/<ax>_<ay>/<t>.c.gz        MMC1 change records
//	<map>/<surface>/<force>/0/<ax>_<ay>/recipes.json.gz recipe versions
//	<map>/<surface>/<force>/tags.json.gz                tag versions
//	<map>/<surface>/m/<t>.m.gz                          5-minute mover windows
//	<map>/<surface>/m/<t>.k.gz                          hourly mover keyframes
//
// Only the z0 change files are imported. Snapshots and the coarse levels z1-z4 are
// derivable (research/testdata.md finding 6) and are only read by Verify, which checks
// the imported state against them.
//
// # What is written
//
//   - Terrain: one core.ChunkUpdate per (chunk, terrain window) holding the chunk's state
//     after the last record in that window, fed in global time order with periodic
//     TerrainStore.Commit calls, so a store never sees a late update.
//   - Movers: one core.MoverSample per window row after the data-quality fixes below, fed
//     in global time order with MoverStore.Commit calls that trail the feed by more than
//     core.HandoffWindow, so the store can flag handoffs. The mover import streams (see
//     moverstream.go): window files are decoded in time order with a small lookahead,
//     each journey is fixed in segments, and the surfaces are merged by time, so memory
//     does not grow with the length of the history.
//   - Overlays: tag and recipe versions as open/close core.OverlayEvents.
//   - Map metadata (titles, gridworld instances and bounds, Universal Edges segments) as
//     core.MapInfo values in the Report, for <db>/maps.json.
//
// # Data-quality fixes (research/testdata.md §3.4)
//
//   - Clamp re-timing: illisis's store re-stamped samples that reached the controller
//     after a window close to the window floor (store/movers.ts `Math.max(t, s.floor)`).
//     Samples stamped on a floor (t mod window in 0..2 ms, or the earliest time of a
//     window file that starts less than 1 s after a window boundary) are re-timed: a
//     run of them between two good samples is spread over that interval by distance
//     travelled, otherwise each is placed by distance over its reported speed. Only
//     moves of 50 ms to 5 s earlier are applied; a sample that is not re-timed keeps its
//     stamp, so no sample moves later. A gone clamped together with the sample after it
//     moves back with it. A run whose next row has not arrived 30 minutes later is
//     fixed without it (the streaming bound; it does not happen on the test data).
//   - End switches: a train sample that jumps, at low speed and within 3 s, by the
//     train's length ((carriages - 1) * 7 tiles) gets core.FlagEndSwitch. This is a
//     heuristic; it is not checked against ground truth.
//   - (journey, t) duplicates: the last one in arrival order is kept. Re-timing never
//     creates equal times, so the dropped rows are source rows followed by a row of the
//     same journey with the same stamp that was not re-timed.
//   - Handoffs: a gone followed by a sample of the same journey within
//     core.HandoffWindow is counted here; the store sets core.FlagHandoff itself.
//   - Headings: vehicle rows carry no heading in windows; an estimate is derived from
//     the velocity (the previous one is kept while the vehicle stands still) and the row
//     keeps core.FlagNoHeading, so viewers can tell it from a captured heading. Player
//     rows get core.FlagNoHeading and no estimate. Every imported sample has
//     core.FlagNoTick.
//   - Keyframe seeds: a keyframe mover of a journey that has no window row at or before
//     the keyframe is imported as a sample at its own time, but no earlier than 30
//     minutes before the keyframe (so it never falls behind rows already fed).
//
// # Verification
//
// VerifyTerrain compares the store with illisis's own snapshots and replay. VerifyMovers
// decodes the window files again and looks every source row up in the store (journey,
// gone or sample, time within the re-timing bound, position within 1/16 tile, flags),
// and checks the result against the import report. VerifyOverlays reads the intervals
// back.
//
// The data is untrusted: every file is size-capped after decompression, every field is
// range-checked, and paths are built only from directory listings and integers. Files
// that fail to read or decode are skipped and counted in the reports (error_count);
// import-minimap then exits with an error unless --allow-errors is given.
package minimap
