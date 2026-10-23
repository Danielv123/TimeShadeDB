# TimeShadeDB map for Clusterio

A Clusterio 2 plugin (`timeshade`) that records every instance's map into a TimeShadeDB v3 server
and shows it in the Clusterio web UI. It records terrain from the game's own chart, trains,
vehicles, players, chart tags, machine recipes and optional rails, live and through time.

| Part | What it does |
|---|---|
| Instances (`instance.ts`, `forward.ts`) | The save-patch module `timeshade` (the capture Lua, [capture/](../capture/README.md)) prints envelopes on the IPC channel `timeshade:out`. The plugin stamps each one with `recv_ms` and `seq`, splits it by map, and sends what arrived to the controller about once a second, as a request; what the controller did not acknowledge is sent again. At every start, on a `gridworld.tile` change, on a capture setting change and on a new controller session it runs `/sc ts_rt.config(...) ts_rt.resync(...)` over RCON. |
| Controller (`controller.ts`) | Runs `timeshadedb serve-v3 --listen 127.0.0.1:0` as a sidecar (`sidecar.ts`). It forwards envelopes to the sidecar's `POST /api/v3/ingest` (`ingest.ts`), and publishes the cluster's maps as `map` messages: titles, gridworld instance bounds and Universal Edges lines. |
| Proxy (`http.ts`) | `/api/timeshade/*` on the controller's web server goes to the sidecar. Viewer files are open. The API (`/api/timeshade/api/v3/*`) needs a Clusterio token of a user with `timeshade.view`. |
| Web UI (`web/`) | A **Map** page at `/timeshade` that shows the TimeShadeDB viewer in a frame. |

## Maps

Map keys follow [docs/v3/PROTOCOL.md](../docs/v3/PROTOCOL.md): `gw-<world>` for a gridworld
world, `i-<instance id>` for everything else. A gridworld tile instance sends its tiled surfaces
to its world's map: always its `tile.surface`, plus the surfaces the capture marks tiled. Its
other surfaces, such as space platforms, go to its own map. Each envelope carries one map, so
the instance splits Lua's envelopes. Messages without a surface go to every map of the
instance: `surfaces`, `resynced` and `adopt`.

A train or vehicle that crosses between instances, through Universal Edges or a gridworld
border, keeps its journey id. The destination sends `adopt`. The sidecar then stores it as one
journey, with a handoff where the old instance's gone and the new instance's first sample are
less than 10 s apart. The old instance's last samples and gone often reach the sidecar after
the adopt (each instance sends once a second); they are kept by their time.

## Install

1. Install the plugin next to Clusterio:
   `npm install @timeshadedb/clusterio-plugin` (not published yet; use the tarball from
   `npm pack`). Automatic discovery adds it to `plugin-list.json`.
2. The binary: npm installs the `@timeshadedb/bin-<platform>-<arch>` package for the machine as
   an optional dependency. These packages are built by `npm run build:npm-binaries` and are not
   published yet. Until then, set `timeshade.binary_path` or the `TIMESHADEDB_BINARY`
   environment variable, or put a build in `bin/` (`npm run build:binary -- --web`).
3. Restart the controller and the hosts. Instances get the module at their next start (save
   patching).

The binary must embed a viewer built with relative asset URLs (`vite build --base ./`; the v3
viewer config sets `base: "./"`). Otherwise the viewer cannot load its scripts under
`/api/timeshade/`.

### Controller config

