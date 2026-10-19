// SPDX-License-Identifier: BlueOak-1.0.0
// Ported from illisis/minimap instance.ts (Blue Oak Model License 1.0.0, see LICENSE-BlueOak-1.0.0.md).
//
// The instance side (PLAN.md §4.10): Lua's capture module prints envelopes on the IPC channel timeshade:out; this
// stamps each with its arrival time, splits it by map, and sends what arrived to the controller about once a second.
// It configures the capture over RCON at every start, on a gridworld.tile change and on a new controller session.
import * as lib from "@clusterio/lib";
import type { InstancePluginContext } from "@clusterio/host";
import { envelopeError, Splitter, type LuaEnvelope, type TraceMsg } from "./forward.js";
import { tileOf } from "./integrations.js";
import { EnvelopesEvent, type Envelope } from "./messages.js";
import { lifecycle, newId, rateLimited, type Level } from "./runtime.js";

/** Envelopes go to the controller once a second. */
export const BATCH_MS = 1000;
/** One link event carries at most about this many bytes of envelopes. */
export const MAX_EVENT_BYTES = 1 << 20;
/** Envelopes waiting for the next send; past it the oldest are dropped (the controller link is down for long). */
export const MAX_PENDING_BYTES = 32 << 20;

export type InstanceOptions = {
	/** The wall clock stamped as recv_ms (tests). */
	now?: () => number;
};

const LEVELS = new Set(["info", "verbose", "warn", "error"]);

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
		for (const env of split.envelopes) {
			const bytes = JSON.stringify(env).length;
			pending.push({ env, bytes });
			pendingBytes += bytes;
		}
		let dropped = 0;
		while (pendingBytes > MAX_PENDING_BYTES && pending.length > 1) {
			pendingBytes -= pending.shift()!.bytes;
			dropped += 1;
		}
		if (dropped > 0) {
			warn("dropped", "timeshade.forward dropped", { op: "timeshade.forward", phase: "dropped", envelopes: dropped });
		}
	}

	function flush() {
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
				// Not connected: keep them for the next try (bounded by MAX_PENDING_BYTES).
				warn("send", "timeshade.forward send failed", { op: "timeshade.forward", phase: "send_failed", error: err?.message });
				return;
			}
			pending = pending.slice(n);
			pendingBytes -= bytes;
		}
	}

	// Clusterio parses each IPC line and calls the handler without awaiting in between, and receive awaits nothing,
	// so lines are handled in arrival order.
	instance.server.handle("timeshade:out", async (data: unknown) => receive(data));
	const timer = setInterval(flush, BATCH_MS);
	instance.hooks.exit.attach(plugin.name, () => {
		clearInterval(timer);
		flush();
	});

	/**
	 * The config, then a resync, in one /sc so they arrive in order. Lua checks every field; a refusal comes back as
	 * RCON output, which sendRcon(..., true) throws and the hook logs.
	 */
	async function configure(reason: string) {
		const trace = newId();
		const tile = tileOf(instance.config as unknown as { get(name: string): unknown });
		splitter.setTile(tile);
		let rails = false;
		try {
			rails = Boolean(instance.config.get("timeshade.capture_rails"));
		} catch {
			// A controller without the field (an older plugin version): the default.
		}
		const config: Record<string, unknown> = { rails };
		if (tile) {
			config.tile = tile;
		}
		lifecycle(logger, "timeshade.resync", "start", { trace, reason }, "verbose");
		const json = lib.escapeString(JSON.stringify(config));
		await instance.sendRcon(`/sc ts_rt.config("${json}") ts_rt.resync("${trace}")`, true, plugin.name);
	}

	instance.hooks.start.attach(plugin.name, () => configure("start"));
	instance.hooks.instanceConfigFieldChanged.attach(plugin.name, async (field: string) => {
		if ((field === "gridworld.tile" || field === "timeshade.capture_rails") && instance.status === "running") {
			await configure(field === "gridworld.tile" ? "tile" : "config");
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
