// SPDX-License-Identifier: BlueOak-1.0.0
// Ported from illisis/minimap instance.ts (Blue Oak Model License 1.0.0, see LICENSE-BlueOak-1.0.0.md).
//
// The instance side (PLAN.md §4.10): Lua's capture module prints envelopes on the IPC channel timeshade:out; this
// stamps each with its arrival time, splits it by map, and sends what arrived to the controller about once a second.
// While nothing waits it sends caught_up envelopes every few seconds, and ended ones when the instance stops, so the map
// server holds a map's horizon back only for an instance that still has data to send (docs/API.md 5, "Horizon").
// It configures the capture over RCON at every start, on a gridworld.tile change and on a new controller session.
import * as lib from "@clusterio/lib";
import type { InstancePluginContext } from "@clusterio/host";
import { envelopeError, Splitter, type LuaEnvelope, type TraceMsg } from "./forward.js";
import { tileOf } from "./integrations.js";
import { EnvelopesEvent, type Envelope } from "./messages.js";
import { lifecycle, newId, rateLimited, type Level } from "./runtime.js";
import { DiskTail } from "./spool.js";

/** Envelopes go to the controller once a second. */
export const BATCH_MS = 1000;
/** One link event carries at most about this many bytes of envelopes. */
export const MAX_EVENT_BYTES = 1 << 20;
/**
 * Envelopes waiting for the next send in memory; past it they wait on disk (timeshade-pending/ in the instance
 * directory, kept across host restarts) while the controller link is down, up to MAX_PENDING_DISK_BYTES, past which
 * the oldest are dropped. Without a directory (tests) the oldest are dropped past MAX_PENDING_BYTES.
 */
export const MAX_PENDING_BYTES = 32 << 20;
export const MAX_PENDING_DISK_BYTES = 4 * 1024 ** 3;
/** While nothing waits, a caught_up envelope per map this often. */
export const HEARTBEAT_MS = 5000;

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
	// Increasing per source (docs/PROTOCOL.md 1), also across host restarts.
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
	/** The newest tick Lua sent (-1: none yet), and when envelopes last left. */
	let lastTick = -1;
	let lastSent = now();

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

	/** caught_up envelopes for every map of the instance: nothing older waits (ended: the instance stopped). */
	function caughtUp(ended: boolean) {
		const envs: Envelope[] = [];
		for (const map of splitter.maps()) {
			const env: Envelope = { v: 1, tick: Math.max(lastTick, 0), msgs: [], map, recv_ms: now(), seq: nextSeq(), caught_up: true };
			if (ended) {
				env.ended = true;
			}
			envs.push(env);
		}
		enqueue(envs);
	}

	function flush() {
		refill();
		if (pending.length === 0 && (!disk || disk.empty) && lastTick >= 0 && now() - lastSent >= HEARTBEAT_MS) {
			caughtUp(false);
		}
		while (pending.length > 0) {
			const batch: Envelope[] = [];
			let bytes = 0;
			let n = 0;
			while (n < pending.length && (batch.length === 0 || bytes + pending[n].bytes <= MAX_EVENT_BYTES)) {
				batch.push(pending[n].env);
				bytes += pending[n].bytes;
				n += 1;
			}
			try {
				instance.sendTo("controller", new EnvelopesEvent(batch));
			} catch (err: any) {
				// Not connected: keep them for the next try (in memory, then on disk).
				warn("send", "timeshade.forward send failed", { op: "timeshade.forward", phase: "send_failed", error: err?.message });
				return;
			}
			pending = pending.slice(n);
			pendingBytes -= bytes;
			lastSent = now();
			if (pending.length === 0 && fromSeg !== undefined) {
				disk!.done(fromSeg);
				fromSeg = undefined;
				refill();
			}
		}
	}

	// Clusterio parses each IPC line and calls the handler without awaiting in between, and receive awaits nothing,
	// so lines are handled in arrival order.
	instance.server.handle("timeshade:out", async (data: unknown) => receive(data));
	const timer = setInterval(flush, options.batchMs ?? BATCH_MS);
	if (disk && !disk.empty) {
		lifecycle(logger, "timeshade.forward", "spool_loaded", { envelopes: disk.lines, bytes: disk.bytes });
	}
	instance.hooks.exit.attach(plugin.name, () => {
		clearInterval(timer);
		if (lastTick >= 0) {
			caughtUp(true);
		}
		flush();
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
		if (event === "connect" && instance.status === "running") {
			await configure("reconnect");
		}
	});
}
