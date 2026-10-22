// Package publish writes the static export of a v3 database (docs/API.md 6, PLAN.md
// §4.6, WP12): the same relative paths as the HTTP API, written to a directory or
// uploaded to an S3-compatible bucket (AWS S3, Cloudflare R2, MinIO) behind a CDN, so
// the viewer runs against the bucket with its static DataSource.
//
// A Source reads API responses: HandlerSource from the API server over an opened
// database in the same process, HTTPSource from a running server. A Target stores
// objects: DirTarget (files) or S3Target (signed PUTs, SigV4 without an SDK). The
// Publisher plans each round from the catalog, the timelines and the mover day indexes,
// keeps the round's objects in a durable queue, and remembers in its State which
// objects the target holds: immutable objects (tile versions, deltas, palettes, mover
// windows and keyframes, final timelines and day indexes, content-hashed viewer
// assets) are read and stored once; mutable ones (catalog, open timelines and day
// indexes, overlays, live movers, index.html) are stored again only when their bytes
// change. README.md describes the layout, the tile-version policy and the commands.
package publish
