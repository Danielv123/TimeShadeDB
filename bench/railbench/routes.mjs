// Rail routes under a recorded train (railbench ground truth): the pieces its leading joint ran over in order, and
// the joint's arc length along them at every recorded line. Used by simulate.mjs and curves.mjs.
import { railPoint } from "./railgeom.mjs";

const J = 2;

// ---------------------------------------------------------------------------------------------------------------
// Routes: the leading joint's arc length along the rail path, per recorded line

/** A route: pieces in travel order with cumulative start offsets; point(u) on it. */
export class Route {
	constructor() {
		this.pieces = [];
		this.cum = [0];
	}
	push(c) {
		this.pieces.push({ type: c.r.type, x: c.r.x, y: c.r.y, dir: c.r.dir, rev: c.rev, r: c.r });
		this.cum.push(this.cum[this.cum.length - 1] + c.r.L);
	}
	get tail() {
		return this.pieces[this.pieces.length - 1];
	}
	u(i, s) {
		const p = this.pieces[i];
		return this.cum[i] + (p.rev ? p.r.L - s : s);
	}
	point(u) {
		let lo = 0;
		let hi = this.pieces.length - 1;
		if (u <= 0) {
			hi = 0;
		}
		while (lo < hi) {
			const mid = (lo + hi + 1) >> 1;
			if (this.cum[mid] <= u) {
				lo = mid;
			} else {
				hi = mid - 1;
			}
		}
		const p = this.pieces[lo];
		const L = p.r.L;
		const t = Math.max(0, Math.min(L, u - this.cum[lo]));
		const q = railPoint(p, p.rev ? L - t : t);
		return q;
	}
	/** The leading carriage centre for a leading joint at u (the midpoint of the joints at u and u - 4). */
	head(u) {
		const a = this.point(u);
		const b = this.point(u - 4);
		return [(a.x + b.x) / 2, (a.y + b.y) / 2];
	}
}

/** Leading joint of line k: { x, y } and the travel axis. */
export function leadingJoint(tr, k) {
	const n = tr.n;
	const fwd = tr.dir[k] > 0;
	const b = k * tr.stride;
	const i0 = fwd ? 0 : n - 1;
	const x = tr.cars[b + 3 * i0] / 256;
	const y = tr.cars[b + 3 * i0 + 1] / 256;
	const o = (tr.cars[b + 3 * i0 + 2] / 65536) * 2 * Math.PI;
	let ux = Math.sin(o);
	let uy = -Math.cos(o);
	if (n > 1) {
		const i1 = fwd ? 1 : n - 2;
		const sx = tr.cars[b + 3 * i1] / 256;
		const sy = tr.cars[b + 3 * i1 + 1] / 256;
		if (ux * (x - sx) + uy * (y - sy) < 0) {
			ux = -ux;
			uy = -uy;
		}
	} else if (!fwd) {
		ux = -ux;
		uy = -uy;
	}
	return { x: x + J * ux, y: y + J * uy, ux, uy };
}

/** Per line of a train: U (leading joint arc length on its run's route, NaN when unknown), run id; and the routes. */
export function buildRoutes(tr, net) {
	const n = tr.ticks.length;
	const U = new Float64Array(n).fill(NaN);
	const run = new Int32Array(n).fill(-1);
	const routes = [];
	let route = null;
	let pending = []; // lines before the route's direction is known: [{ k, p }]
	let lastFwd = null;
	const newRun = () => {
		route = null;
		pending = [];
	};
	for (let k = 0; k < n; k++) {
		const fwd = tr.dir[k] > 0;
		if (lastFwd !== null && fwd !== lastFwd) {
			newRun();
		}
		lastFwd = fwd;
		const j = leadingJoint(tr, k);
		const c = net.near(tr.surface, j.x, j.y, j.ux, j.uy, 0.3, 0.9);
		if (!c.length) {
			newRun();
			continue;
		}
		const p = c[0];
		if (!route) {
			const f = pending[0];
			if (!f) {
				pending.push({ k, p });
				continue;
			}
			let seg = null;
			if (p.r === f.p.r) {
				if (Math.abs(p.s - f.p.s) < 1e-4) {
					pending.push({ k, p });
					continue;
				}
				seg = [{ r: p.r, rev: p.s < f.p.s }];
			} else {
				seg = net.chain(f.p.r, p.r, 6);
				// the projection picked the joint's own direction of travel only if the chain runs that way
			}
			if (!seg) {
				pending = [{ k, p }];
				continue;
			}
			route = new Route();
			for (const s of seg) {
				route.push(s);
			}
			routes.push(route);
			for (const q of [...pending, { k, p }]) {
				const i = route.pieces.findIndex(x => x.r === q.p.r);
				U[q.k] = route.u(i, q.p.s);
				run[q.k] = routes.length - 1;
			}
			pending = [];
			continue;
		}
		// on the route's tail or a recent piece (a projection that flips back across a shared end)
		let i = -1;
		for (let b = route.pieces.length - 1; b >= Math.max(0, route.pieces.length - 4); b--) {
			if (route.pieces[b].r === p.r) {
				i = b;
				break;
			}
		}
		if (i < 0) {
			const seg = net.chain(route.tail.r, p.r, 6);
			if (seg && seg[0].rev === route.tail.rev) {
				for (const s of seg.slice(1)) {
					route.push(s);
				}
				i = route.pieces.length - 1;
			} else {
				newRun();
				pending.push({ k, p });
				continue;
			}
		}
		U[k] = route.u(i, p.s);
		run[k] = routes.length - 1;
	}
	return { U, run, routes };
}

/**
 * The shortest rail path from (piece a, s, direction rev) to (piece b, s) through the network (Dijkstra over pieces,
 * directed): a Route and the arc length of both points on it, or null within maxLen.
 */
export function shortestRoute(net, a, b, maxLen) {
	// state: piece entered, exit end; cost to the start of that piece
	const startExit = a.rev ? 0 : 1;
	const along = (p, rev, s) => (rev ? p.L - s : s);
	const ua = along(a.r, a.rev, a.s);
	const heap = [{ c: 0, r: a.r, exit: startExit, prev: null }];
	const best = new Map();
	while (heap.length) {
		heap.sort((x, y) => y.c - x.c);
		const node = heap.pop();
		const key = `${node.r.unit}:${node.exit}`;
		if (best.has(key)) {
			continue;
		}
		best.set(key, node);
		if (node.r === b.r && node.prev) {
			const rev = node.exit === 0;
			const route = new Route();
			const list = [];
			for (let x = node; x; x = x.prev) {
				list.push({ r: x.r, rev: x.exit === 0 });
			}
			list.reverse();
			for (const c of list) {
				route.push(c);
			}
			return { route, ua, ub: route.cum[list.length - 1] + along(b.r, rev, b.s) };
		}
		if (node.r === b.r && !node.prev && (a.rev ? b.s <= a.s : b.s >= a.s)) {
			const route = new Route();
			route.push({ r: a.r, rev: a.rev });
			return { route, ua, ub: along(b.r, a.rev, b.s) };
		}
		if (node.c > maxLen) {
			continue;
		}
		for (const nb of net.neighbours(node.r, node.exit)) {
			const c = node.c + node.r.L;
			heap.push({ c, r: nb.r, exit: 1 - nb.e, prev: node });
		}
	}
	return null;
}

