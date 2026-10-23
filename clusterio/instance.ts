// SPDX-License-Identifier: BlueOak-1.0.0
// Ported from illisis/minimap instance.ts (Blue Oak Model License 1.0.0, see LICENSE-BlueOak-1.0.0.md).
//
// The instance side (PLAN.md §4.10): Lua's capture module prints envelopes on the IPC channel timeshade:out; this
// stamps each with its arrival time, splits it by map, and sends what arrived to the controller about once a second,
// as a request: what the controller did not take stays queued. A map without envelopes for a few seconds gets a
// caught_up envelope, and every map an ended one when the instance stops, so the map server holds a map's horizon back
// only for an instance that still has data of it to send (docs/v3/API.md 5, "Horizon").
// It configures the capture over RCON at every start, on a gridworld.tile change and on a new controller session.
import * as lib from "@clusterio/lib";
import type { InstancePluginContext } from "@clusterio/host";
import { envelopeError, Splitter, type LuaEnvelope, type TraceMsg } from "./forward.js";
import { tileOf } from "./integrations.js";
import { EnvelopesEvent, EnvelopesRequest, type Envelope } from "./messages.js";
import { lifecycle, newId, rateLimited, type Level } from "./runtime.js";
import { DiskTail } from "./spool.js";

/** Envelopes go to the controller once a second. */
export const BATCH_MS = 1000;
/** One link message carries at most about this many bytes of envelopes. */
export const MAX_EVENT_BYTES = 1 << 20;
/**
 * Envelopes waiting for the next send in memory; past it they wait on disk (timeshade-pending/ in the instance
 * directory, kept across host restarts) while the controller link is down, up to MAX_PENDING_DISK_BYTES, past which
 * the oldest are dropped. Without a directory (tests) the oldest are dropped past MAX_PENDING_BYTES.
 */
export const MAX_PENDING_BYTES = 32 << 20;
export const MAX_PENDING_DISK_BYTES = 4 * 1024 ** 3;
/** A map without envelopes this long gets a caught_up envelope. */
export const HEARTBEAT_MS = 5000;
/** After failed requests the next try waits twice as long each time, up to this; a new or resumed link tries at once. */
export const MAX_RETRY_MS = 15_000;

export type InstanceOptions = {
	/** The wall clock stamped as recv_ms (tests). */
	now?: () => number;
	/** How often envelopes are sent (tests); BATCH_MS by default. */
	batchMs?: number;
	/** Where envelopes past MAX_PENDING_BYTES wait (default timeshade-pending/ in the instance directory). */
	spoolDir?: string;
};

const LEVELS = new Set(["info", "verbose", "warn", "error"]);
const RECONFIGURE = new Set(["gridworld.tile", "timeshade.capture_rails", "timeshade.extra_forces"]);

/** "a, b,,c" as ["a", "b", "c"]. */
export function forceList(value: unknown): string[] {
	return typeof value === "string" ? value.split(",").map(s => s.trim()).filter(Boolean) : [];
}

