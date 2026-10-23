// Fixed-radius line/arc paths between two poses (Dubins CSC paths), the reconstruction railbench tests for train
// samples: rails are straights joined by circular arcs of one radius (docs/v3/RAILS.md), so between two samples a
// train follows arc-line-arc with that radius (either arc or the line may be empty).
//
// Coordinates are Factorio's (x east, y south); headings are Factorio orientations in radians (0 north, pi/2 east), so
// the direction of heading th is (sin th, -cos th). A path is a list of segments { kind: "L"|"C", ... } with an
// arc-length table.

const TAU = 2 * Math.PI;
const mod2pi = (a) => ((a % TAU) + TAU) % TAU;

/** The CSC path of turning radius R from pose a = { x, y, h } to pose b, the shortest of LSL, RSR, LSR, RSL, or null. */
export function dubins(a, b, R) {
	// to the standard frame: X = x, Y = -y, math angle phi = pi/2 - h
	const ax = a.x;
	const ay = -a.y;
	const bx = b.x;
	const by = -b.y;
	const pa = Math.PI / 2 - a.h;
	const pb = Math.PI / 2 - b.h;
	const dx = bx - ax;
	const dy = by - ay;
	const D = Math.hypot(dx, dy);
	const d = D / R;
	const th = Math.atan2(dy, dx);
	const al = mod2pi(pa - th);
	const be = mod2pi(pb - th);
	const sa = Math.sin(al);
	const sb = Math.sin(be);
	const ca = Math.cos(al);
	const cb = Math.cos(be);
	const cab = Math.cos(al - be);
	const cands = [];
	// LSL
	{
		const p2 = 2 + d * d - 2 * cab + 2 * d * (sa - sb);
		if (p2 >= 0) {
			const t = Math.atan2(cb - ca, d + sa - sb);
			cands.push({ type: "LSL", t: mod2pi(-al + t), p: Math.sqrt(p2), q: mod2pi(be - t) });
		}
	}
	// RSR
	{
		const p2 = 2 + d * d - 2 * cab + 2 * d * (sb - sa);
		if (p2 >= 0) {
			const t = Math.atan2(ca - cb, d - sa + sb);
			cands.push({ type: "RSR", t: mod2pi(al - t), p: Math.sqrt(p2), q: mod2pi(-be + t) });
		}
	}
	// LSR
	{
		const p2 = -2 + d * d + 2 * cab + 2 * d * (sa + sb);
		if (p2 >= 0) {
			const p = Math.sqrt(p2);
			const t = Math.atan2(-ca - cb, d + sa + sb) - Math.atan2(-2, p);
			cands.push({ type: "LSR", t: mod2pi(-al + t), p, q: mod2pi(-mod2pi(be) + t) });
		}
	}
	// RSL
	{
		const p2 = -2 + d * d + 2 * cab - 2 * d * (sa + sb);
		if (p2 >= 0) {
			const p = Math.sqrt(p2);
			const t = Math.atan2(ca + cb, d - sa - sb) - Math.atan2(2, p);
			cands.push({ type: "RSL", t: mod2pi(al - t), p, q: mod2pi(be - t) });
		}
	}
	let best = null;
	for (const c of cands) {
		// an arc of almost a full turn is the other way round by a hair: treat it as none
		for (const k of ["t", "q"]) {
			if (c[k] > TAU - 1e-9) {
				c[k] = 0;
			}
		}
		c.len = (c.t + c.p + c.q) * R;
		if (!best || c.len < best.len) {
			best = c;
		}
	}
	if (!best) {
		return null;
	}
	return build(a, best, R);
}

