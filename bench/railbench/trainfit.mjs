// Fits a recorded train line to the rail model (docs/v3/RAIL-GEOMETRY.md section 3): the rail path under the train and
// each carriage's front joint as an arc length on it. geometry.mjs measures the model with it and gen-vectors.mjs takes
// real-game placement cases from it.
import { chainPos } from "./railnet.mjs";
import { pathPoint } from "./railgeom.mjs";

const J = 2;

/**
 * Carriage centres and axes of line k of train tr, the axes turned towards the train's front (for a single carriage:
 * towards its direction of travel). Returns { cx, cy, ux, uy } (Float64Arrays) in the train's carriage order.
 */
export function carriageAxes(tr, k, out = null) {
	const n = tr.n;
	const base = k * tr.stride;
	const fwd = tr.speed[k] >= 0;
	const o2 = out ?? { cx: new Float64Array(n), cy: new Float64Array(n), ux: new Float64Array(n), uy: new Float64Array(n) };
	const { cx, cy, ux, uy } = o2;
	for (let i = 0; i < n; i++) {
		cx[i] = tr.cars[base + 3 * i] / 256;
		cy[i] = tr.cars[base + 3 * i + 1] / 256;
		const o = (tr.cars[base + 3 * i + 2] / 65536) * 2 * Math.PI;
		ux[i] = Math.sin(o);
		uy[i] = -Math.cos(o);
	}
	for (let i = 0; i < n; i++) {
		let dx;
		let dy;
		if (n === 1) {
			dx = fwd ? ux[i] : -ux[i];
			dy = fwd ? uy[i] : -uy[i];
		} else if (i > 0) {
			dx = cx[i - 1] - cx[i];
			dy = cy[i - 1] - cy[i];
		} else {
			dx = cx[0] - cx[1];
			dy = cy[0] - cy[1];
		}
		if (ux[i] * dx + uy[i] * dy < 0) {
			ux[i] = -ux[i];
			uy[i] = -uy[i];
		}
	}
	return o2;
}

/**
 * The rail path under line k of train tr and every carriage's front joint on it. The joints are first guessed at
 * centre +- 2 along the carriage axis and projected on the pieces under them; the pieces are chained from the rear to
 * the front; then each front joint is moved along the path until the model's carriage centre (the midpoint of the
 * joints at U and U - 4) lies abreast of the recorded one. Returns null when a joint is on no piece or the pieces do
 * not chain, else { path: [{ type, x, y, dir, rev }], pieces: [rail record], Uf: Float64Array, residual: Float64Array
 * (what is left across the track per carriage), front: [projection of each carriage's guessed front joint], axes }.
 */
export function fitTrain(net, tr, k, axes = null) {
	const n = tr.n;
	const ax = carriageAxes(tr, k, axes);
	const { cx, cy, ux, uy } = ax;
	const proj = [];
	for (let i = 0; i < n; i++) {
		for (const sgn of [1, -1]) {
			const c = net.near(tr.surface, cx[i] + sgn * J * ux[i], cy[i] + sgn * J * uy[i], ux[i], uy[i], 0.3, 0.9);
			if (!c.length) {
				return null;
			}
			proj.push(c[0]);
		}
	}
	let chain = [];
	for (let j = proj.length - 1; j > 0; j--) {
		const a = proj[j].r;
		const b = proj[j - 1].r;
		let seg;
		if (a === b) {
			if (chain.length) {
				continue;
			}
			// both on one piece: the order of s gives the direction
			seg = [{ r: a, rev: proj[j - 1].s < proj[j].s }];
		} else {
			seg = net.chain(a, b, 4);
		}
		if (!seg) {
			return null;
		}
		if (chain.length) {
			const last = chain[chain.length - 1];
			if (last.r !== seg[0].r || last.rev !== seg[0].rev) {
				return null;
			}
			chain = chain.concat(seg.slice(1));
		} else {
			chain = seg;
		}
	}
	if (!chain.length) {
		return null;
	}
	const idx = new Map(chain.map((c, i) => [c.r, i]));
	const path = chain.map(c => ({ type: c.r.type, x: c.r.x, y: c.r.y, dir: c.r.dir, rev: c.rev }));
	const Uf = new Float64Array(n);
	const residual = new Float64Array(n);
	for (let i = 0; i < n; i++) {
		let u = chainPos(chain, idx.get(proj[2 * i].r), proj[2 * i].s);
		for (let it = 0; it < 6; it++) {
			const a = pathPoint(path, u);
			const b = pathPoint(path, u - 4);
			const d = ((a.x + b.x) / 2 - cx[i]) * ux[i] + ((a.y + b.y) / 2 - cy[i]) * uy[i];
			u -= d;
			if (Math.abs(d) < 1e-7) {
				break;
			}
		}
		Uf[i] = u;
		const a = pathPoint(path, u);
		const b = pathPoint(path, u - 4);
		residual[i] = Math.hypot((a.x + b.x) / 2 - cx[i], (a.y + b.y) / 2 - cy[i]);
	}
	return { path, pieces: chain.map(c => c.r), Uf, residual, front: proj.filter((_, i) => i % 2 === 0), back: proj.filter((_, i) => i % 2 === 1), axes: ax };
}
