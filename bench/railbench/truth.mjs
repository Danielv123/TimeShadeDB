// Ground truth per tick for railbench: the leading end, stop/start spans and the track kind of each recorded line.

export const KINDS = ["straight", "diagonal", "curve", "junction", "stopstart", "unknown"];
export const K = Object.fromEntries(KINDS.map((k, i) => [k, i]));
// a moving tick within this many ticks of the train standing counts as stop/start (eval.mjs and simulate.mjs take
// --stopstart-ticks; 0 splits by the track alone)
export const STOPSTART_TICKS = 120;

/** Per line of a train: direction it leads with (+1 front, -1 back), stop/start flag, track kind (lazily). */
export function prepareTrain(tr, rails, stopTicks = STOPSTART_TICKS) {
	const n = tr.ticks.length;
	tr.dir = new Int8Array(n);
	let d = 1;
	for (let k = 0; k < n; k++) {
		if (tr.speed[k] > 0) {
			d = 1;
		} else if (tr.speed[k] < 0) {
			d = -1;
		}
		tr.dir[k] = d;
	}
	// moving at line k: speed != 0. Ticks without a line are standing ticks (the recorder writes every moving tick).
	// near a stop: within stopTicks of a standing tick
	tr.near = new Uint8Array(n);
	let lastStand = -Infinity;
	for (let k = 0; k < n; k++) {
		if (tr.speed[k] === 0 || (k > 0 && tr.ticks[k] - tr.ticks[k - 1] > 1)) {
			lastStand = tr.speed[k] === 0 ? tr.ticks[k] : tr.ticks[k] - 1;
		}
		if (tr.ticks[k] - lastStand <= stopTicks) {
			tr.near[k] = 1;
		}
	}
	let nextStand = Infinity;
	for (let k = n - 1; k >= 0; k--) {
		if (tr.speed[k] === 0 || (k + 1 < n && tr.ticks[k + 1] - tr.ticks[k] > 1) || k === n - 1) {
			nextStand = tr.speed[k] === 0 ? tr.ticks[k] : tr.ticks[k] + 1;
		}
		if (nextStand - tr.ticks[k] <= stopTicks) {
			tr.near[k] = 1;
		}
	}
	tr.kind = new Uint8Array(n).fill(255);
	tr.kindOf = (k) => {
		if (tr.kind[k] === 255) {
			let kind;
			if (tr.speed[k] === 0 || (stopTicks > 0 && tr.near[k])) {
				kind = K.stopstart;
			} else {
				const h = head(tr, k);
				kind = K[rails.kindAt(tr.surface, h.x, h.y, h.hx, h.hy).kind];
			}
			tr.kind[k] = kind;
		}
		return tr.kind[k];
	};
}

/** The leading carriage of line k (centre, its axis as a unit vector towards the leading end). */
export function head(tr, k) {
	const i = tr.dir[k] > 0 ? 0 : tr.n - 1;
	const b = k * tr.stride + 3 * i;
	const o = tr.cars[b + 2] / 65536;
	const a = 2 * Math.PI * o;
	return { x: tr.cars[b] / 256, y: tr.cars[b + 1] / 256, hx: Math.sin(a), hy: -Math.cos(a) };
}

/**
 * Ground truth at fractional tick tau: every carriage from the leading end as [x, y, o] (o in turns), the head speed in
 * tiles a tick, the line index, or null before the train's first line.
 */
export function truthAt(tr, tau, out) {
	const t = Math.floor(tau);
	const k = tr.at(t);
	if (k < 0) {
		return null;
	}
	const f = tau - t;
	const k2 = k + 1 < tr.ticks.length && tr.ticks[k + 1] === t + 1 && tr.ticks[k] === t && tr.dir[k + 1] === tr.dir[k] ? k + 1 : k;
	const standing = tr.ticks[k] !== t; // no line at this tick: the train stands where its last line put it
	const w = k2 === k ? 0 : f;
	const fwd = tr.dir[k] > 0;
	out.n = tr.n;
	for (let j = 0; j < tr.n; j++) {
		const i = fwd ? j : tr.n - 1 - j;
		const b = k * tr.stride + 3 * i;
		const b2 = k2 * tr.stride + 3 * i;
		let o1 = tr.cars[b + 2] / 65536;
		let o2 = tr.cars[b2 + 2] / 65536;
		let d = o2 - o1;
		d -= Math.round(d);
		out.c[3 * j] = (tr.cars[b] + (tr.cars[b2] - tr.cars[b]) * w) / 256;
		out.c[3 * j + 1] = (tr.cars[b + 1] + (tr.cars[b2 + 1] - tr.cars[b + 1]) * w) / 256;
		out.c[3 * j + 2] = (((o1 + d * w) % 1) + 1) % 1;
	}
	out.speed = standing ? 0 : Math.abs(tr.speed[k]) / 1e5;
	out.k = k;
	out.moving = !standing && tr.speed[k] !== 0;
	return out;
}
