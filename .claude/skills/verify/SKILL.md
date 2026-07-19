# Verify TimeShadeDB persistence changes

1. Build the web bundle first: `npm --prefix web ci && npm --prefix web run build`.
2. Build the CLI: `go build -o <temp>/timeshadedb.exe ./cmd/timeshadedb`.
3. Drive chunk persistence through `serve`, `POST /api/ingest/chunk/{save}`, `/api/chunk/saves`, and `/api/chunk/tiles/...`.
4. Drive legacy persistence through `import-csv`, `query-tile`, and `compact` using a small generated gzip CSV.
5. For migration infrastructure without a production migration yet, build a temporary source copy with a no-op migration registered, and another whose migration returns an error. Verify read-only rejection, writable migration, reopen, and unchanged source after failure.

Use `$CLAUDE_JOB_DIR/tmp` for binaries and databases. Stop background servers after capture.
