// Package core holds the shared types and store interfaces of TimeShadeDB v3.
//
// It is the contract every v3 work package codes against (PLAN.md §6 WP0). The
// normative descriptions live in docs/FORMAT.md (on-disk and wire formats),
// docs/API.md (HTTP API) and docs/PROTOCOL.md (capture envelopes); the doc
// comments here repeat the parts that matter for callers.
//
// Contents:
//   - time.go: TimeMs, window arithmetic, UTC day names.
//   - keys.go: DatastoreKey (map, surface, force), SurfaceKey (map, surface), the
//     escaped ids used in paths and URLs.
//   - grid.go: chunk and tile coordinates, the -256 tile grid origin, levels z <= 0.
//   - pixels.go: RGB565 chunk and tile images, the 2x2 average used by the pyramid.
//   - terrain.go: ChunkUpdate, tile versions, TerrainStore and TileSource.
//   - movers.go: MoverSample, MoverWindow, DayIndex, MoverStore.
//   - overlays.go: Interval, OverlayEvent, OverlayStore.
//   - series.go: the append-only record log interface (Series, SeriesStore).
//   - apitypes.go: JSON shapes of the HTTP API and the static export.
//   - protocol.go: capture envelope and message types, chart encoding.
//   - metrics.go: Prometheus metric names shared by the server and the benchmarks.
//
// Reference implementations of every interface live in core/memstore. They keep all
// data in memory and favour obviousness over speed: they are test oracles, not
// production stores. core/coretest holds the contract tests every implementation runs,
// and core/wire the reference codecs for the wire formats (and the web fixtures).
//
// Rules for changing this package: changes are additive (new fields, new methods on
// new interfaces). A work package that needs a change makes the smallest additive one
// and lists it in its report.
package core