| Field | Default | Meaning |
|---|---|---|
| `timeshade.binary_path` | unset | The timeshadedb executable. Unset: `TIMESHADEDB_BINARY`, then the platform package, then `bin/` |
| `timeshade.database_directory` | `<controller database>/timeshade` | Where the map is stored |
| `timeshade.mem_budget_mb` | 256 | `serve-v3 --mem-budget` |
| `timeshade.queue_mb` | 64 | Capture data kept in memory while the sidecar is down or slow; past it the queue continues on disk |
| `timeshade.queue_disk_mb` | 4096 | Capture data kept on disk past the memory queue (`timeshade-queue/` in the controller's database directory), also across controller restarts; past it the oldest is dropped |

Changing any of the first three restarts the sidecar.

### Instance config

| Field | Default | Meaning |
|---|---|---|
| `timeshade.capture_rails` | false | Also record rail entities |
| `timeshade.extra_forces` | "" | Comma-separated forces to capture besides those with players, e.g. `player` on a server nobody has joined yet |

### Permission

`timeshade.view` (granted to the default role) is needed for the Map page, for view tokens and
for every API request through the proxy.

## How the pieces behave

- **Sidecar.** The sidecar listens on a random loopback port with a random ingest token and
  read token, new at every controller start. Only the controller knows the tokens. Its first
  stdout line names the port. If it exits unasked, it is restarted after 1 s, and the delay
  doubles up to 60 s. A run of a minute or more resets the delay. A process that does not
  print its address within 30 s is killed. On controller shutdown, queued data gets up to 3 s
  to go out (what is left stays on disk and goes out after the next start), then the sidecar's
  stdin closes (`--exit-on-stdin-close`). It is killed after 10 s.
  If the controller dies, the sidecar exits on its own when its stdin closes.
- **Forwarding.** Bodies are gzip NDJSON of up to 1 MiB, one request at a time, in order.
  Network errors, 5xx and 429 are retried with backoff and `Retry-After`; ingest is idempotent.
  A 400 or 413 splits the batch, and a single refused line is dropped. Lines the sidecar
  counts as bad are logged (rate-limited). While the sidecar is down the queue holds up to
  `timeshade.queue_mb` in memory and the rest on disk, up to `timeshade.queue_disk_mb`.
  Instances send their envelopes as requests, one at a time: Clusterio drops an event it
  cannot forward (the controller is away or came back with a new session) without telling
  the sender, while a request fails. What the controller did not acknowledge stays queued,
  up to 32 MiB in memory and the rest on disk (`timeshade-pending/` in the instance
  directory, up to 4 GiB, also across host restarts), and is sent again; ingest skips what
  arrives twice. A request that reaches the controller after its shutdown closed the queue
  fails as well. After a failed request the next try waits 2 s, doubling up to 15 s (the host
  logs every message it cannot deliver), until the controller link connects or resumes. When
  the instance stops, what waits goes as events (its link routes no responses any more), or
  stays on disk for the next start while the controller is away. Each line keeps the time it
  arrived at the instance, so a backlog lands where it belongs (docs/v3/API.md 5,
  "Horizon"). A controller crash (not a stop) loses the acknowledged lines it had not yet
  handed to the map server or written to disk: its memory queue.
- **Caught up.** An instance sends a `caught_up` envelope for each of its maps that had no
  envelope for 5 s, after everything that waits, also while its other map is busy, and
  `ended` ones when it stops, so the map server holds a map's horizon back only for an
  instance that still has data of that map to send (docs/v3/PROTOCOL.md 1).
- **Auth.**
  - Tokens are accepted as `X-Access-Token`, `Authorization: Bearer` or `?token=` (the
    viewer's live events use `?token=`).
  - A Clusterio user token works. So does a view token from the web page: audience
    `timeshade.view`, 24 h, signed with the controller's secret. Clusterio's own routes refuse
    view tokens.
  - Both kinds of token stop working when the user's tokens are revoked or the user is gone.
  - The client's token is never forwarded. The proxy sends the sidecar's read token instead.
  - The proxy forwards only GET and HEAD, only the viewer files and `api/v3/*`. It does not
    forward `ingest`, `/metrics` or `/healthz`.
  - Responses from the API are marked `private`, and every response gets `no-transform`, so
    the controller's compression leaves the sidecar's zstd or gzip bodies and the live event
    stream alone.
- **Web page.**
  - The page asks for a view token over the control link (`ViewTokenRequest`).
  - It opens `<web root>api/timeshade/?data=<absolute api base>&token=<view token>` in a
    frame. The token is renewed 10 minutes before it expires, which reloads the frame.
  - **Open in a new tab** opens the same URL.

## Development

```sh
cd clusterio
npm install
npm run build              # module/ (from ../capture), dist/node, dist/web
npm run build:binary -- --web   # bin/timeshadedb(.exe) with the viewer built for sub-paths
npm test
```

Tests (`node --test`):

- Clusterio fakes for the instance and controller contexts.
- A fake sidecar process, used for the lifecycle tests: spawn, ready, crash and restart,
  backoff, the ready timeout, a missing binary, stop and kill.
- An express app with Clusterio's compression middleware, used for the proxy and auth tests.
- An end-to-end test with the real binary. Three plugin-level instances feed one controller
  plugin: two gridworld tiles and a plain instance replaying a real capture. A train crossing
  between the tiles comes out as one journey with a handoff, decoded from the MVW3 window. The
  test is skipped without `bin/timeshadedb(.exe)` or `TIMESHADEDB_BINARY`.

Checked by hand on 2026-10-19 with a real cluster: Clusterio 2.0.2 installed from npm, the plugin
installed from its `npm pack` tarball, one host and two instances on Factorio 2.1.19 headless.
`ts_rt.seam` stood in for charting, and a car was spawned over RCON. Results:

- The module was patched in.
- The RCON config and resync ran, with `timeshade.extra_forces=player` taken from the instance
  config.
- Terrain and movers of both instances reached the catalog.
- View, tile and live requests worked through the proxy.
- The proxy answered 401 without a token or with a bad one, and 403 for a user without roles.
  A revoked token stopped working.
- A killed sidecar was restarted and kept storing data.
- Controller shutdown stopped the sidecar.

## Not done yet

- **Stopped instances.** When an instance stops, its players are not ended. The capture sends
  gones at a clean stop, but not after a crash. Ingest's `Journeys.Stopped` has no protocol
  message.
- **Universal Edges lines** are drawn on the world's map only for edge sides on an instance's
  `tile.surface`. The controller does not know the instance's other tiled surfaces.
- **S3 publishing.** The plugin does not run `publish --s3` (WP12).
- **Binary packages.** The `@timeshadedb/bin-*` packages and the plugin are not published. The
  scope name is a placeholder.

## Licence

The files with an `SPDX-License-Identifier: BlueOak-1.0.0` header are ported in part from
[illisis/minimap](https://github.com/illisis/minimap) and stay under the Blue Oak Model
License 1.0.0 ([LICENSE-BlueOak-1.0.0.md](LICENSE-BlueOak-1.0.0.md)). The module copied into
`module/` carries the same notice.
