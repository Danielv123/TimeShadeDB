# Verify TimeShadeDB changes

1. Build the web bundle first: `npm --prefix web ci && npm --prefix web run build`.
2. Build the CLI: `go build -o <temp>/timeshadedb.exe ./cmd/timeshadedb`.
3. Run the unit tests that cover the change: `go test ./...`, `npm --prefix web test`, and for capture changes `npm test` in `capture/test` (plus `node capture/test/run_game.mjs` when Factorio is installed).
4. Drive the server end to end: `timeshadedb serve --db <temp>/db --listen 127.0.0.1:0`, post capture envelopes to `POST /api/v3/ingest` (docs/PROTOCOL.md) or forward a capture directory with `timeshadedb tail --once`, then read them back through `/api/v3/catalog`, tiles and movers (docs/API.md).
5. For storage changes, restart the server on the same database and check that the data is still there.

Use `$CLAUDE_JOB_DIR/tmp` for binaries and databases. Stop background servers after capture.
