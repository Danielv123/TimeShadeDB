// Reads the railbench recorder's ground truth (mods/railbench_recorder/control.lua describes the format) into per-train
// arrays keyed by tick, and the rail dump.
import fs from "node:fs";
import path from "node:path";

/** A growable Int32 array. */
class IntList {
	constructor(cap = 1024) {
		this.a = new Int32Array(cap);
		this.n = 0;
	}
	push(v) {
		if (this.n === this.a.length) {
			const b = new Int32Array(this.a.length * 2);
			b.set(this.a);
			this.a = b;
		}
		this.a[this.n++] = v;
	}
	view() {
		return this.a.subarray(0, this.n);
	}
}

/**
 * One train's recorded lines. ticks[i], speed[i] (1e-5 tiles a tick), state[i], rail[i] (unit number under the end it
 * moves towards); cars[i * stride ...] = x, y (1/256 tile), o (1/65536 turn) per carriage from the train's front.
 * A train whose carriage count changes gets a new segment (a new TrainRec with the same id and a later first tick).
 */
export class TrainRec {
	constructor(id, surface, n, kinds) {
		this.id = id;
		this.surface = surface;
		this.n = n;
		this.kinds = kinds;
		this.stride = 3 * n;
		this._ticks = new IntList();
		this._speed = new IntList();
		this._state = new IntList();
		this._rail = new IntList();
		this._cars = new IntList(1024 * this.stride);
		this.gone = null;
	}
	finish() {
		this.ticks = this._ticks.view();
		this.speed = this._speed.view();
		this.state = this._state.view();
		this.rail = this._rail.view();
		this.cars = this._cars.view();
		delete this._ticks;
		delete this._speed;
		delete this._state;
		delete this._rail;
		delete this._cars;
	}
	/** Index of the last line at or before tick, or -1. */
	at(tick) {
		const t = this.ticks;
		let lo = 0;
		let hi = t.length - 1;
		if (hi < 0 || t[0] > tick) {
			return -1;
		}
		while (lo < hi) {
			const mid = (lo + hi + 1) >> 1;
			if (t[mid] <= tick) {
				lo = mid;
			} else {
				hi = mid - 1;
			}
		}
		return lo;
	}
}

/** Parses every gt-*.txt of dir. Returns { version, tick0, states, surfaces, rails, trains: TrainRec[], lastTick }. */
export function readGroundTruth(dir) {
	const files = fs.readdirSync(dir).filter(f => /^gt-\d+\.txt$/.test(f)).sort();
	const res = { version: null, tick0: null, states: {}, surfaces: {}, rails: [], trains: [], lastTick: null };
	const open = new Map(); // id -> current TrainRec
	const prev = new Map(); // id -> last absolute carriage values (Int32Array)
	let tick = null;
	let curSurface = 0;
	for (const f of files) {
		const text = fs.readFileSync(path.join(dir, f), "latin1");
		let pos = 0;
		while (pos < text.length) {
			let end = text.indexOf("\n", pos);
			if (end < 0) {
				end = text.length;
			}
			const line = text.slice(pos, end);
			pos = end + 1;
			if (line.length === 0) {
				continue;
			}
			const c = line.charCodeAt(0);
			const c1 = line.charCodeAt(1);
			if ((c === 68 /* D */ || c === 65 /* A */) && c1 === 32) {
				const p = line.split(" ");
				const id = Number(p[1]);
				const tr = open.get(id);
				if (!tr) {
					continue;
				}
				tr._ticks.push(tick);
				tr._speed.push(Number(p[2]));
				tr._state.push(Number(p[3]));
				let rail = p[4] === "-" ? (tr._rail.n ? tr._rail.a[tr._rail.n - 1] : 0) : Number(p[4]);
				tr._rail.push(rail);
				let pv = prev.get(id);
				const n = (p.length - 5) / 3;
				if (n !== tr.n) {
					throw new Error(`${f}: train ${id} line with ${n} carriages, expected ${tr.n}`);
				}
				if (c === 65 || !pv) {
					pv = new Int32Array(3 * n);
					for (let i = 0; i < 3 * n; i++) {
						pv[i] = Number(p[5 + i]);
					}
				} else {
					for (let i = 0; i < 3 * n; i++) {
						pv[i] += Number(p[5 + i]);
					}
					for (let i = 2; i < 3 * n; i += 3) {
						pv[i] = ((pv[i] % 65536) + 65536) % 65536;
					}
				}
				prev.set(id, pv);
				for (let i = 0; i < 3 * n; i++) {
					tr._cars.push(pv[i]);
				}
			} else if (c === 84 /* T */ && c1 === 32) {
				tick = Number(line.slice(2));
				res.lastTick = tick;
			} else if (line.startsWith("TRAIN ")) {
				const [, id, surface, n, kinds, manual] = line.split(" ");
				const tr = new TrainRec(Number(id), Number(surface), Number(n), kinds);
				tr.manual = manual === "1";
				tr.firstTick = tick;
				const old = open.get(tr.id);
				if (old) {
					old.gone = tick;
				}
				open.set(tr.id, tr);
				prev.delete(tr.id);
				res.trains.push(tr);
			} else if (line.startsWith("GONE ")) {
				const id = Number(line.slice(5));
				const tr = open.get(id);
				if (tr) {
					tr.gone = tick;
				}
				open.delete(id);
				prev.delete(id);
			} else if (line.startsWith("RAIL ")) {
				const p = line.split(" ");
				res.rails.push({
					surface: Number(p[1]), unit: Number(p[2]), type: p[3], name: p[4], x: Number(p[5]) / 256, y: Number(p[6]) / 256,
					dir: Number(p[7]), layer: Number(p[8]), length: Number(p[9]) / 1000,
					ends: [
						{ x: Number(p[10]) / 256, y: Number(p[11]) / 256, dir: Number(p[12]), conn: Number(p[16]) },
						{ x: Number(p[13]) / 256, y: Number(p[14]) / 256, dir: Number(p[15]), conn: Number(p[17]) },
					],
				});
			} else if (line.startsWith("SURF ")) {
				const [, idx, ...name] = line.split(" ");
				curSurface = Number(idx);
				res.surfaces[curSurface] = name.join(" ");
			} else if (line.startsWith("STATE ")) {
				for (const kv of line.slice(6).split(" ")) {
					const [v, k] = kv.split("=");
					res.states[Number(v)] = k;
				}
			} else if (line.startsWith("RBGT ")) {
				const p = line.split(" ");
				res.version = p[2];
				res.tick0 = Number(p[3]);
			}
		}
	}
	for (const tr of res.trains) {
		tr.finish();
	}
	return res;
}

/** Carriage i of line k: { x, y, o } in tiles and turns. */
export function carAt(tr, k, i, out = {}) {
	const b = k * tr.stride + 3 * i;
	out.x = tr.cars[b] / 256;
	out.y = tr.cars[b + 1] / 256;
	out.o = tr.cars[b + 2] / 65536;
	return out;
}