export default async function loadInstancePlugin(context: InstancePluginContext, options: InstanceOptions = {}) {
	const { instance, logger, plugin } = context;
	const now = options.now ?? Date.now;
	const warn = rateLimited(logger);
	const splitter = new Splitter(instance.id);
	splitter.setTile(tileOf(instance.config as unknown as { get(name: string): unknown }));
	// Increasing per source (docs/v3/PROTOCOL.md 1), also across host restarts.
	let seq = now() * 1000;
	const nextSeq = () => ++seq;
	let pending: { env: Envelope; bytes: number }[] = [];
	let pendingBytes = 0;
	// The disk part of the pending envelopes (spool.ts): newer than the memory part, which is either all received
	// directly or all from the segment fromSeg (deleted once that is sent).
	const instPath = (instance as unknown as { path?: (...parts: string[]) => string }).path;
	const spoolDir = options.spoolDir ?? (typeof instPath === "function" ? instPath.call(instance, "timeshade-pending") : undefined);
	const disk = spoolDir ? new DiskTail(spoolDir, MAX_PENDING_DISK_BYTES) : undefined;
	let fromSeg: number | undefined;
	let diskDropped = 0;
	/** The newest tick Lua sent (-1: none yet). */
	let lastTick = -1;
	/** When an envelope of each map was last queued, and when Lua's first envelope came (for maps without one yet). */
	const queued = new Map<string, number>();
	let firstMs = 0;
	/**
	 * A request is out (one at a time, so the envelopes stay in order); whether the controller link is up as far as
	 * the last request and the connection events tell; the instance stopped.
	 */
	let sending = false;
	let linkUp = true;
	let closed = false;
	/** When the next request may go after failures, and the wait after the next failure. */
	let retryAt = 0;
	let retryMs = 0;

	/** A trace line from Lua, logged; reserved keys and the fields the cluster logger stamps never come from Lua. */
	function log(msg: TraceMsg) {
		const { type, op, phase, level, message, timestamp, instance_id, instance_name, plugin: _p, ...fields } =
			msg as TraceMsg & Record<string, unknown>;
		const lvl = typeof level === "string" && LEVELS.has(level) ? level as Level : "info";
		lifecycle(logger, String(op), String(phase), fields, lvl);
	}

	function receive(data: unknown) {
		const error = envelopeError(data);
		if (error) {
			warn(`envelope:${error}`, "timeshade.ipc invalid", { op: "timeshade.ipc", phase: "invalid", error });
			return;
		}
		if (lastTick < 0) {
			firstMs = now();
		}
		lastTick = Math.max(lastTick, (data as LuaEnvelope).tick);
		const split = splitter.split(data as LuaEnvelope, now(), nextSeq);
		if (split.invalid > 0) {
			warn("message", "timeshade.ipc invalid", { op: "timeshade.ipc", phase: "invalid", error: "bad message", messages: split.invalid });
		}
		for (const trace of split.traces) {
			log(trace);
		}
		for (const r of split.resynced) {
			lifecycle(logger, "timeshade.resync", "done", { trace: r.trace, counts: r.counts }, "verbose");
		}
		enqueue(split.envelopes);
	}

	/** Queues envelopes after everything pending: in memory, or on disk once the memory part is full. */
	function enqueue(envs: Envelope[]) {
		const toDisk: string[] = [];
		for (const env of envs) {
			queued.set(env.map, now());
			const json = JSON.stringify(env);
			if (disk && (toDisk.length > 0 || !disk.empty || fromSeg !== undefined || pendingBytes + json.length > MAX_PENDING_BYTES)) {
				toDisk.push(json);
				continue;
			}
			pending.push({ env, bytes: json.length });
			pendingBytes += json.length;
		}
		let dropped = 0;
		if (disk) {
			disk.append(toDisk);
			dropped = disk.dropped - diskDropped;
			diskDropped = disk.dropped;
		}
		while (!disk && pendingBytes > MAX_PENDING_BYTES && pending.length > 1) {
			pendingBytes -= pending.shift()!.bytes;
			dropped += 1;
		}
		if (dropped > 0) {
			warn("dropped", "timeshade.forward dropped", { op: "timeshade.forward", phase: "dropped", envelopes: dropped });
		}
	}

	/** With the memory part sent, the oldest disk segment becomes the memory part. */
	function refill() {
		if (!disk || pending.length > 0 || fromSeg !== undefined) {
			return;
		}
		const t = disk.take();
		if (!t) {
			return;
		}
		fromSeg = t.seg;
		for (const line of t.lines) {
			try {
				pending.push({ env: JSON.parse(line) as Envelope, bytes: line.length });
				pendingBytes += line.length;
			} catch {
				// a torn last line of a segment written when the host died
			}
		}
		if (pending.length === 0) {
			disk.done(t.seg);
			fromSeg = undefined;
		}
	}

	/**
	 * caught_up envelopes for maps of the instance: nothing older of the map waits (ended: the instance stopped). They
	 * queue after everything that waits, so ingest has every older line of the map when it applies one.
	 */
	function caughtUp(maps: string[], ended: boolean) {
		const envs: Envelope[] = [];
		for (const map of maps) {
			const env: Envelope = { v: 1, tick: Math.max(lastTick, 0), msgs: [], map, recv_ms: now(), seq: nextSeq(), caught_up: true };
			if (ended) {
				env.ended = true;
			}
			envs.push(env);
		}
		enqueue(envs);
	}

	/** caught_up for each map without an envelope for HEARTBEAT_MS (once Lua has sent anything), busy maps or not. */
	function heartbeat() {
		if (lastTick < 0) {
			return;
		}
		const t = now();
		caughtUp(splitter.maps().filter(map => t - (queued.get(map) ?? firstMs) >= HEARTBEAT_MS), false);
	}

	/** The first pending envelopes, at most about MAX_EVENT_BYTES (at least one). */
	function nextBatch(): { batch: Envelope[]; n: number; bytes: number } {
		const batch: Envelope[] = [];
		let bytes = 0;
		let n = 0;
		while (n < pending.length && (batch.length === 0 || bytes + pending[n].bytes <= MAX_EVENT_BYTES)) {
			batch.push(pending[n].env);
			bytes += pending[n].bytes;
			n += 1;
		}
		return { batch, n, bytes };
	}

	/** The first n pending envelopes were delivered. */
	function sent(n: number, bytes: number) {
		pending = pending.slice(n);
		pendingBytes -= bytes;
		if (pending.length === 0 && fromSeg !== undefined) {
			disk!.done(fromSeg);
			fromSeg = undefined;
			refill();
		}
	}

	/**
	 * Sends what waits, one request at a time. A request that fails (the controller is away, or the session it was sent
	 * in ended) leaves its envelopes queued for the next try, in memory and then on disk; a delivered one may come again
	 * after a failure that hid its response, which ingest skips (docs/v3/PROTOCOL.md 1, seq). Tries after failures back
	 * off up to MAX_RETRY_MS (the host logs each failed message) until the link connects or resumes.
	 */
	async function flush() {
		if (sending || closed) {
			return;
		}
		refill();
		heartbeat();
		if (now() < retryAt) {
			return;
		}
		sending = true;
		try {
			while (pending.length > 0) {
				const { batch, n, bytes } = nextBatch();
				try {
					await instance.sendTo("controller", new EnvelopesRequest(batch));
				} catch (err: any) {
					if (!closed) {
						linkUp = false;
						retryMs = Math.min(Math.max(2 * retryMs, 2 * (options.batchMs ?? BATCH_MS)), MAX_RETRY_MS);
						retryAt = now() + retryMs;
						warn("send", "timeshade.forward send failed", { op: "timeshade.forward", phase: "send_failed", error: err?.message });
					}
					return;
				}
				if (closed) {
					return; // the exit hook took over what waits
				}
				linkUp = true;
				retryMs = 0;
				sent(n, bytes);
			}
		} finally {
			sending = false;
		}
	}

	// Clusterio parses each IPC line and calls the handler without awaiting in between, and receive awaits nothing,
	// so lines are handled in arrival order.
	instance.server.handle("timeshade:out", async (data: unknown) => receive(data));
	const timer = setInterval(() => {
		flush().catch(err => logger.error(`timeshade: forwarding failed:\n${err?.stack ?? err}`));
	}, options.batchMs ?? BATCH_MS);
	if (disk && !disk.empty) {
		lifecycle(logger, "timeshade.forward", "spool_loaded", { envelopes: disk.lines, bytes: disk.bytes });
	}
	// The instance's link closes as it stops (Clusterio fails its open requests and routes no more responses to it), so
	// what waits leaves as events, which Clusterio forwards while the controller is connected. While the controller is
	// known to be away it all waits on disk for the next start instead.
	instance.hooks.exit.attach(plugin.name, () => {
		clearInterval(timer);
		closed = true;
		if (lastTick >= 0) {
			caughtUp(splitter.maps(), true);
		}
		if (linkUp || !disk) {
			refill();
			while (pending.length > 0) {
				const { batch, n, bytes } = nextBatch();
				instance.sendTo("controller", new EnvelopesEvent(batch));
				sent(n, bytes);
			}
		}
		if (disk) {
			// What could not be sent waits on disk for the next start (lines of a taken segment are still in its file).
			if (fromSeg === undefined) {
				disk.putBack(pending.map(p => JSON.stringify(p.env)));
			}
			disk.close();
		}
	});

	/** An instance config field, or `fallback` when the controller does not know it (an older plugin version). */
	function field(name: string, fallback: unknown): unknown {
		try {
			return (instance.config as unknown as { get(name: string): unknown }).get(name) ?? fallback;
		} catch {
			return fallback;
		}
	}

	/**
	 * The config, then a resync, in one /sc so they arrive in order. Lua checks every field; a refusal comes back as
	 * RCON output, which sendRcon(..., true) throws and the hook logs.
	 */
	async function configure(reason: string) {
		const trace = newId();
		const tile = tileOf(instance.config as unknown as { get(name: string): unknown });
		splitter.setTile(tile);
		const config: Record<string, unknown> = {
			rails: Boolean(field("timeshade.capture_rails", false)),
			forces: forceList(field("timeshade.extra_forces", "")),
		};
		if (tile) {
			config.tile = tile;
		}
		lifecycle(logger, "timeshade.resync", "start", { trace, reason }, "verbose");
		const json = lib.escapeString(JSON.stringify(config));
		await instance.sendRcon(`/sc ts_rt.config("${json}") ts_rt.resync("${trace}")`, true, plugin.name);
	}

	instance.hooks.start.attach(plugin.name, () => configure("start"));
	instance.hooks.instanceConfigFieldChanged.attach(plugin.name, async (name: string) => {
		if (RECONFIGURE.has(name) && instance.status === "running") {
			await configure(name === "gridworld.tile" ? "tile" : "config");
		}
	});
	// A new session means the controller restarted and may have lost what it had not forwarded; a resumed session lost
	// nothing (Clusterio keeps unacknowledged messages), so only "connect" resyncs.
	instance.hooks.controllerConnectionEvent.attach(plugin.name, async (event: string) => {
		linkUp = event === "connect" || event === "resume";
		if (linkUp) {
			retryAt = 0;
			retryMs = 0;
		}
		if (event === "connect" && instance.status === "running") {
			await configure("reconnect");
		}
	});
}
