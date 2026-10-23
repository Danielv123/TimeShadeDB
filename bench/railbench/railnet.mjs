// The rail network of a railbench run as exact geometry (railgeom.mjs): pieces from the recorder's RAIL lines, which
// piece a point lies on and where along it, which pieces connect, and the rail path between two pieces. Used by
// geometry.mjs (the curve study) and simulate.mjs (rail-path reconstruction).
import { railLength, railOffset, railPoint } from "./railgeom.mjs";

const CELL = 4;
const COARSE = 0.25;

const endKey = (s, x, y) => `${s}:${Math.round(x * 4)}:${Math.round(y * 4)}`;

export class RailNet {
	/** rails: the recorder's RAIL records ({ surface, unit, type, x, y, dir, layer, ends }). */
	constructor(rails) {
		this.rails = rails;
		this.byUnit = new Map();
		this.grid = new Map();
		this.ends = new Map(); // end key -> [{ r, e }]
		for (const r of rails) {
			this.byUnit.set(r.unit, r);
			r.L = railLength(r.type, r.dir);
			const n = Math.max(2, Math.ceil(r.L / COARSE));
			r.coarse = new Float64Array(2 * (n + 1));
			const cells = new Set();
			for (let i = 0; i <= n; i++) {
				const [ox, oy] = railOffset(r.type, r.dir, (r.L * i) / n);
				const x = r.x + ox;
				const y = r.y + oy;
				r.coarse[2 * i] = x;
				r.coarse[2 * i + 1] = y;
				cells.add(`${r.surface}:${Math.floor(x / CELL)}:${Math.floor(y / CELL)}`);
			}
			for (const c of cells) {
				let l = this.grid.get(c);
				if (!l) {
					l = [];
					this.grid.set(c, l);
				}
				l.push(r);
			}
			for (let e = 0; e < 2; e++) {
				const k = endKey(r.surface, r.ends[e].x, r.ends[e].y);
				let l = this.ends.get(k);
				if (!l) {
					l = [];
					this.ends.set(k, l);
				}
				l.push({ r, e });
			}
		}
	}

	/** Pieces connected to end e of r: [{ r, e }] with e the end of the other piece that meets it (opposite direction). */
	neighbours(r, e) {
		const end = r.ends[e];
		const out = [];
		for (const c of this.ends.get(endKey(r.surface, end.x, end.y)) ?? []) {
			if (c.r !== r && (c.r.ends[c.e].dir + 8) % 16 === end.dir) {
				out.push(c);
			}
		}
		return out;
	}

	/** Nearest point of piece r to (x, y): { r, s, d, x, y, tx, ty }. */
	project(r, x, y) {
		const c = r.coarse;
		const n = c.length / 2 - 1;
		let best = Infinity;
		let bi = 0;
		for (let i = 0; i <= n; i++) {
			const d = (c[2 * i] - x) ** 2 + (c[2 * i + 1] - y) ** 2;
			if (d < best) {
				best = d;
				bi = i;
			}
		}
		let lo = (r.L * Math.max(0, bi - 1)) / n;
		let hi = (r.L * Math.min(n, bi + 1)) / n;
		const f = (s) => {
			const [ox, oy] = railOffset(r.type, r.dir, s);
			return (r.x + ox - x) ** 2 + (r.y + oy - y) ** 2;
		};
		let m1 = lo + (hi - lo) * 0.381966;
		let m2 = lo + (hi - lo) * 0.618034;
		let f1 = f(m1);
		let f2 = f(m2);
		for (let it = 0; it < 30; it++) {
			if (f1 < f2) {
				hi = m2;
				m2 = m1;
				f2 = f1;
				m1 = lo + (hi - lo) * 0.381966;
				f1 = f(m1);
			} else {
				lo = m1;
				m1 = m2;
				f1 = f2;
				m2 = lo + (hi - lo) * 0.618034;
				f2 = f(m2);
			}
		}
		const s = (lo + hi) / 2;
		const p = railPoint(r, s);
		return { r, s, d: Math.hypot(p.x - x, p.y - y), x: p.x, y: p.y, tx: p.tx, ty: p.ty };
	}

	/**
	 * Pieces on surface s within maxD of (x, y) whose tangent there lies within acos(minDot) of the axis (ux, uy) (either
	 * way), nearest first.
	 */
	near(s, x, y, ux, uy, maxD = 0.5, minDot = 0.9) {
		const out = [];
		const seen = new Set();
		const cx = Math.floor(x / CELL);
		const cy = Math.floor(y / CELL);
		for (let dx = -1; dx <= 1; dx++) {
			for (let dy = -1; dy <= 1; dy++) {
				for (const r of this.grid.get(`${s}:${cx + dx}:${cy + dy}`) ?? []) {
					if (seen.has(r)) {
						continue;
					}
					seen.add(r);
					const p = this.project(r, x, y);
					if (p.d <= maxD && (ux === undefined || Math.abs(p.tx * ux + p.ty * uy) >= minDot)) {
						out.push(p);
					}
				}
			}
		}
		out.sort((a, b) => a.d - b.d);
		return out;
	}

	/**
	 * The chain of pieces from a to b (both included) through connected ends, shortest in pieces, at most maxDepth
	 * pieces between them: [{ r, rev }] in order, where rev = false runs the piece from its front end (s = 0) to its back
	 * end. When a === b, `dir` (+1: towards growing s) gives the direction. null when not connected.
	 */
	chain(a, b, maxDepth = 4, dir = 1) {
		if (a === b) {
			return [{ r: a, rev: dir < 0 }];
		}
		// BFS over (piece, exit end)
		const start = [];
		for (let e = 0; e < 2; e++) {
			start.push({ r: a, exit: e, prev: null });
		}
		let frontier = start;
		for (let depth = 0; depth <= maxDepth; depth++) {
			const next = [];
			for (const node of frontier) {
				for (const nb of this.neighbours(node.r, node.exit)) {
					if (nb.r === node.r || (node.prev && nb.r === node.prev.r)) {
						continue;
					}
					const n2 = { r: nb.r, exit: 1 - nb.e, prev: node };
					if (nb.r === b) {
						const list = [];
						for (let x = n2; x; x = x.prev) {
							// a piece left by its back end (exit 1) runs front to back
							list.push({ r: x.r, rev: x.exit === 0 });
						}
						return list.reverse();
					}
					next.push(n2);
				}
			}
			frontier = next;
		}
		return null;
	}
}

/** Arc length along a chain [{ r, rev }] of the point at s of chain[i]. */
export function chainPos(chain, i, s) {
	let u = 0;
	for (let k = 0; k < i; k++) {
		u += chain[k].r.L;
	}
	const c = chain[i];
	return u + (c.rev ? c.r.L - s : s);
}