/** Segments of a CSC solution, in Factorio coordinates. */
function build(a, c, R) {
	const segs = [];
	let x = a.x;
	let y = a.y;
	let h = a.h;
	const turns = { L: -1, R: 1 }; // a left turn lowers the Factorio orientation
	const parts = [[c.type[0], c.t * R], ["S", c.p * R], [c.type[2], c.q * R]];
	for (const [k, len] of parts) {
		if (len <= 1e-9) {
			continue;
		}
		if (k === "S") {
			segs.push({ kind: "L", x, y, h, len });
			x += Math.sin(h) * len;
			y -= Math.cos(h) * len;
		} else {
			const s = turns[k];
			// centre: R to the side of the turn
			const cx = x + s * R * Math.cos(h);
			const cy = y + s * R * Math.sin(h);
			segs.push({ kind: "C", x, y, h, len, cx, cy, s, R });
			const dh = (s * len) / R;
			const h2 = h + dh;
			x = cx - s * R * Math.cos(h2);
			y = cy - s * R * Math.sin(h2);
			h = h2;
		}
	}
	let acc = 0;
	for (const g of segs) {
		g.s0 = acc;
		acc += g.len;
	}
	return { type: c.type, len: acc, segs, end: { x, y, h } };
}

/** The point at arc length s along path p (clamped), into out = [x, y, h]. */
export function pointAt(p, s, out = [0, 0, 0]) {
	if (p.segs.length === 0) {
		out[0] = p.start?.x ?? 0;
		out[1] = p.start?.y ?? 0;
		out[2] = p.start?.h ?? 0;
		return out;
	}
	s = Math.max(0, Math.min(p.len, s));
	let g = p.segs[p.segs.length - 1];
	for (const q of p.segs) {
		if (s <= q.s0 + q.len) {
			g = q;
			break;
		}
	}
	const u = s - g.s0;
	if (g.kind === "L") {
		out[0] = g.x + Math.sin(g.h) * u;
		out[1] = g.y - Math.cos(g.h) * u;
		out[2] = g.h;
	} else {
		const h2 = g.h + (g.s * u) / g.R;
		out[0] = g.cx - g.s * g.R * Math.cos(h2);
		out[1] = g.cy - g.s * g.R * Math.sin(h2);
		out[2] = h2;
	}
	return out;
}

/** Distance from (x, y) to path p (analytic per segment), and the arc length of the nearest point. */
export function distance(p, x, y) {
	let best = Infinity;
	let bs = 0;
	for (const g of p.segs) {
		if (g.kind === "L") {
			const ux = Math.sin(g.h);
			const uy = -Math.cos(g.h);
			let t = (x - g.x) * ux + (y - g.y) * uy;
			t = Math.max(0, Math.min(g.len, t));
			const d = Math.hypot(x - g.x - ux * t, y - g.y - uy * t);
			if (d < best) {
				best = d;
				bs = g.s0 + t;
			}
		} else {
			// angle of the point around the centre against the arc's start angle, in the turn's direction
			const a0 = Math.atan2(g.y - g.cy, g.x - g.cx);
			const ap = Math.atan2(y - g.cy, x - g.cx);
			let da = (ap - a0) * g.s;
			da = mod2pi(da);
			const span = g.len / g.R;
			let d;
			let t;
			if (da <= span) {
				d = Math.abs(Math.hypot(x - g.cx, y - g.cy) - g.R);
				t = da * g.R;
			} else {
				// past an end: the nearer end point
				const e = pointAt({ segs: [g], len: g.len }, g.len - 1e-12);
				const d0 = Math.hypot(x - g.x, y - g.y);
				const d1 = Math.hypot(x - e[0], y - e[1]);
				d = Math.min(d0, d1);
				t = d0 <= d1 ? 0 : g.len;
			}
			if (d < best) {
				best = d;
				bs = g.s0 + t;
			}
		}
	}
	if (p.segs.length === 0 && p.start) {
		best = Math.hypot(x - p.start.x, y - p.start.y);
	}
	return { d: best, s: bs };
}

/** A path that stands at pose a (both poses at the same place). */
export function still(a) {
	return { type: "", len: 0, segs: [], start: a, end: a };
}
