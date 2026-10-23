// What the instance does to Lua's envelopes before they leave (docs/v3/PROTOCOL.md 1 and 2): check their shape, take
// out trace lines, and split each one by map. An envelope carries one `map`, but one instance can feed two maps: a
// gridworld tile's tiled surfaces belong to its world (`gw-<world>`), its other surfaces (space platforms, say) to the
// instance itself (`i-<id>`). Which surfaces are tiled comes from Lua's surfaces messages, in order with the rest.
import { instanceMap, mapOf, worldMap, type Tile } from "./integrations.js";
import type { Envelope } from "./messages.js";

/** Lua's part of an envelope. */
export type LuaEnvelope = { v: number; tick: number; speed?: number; msgs: unknown[] };
export type TraceMsg = { type: "trace"; op: string; phase: string; level?: string; [field: string]: unknown };
type Msg = { type: string; s?: unknown; [field: string]: unknown };

/** Why a line is not a usable envelope, or undefined when it is one. */
export function envelopeError(data: unknown): string | undefined {
	if (!data || typeof data !== "object" || Array.isArray(data)) {
		return "not an object";
	}
	const e = data as Partial<LuaEnvelope>;
	if (typeof e.v !== "number" || !Number.isInteger(e.v)) {
		return "bad v";
	}
	if (typeof e.tick !== "number" || !Number.isInteger(e.tick) || e.tick < 0) {
		return "bad tick";
	}
	if (e.speed !== undefined && (typeof e.speed !== "number" || !(e.speed > 0))) {
		return "bad speed";
	}
	if (!Array.isArray(e.msgs)) {
		return "no msgs";
	}
	return undefined;
}

export type Split = {
	/** One envelope per map, in the order the maps first appear. */
	envelopes: Envelope[];
	traces: TraceMsg[];
	/** Messages that were not objects with a string type (dropped). */
	invalid: number;
	/** The resynced messages (also forwarded), for the log. */
	resynced: Msg[];
};

/**
 * Splits one instance's envelopes by map. It keeps the tiled-surface set from the surfaces messages, so it must see
 * every envelope of the instance in order.
 */
export class Splitter {
	private tile: Tile | null = null;
	private tiled = new Set<string>();

	constructor(public readonly instanceId: number) { }

	/** The instance's gridworld tile (null: not a tile). Called at every configure. */
	setTile(tile: Tile | null): void {
		const world = this.tile?.world;
		this.tile = tile;
		if (tile?.world !== world) {
			this.tiled.clear();
		}
	}

	/** The maps this instance feeds: its own, and its world's when it is a tile. */
	maps(): string[] {
		const own = instanceMap(this.instanceId);
		return this.tile ? [worldMap(this.tile.world), own] : [own];
	}

	mapOfSurface(surface: string): string {
		return mapOf({ id: this.instanceId, tile: this.tile }, surface, this.tiled);
	}

	/**
	 * `env` must have passed envelopeError. Messages with a surface go to that surface's map; surfaces, resynced, adopt
	 * and any other message without a surface go to every map of the instance (a resync starts and ends on each, and
	 * an adopted journey may be on either); trace messages are returned, not forwarded.
	 */
	split(env: LuaEnvelope, recvMs: number, nextSeq: () => number): Split {
		const out = new Map<string, unknown[]>();
		const traces: TraceMsg[] = [];
		const resynced: Msg[] = [];
		let invalid = 0;
		const add = (map: string, msg: unknown) => {
			let list = out.get(map);
			if (!list) {
				list = [];
				out.set(map, list);
			}
			list.push(msg);
		};
		for (const raw of env.msgs) {
			if (!raw || typeof raw !== "object" || Array.isArray(raw) || typeof (raw as Msg).type !== "string") {
				invalid += 1;
				continue;
			}
			const msg = raw as Msg;
			if (msg.type === "trace") {
				traces.push(msg as TraceMsg);
				continue;
			}
			if (msg.type === "surfaces") {
				this.surfaces(msg);
			} else if (msg.type === "resynced") {
				resynced.push(msg);
			}
			if (typeof msg.s === "string") {
				add(this.mapOfSurface(msg.s), msg);
			} else {
				for (const map of this.maps()) {
					add(map, msg);
				}
			}
		}
		const envelopes: Envelope[] = [];
		for (const [map, msgs] of out) {
			const e: Envelope = { v: env.v, tick: env.tick, msgs, map, recv_ms: recvMs, seq: nextSeq() };
			if (env.speed !== undefined) {
				e.speed = env.speed;
			}
			envelopes.push(e);
		}
		return { envelopes, traces, invalid, resynced };
	}

	/** Each entry's flag replaces the last: a long list comes in parts, and a surface made mid-game comes alone. */
	private surfaces(msg: Msg): void {
		const list = Array.isArray(msg.list) ? msg.list : [];
		for (const entry of list) {
			const e = entry as { name?: unknown; tile?: unknown } | null;
			if (!e || typeof e.name !== "string") {
				continue;
			}
			if (e.tile === true) {
				this.tiled.add(e.name);
			} else {
				this.tiled.delete(e.name);
			}
		}
	}
}
