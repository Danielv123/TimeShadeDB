// Rail pieces from the recorder's RAIL lines: which piece a point lies on, and the track kind there (straight,
// diagonal, curve, junction), for splitting the benchmark's numbers by track kind.
//
// The shape used here only classifies: straight pieces are the segment between their two end locations, curves the
// quadratic Bezier through their end locations with the control point where the two end tangents meet (within about
// 0.1 tile of the true path for a 22.5 or 45 degree piece; bench/railbench/geometry.mjs measures the true path).

/** Unit vector of a 16-way defines.direction (0 north, 4 east; y grows southwards). */
export function dirVector(d) {
	const a = (d * Math.PI) / 8;
	return [Math.sin(a), -Math.cos(a)];
}

const CURVE = /curved/;

/** The track kind of a piece on its own: "curve", "diagonal" (diagonal or half-diagonal straight) or "straight". */
export function pieceKind(r) {
	if (CURVE.test(r.type)) {
		return "curve";
	}
	if (/half-diagonal/.test(r.type)) {
		return "diagonal";
	}
	const ex = r.ends[1].x - r.ends[0].x;
	const ey = r.ends[1].y - r.ends[0].y;
	return Math.abs(ex) > 0.01 && Math.abs(ey) > 0.01 ? "diagonal" : "straight";
}

function lineIntersect(p, u, q, v) {
	const den = u[0] * v[1] - u[1] * v[0];
	if (Math.abs(den) < 1e-9) {
		return null;
	}
	const t = ((q[0] - p[0]) * v[1] - (q[1] - p[1]) * v[0]) / den;
	return [p[0] + u[0] * t, p[1] + u[1] * t];
}

/** Points along the piece every ~0.25 tile, for classification. */
function samplePiece(r) {
	const a = [r.ends[0].x, r.ends[0].y];
	const b = [r.ends[1].x, r.ends[1].y];
	let ctrl = null;
	if (CURVE.test(r.type)) {
		ctrl = lineIntersect(a, dirVector(r.ends[0].dir), b, dirVector(r.ends[1].dir));
	}
	const len = Math.max(r.length, Math.hypot(b[0] - a[0], b[1] - a[1]));
	const n = Math.max(2, Math.ceil(len / 0.25));
	const pts = [];
	for (let i = 0; i <= n; i++) {
		const t = i / n;
		if (ctrl) {
			const u = 1 - t;
			pts.push([u * u * a[0] + 2 * u * t * ctrl[0] + t * t * b[0], u * u * a[1] + 2 * u * t * ctrl[1] + t * t * b[1]]);
		} else {
			pts.push([a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t]);
		}
	}
	return pts;
}

const CELL = 4;
const cellKey = (s, cx, cy) => `${s}:${cx}:${cy}`;

export class RailIndex {
	constructor(rails) {
		this.rails = rails;
		this.grid = new Map();
		// junction pieces: an end at a joint where three or more rail ends of the same layer meet (a switch: the incoming
		// piece and the two it can go on to)
		const joints = new Map();
		const jk = (r, e) => `${r.surface}:${r.layer}:${Math.round(e.x * 4)}:${Math.round(e.y * 4)}`;
		for (const r of rails) {
			for (const e of r.ends) {
				const k = jk(r, e);
				joints.set(k, (joints.get(k) ?? 0) + 1);
			}
		}
		for (const r of rails) {
			r.kind = pieceKind(r);
			r.junction = r.ends.some(e => (joints.get(jk(r, e)) ?? 0) >= 3);
			r.pts = samplePiece(r);
			const seen = new Set();
			for (const [x, y] of r.pts) {
				const k = cellKey(r.surface, Math.floor(x / CELL), Math.floor(y / CELL));
				if (!seen.has(k)) {
					seen.add(k);
					let l = this.grid.get(k);
					if (!l) {
						l = [];
						this.grid.set(k, l);
					}
					l.push(r);
				}
			}
		}
	}

	/** Pieces within maxD of (x, y) on surface s, nearest first: [{ r, d, tx, ty }] with the piece's tangent there. */
	near(s, x, y, maxD = 0.6) {
		const out = [];
		const cx = Math.floor(x / CELL);
		const cy = Math.floor(y / CELL);
		const seen = new Set();
		for (let dx = -1; dx <= 1; dx++) {
			for (let dy = -1; dy <= 1; dy++) {
				for (const r of this.grid.get(cellKey(s, cx + dx, cy + dy)) ?? []) {
					if (seen.has(r)) {
						continue;
					}
					seen.add(r);
					let best = Infinity;
					let bi = 0;
					const p = r.pts;
					for (let i = 0; i + 1 < p.length; i++) {
						const d = segDist(x, y, p[i], p[i + 1]);
						if (d < best) {
							best = d;
							bi = i;
						}
					}
					if (best <= maxD) {
						const tx = p[bi + 1][0] - p[bi][0];
						const ty = p[bi + 1][1] - p[bi][1];
						const tl = Math.hypot(tx, ty) || 1;
						out.push({ r, d: best, tx: tx / tl, ty: ty / tl });
					}
				}
			}
		}
		out.sort((a, b) => a.d - b.d);
		return out;
	}

	/**
	 * The track kind at (x, y) for a mover heading along (hx, hy): the pieces near the point whose tangent lies within
	 * 20 degrees of the heading (either way); "junction" when one of them is a junction piece, else the kind of the
	 * nearest; "unknown" when none is near.
	 */
	kindAt(s, x, y, hx, hy) {
		const c = this.near(s, x, y, 0.75).filter(n => Math.abs(n.tx * hx + n.ty * hy) > 0.94);
		if (c.length === 0) {
			return { kind: "unknown", piece: null };
		}
		const piece = c[0].r;
		if (c.some(n => n.r.junction)) {
			return { kind: "junction", piece };
		}
		return { kind: piece.kind, piece };
	}
}

function segDist(x, y, a, b) {
	const vx = b[0] - a[0];
	const vy = b[1] - a[1];
	const l2 = vx * vx + vy * vy;
	let t = l2 > 0 ? ((x - a[0]) * vx + (y - a[1]) * vy) / l2 : 0;
	t = Math.max(0, Math.min(1, t));
	return Math.hypot(x - a[0] - vx * t, y - a[1] - vy * t);
}
