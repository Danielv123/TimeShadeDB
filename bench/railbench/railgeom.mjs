// Reference implementation of the rail centre-line geometry in docs/v3/RAIL-GEOMETRY.md: where a point at arc length s
// along a rail piece lies, for every rail type of Factorio 2.x (2.0 rails, elevated rails, rail ramps and the legacy
// rails of migrated 1.1 saves), and how rolling stock sits on that path. Written from the formulas the spec derives;
// bench/railbench/geometry.mjs checks it against the recorded ground truth and gen-vectors.mjs writes the shared test
// vectors from it.
//
// Coordinates are Factorio's map coordinates (x east, y south, tiles). Directions are 16-way defines.direction values
// (0 north, 4 east, 8 south, 12 west). Arc length s runs from the piece's front end (LuaEntity.get_rail_end(
// defines.rail_direction.front)) to its back end.

const SQRT2 = Math.SQRT2;
const SQRT5 = Math.sqrt(5);

/** Constants of the spec, section 2. */
export const R = 13; // curved-rail-a and the start of curved-rail-b
export const CURVE_A_LENGTH = 5.132284556; // 13 * atan(5/12)
export const CURVE_B_LENGTH = 5.077891568;
export const CURVE_B_END_RADIUS = 9 * SQRT2;
export const HALF_DIAGONAL_LENGTH = 2 * SQRT5;

// Legacy curved rail (1.1 geometry kept for migrated saves), spec 2.6
const LEGACY_GAUGE = 35 / 64;
const PROJECTION = 0.7071067811865;
const LEGACY_B = (SQRT2 * LEGACY_GAUGE * PROJECTION) / 2;
export const LEGACY_R = (3 - LEGACY_B) / (1 - Math.sin(Math.PI / 4));
const LEGACY_SX = LEGACY_R + 1;
const LEGACY_SY = LEGACY_R * Math.sin(Math.PI / 4) + 1 + LEGACY_B;
export const LEGACY_STRAIGHT = 8 - LEGACY_SY;
export const LEGACY_CURVE_LENGTH = LEGACY_STRAIGHT + LEGACY_GAUGE * PROJECTION + (2 * LEGACY_R * Math.PI) / 8;

// Rail ramp, spec 2.5
const RAMP_HEIGHT = 3;

/** Unit vector of a 16-way direction as Factorio draws it: 22.5-degree steps (used by legacy rails and ramps only). */
export function dirUnit(d) {
	const a = (d * Math.PI) / 8;
	return [Math.sin(a), -Math.cos(a)];
}

/** The 8-fold symmetry the curved and half-diagonal pieces use (spec 2.1): bit 0 mirrors x, bit 1 turns a quarter
 * clockwise, bit 2 a half turn. */
function sym(i8, x, y) {
	if (i8 & 1) {
		x = -x;
	}
	if (i8 & 2) {
		[x, y] = [-y, x];
	}
	if (i8 & 4) {
		x = -x;
		y = -y;
	}
	return [x, y];
}

// Canonical shapes (the piece built facing north at the origin), each a function of s in [0, length].
function curveA(s) {
	const a = s / R;
	return [R * (Math.cos(a) - 1), 2 - R * Math.sin(a)];
}

function curveB(s) {
	const a = (CURVE_A_LENGTH + s) / R;
	const r = R + (s / CURVE_B_LENGTH) * (CURVE_B_END_RADIUS - R);
	return [-11 + r * Math.cos(a), 7 - r * Math.sin(a)];
}

function halfDiagonal(s) {
	const f = 1 - s / SQRT5;
	return [f, 2 * f];
}

function legacyCurve(s) {
	if (s < LEGACY_STRAIGHT) {
		return [1, 4 - s];
	}
	const a = (s - LEGACY_STRAIGHT) / LEGACY_R;
	return [2 - LEGACY_SX + LEGACY_R * Math.cos(a), LEGACY_SY - 4 - LEGACY_R * Math.sin(a)];
}

// Rail ramp: the plan position runs straight over 16 tiles, but s is the length of the path with its height (in
// screen units: height / PROJECTION), so equal steps of s are shorter in plan on the slope. Inverse table by bisection.
function rampHeight(p) {
	const q = Math.max(-0.5, Math.min(0.5, (p - 0.5) * (16 / 14)));
	return (RAMP_HEIGHT * (1 + Math.sin(q * Math.PI))) / 2;
}

const RAMP_STEPS = 4096;
const rampTable = (() => {
	const t = new Float64Array(RAMP_STEPS + 1);
	let acc = 0;
	let h0 = rampHeight(0);
	for (let i = 1; i <= RAMP_STEPS; i++) {
		const p = i / RAMP_STEPS;
		const h = rampHeight(p);
		acc += Math.hypot(16 / RAMP_STEPS, (h - h0) / PROJECTION);
		t[i] = acc;
		h0 = h;
	}
	return t;
})();
export const RAMP_LENGTH = rampTable[RAMP_STEPS];

function rampPlan(s) {
	// p with rampTable(p) = s
	let lo = 0;
	let hi = RAMP_STEPS;
	if (s <= 0) {
		return 0;
	}
	if (s >= RAMP_LENGTH) {
		return 1;
	}
	while (hi - lo > 1) {
		const mid = (lo + hi) >> 1;
		if (rampTable[mid] <= s) {
			lo = mid;
		} else {
			hi = mid;
		}
	}
	return (lo + (s - rampTable[lo]) / (rampTable[hi] - rampTable[lo])) / RAMP_STEPS;
}

/** The base type of a rail type name: elevated pieces have the geometry of the ground ones. */
export function baseType(type) {
	return type.replace(/^elevated-/, "");
}

/** Arc length of a piece. */
export function railLength(type, dir) {
	switch (baseType(type)) {
		case "straight-rail":
			return (dir >> 1) & 1 ? 2 * SQRT2 : 2;
		case "half-diagonal-rail":
			return HALF_DIAGONAL_LENGTH;
		case "curved-rail-a":
			return CURVE_A_LENGTH;
		case "curved-rail-b":
			return CURVE_B_LENGTH;
		case "legacy-straight-rail":
			return (dir >> 1) & 1 ? SQRT2 : 2;
		case "legacy-curved-rail":
			return LEGACY_CURVE_LENGTH;
		case "rail-ramp":
			return RAMP_LENGTH;
		default:
			throw new Error(`railLength: unknown rail type ${type}`);
	}
}

/** Offset from the entity position of the point at arc length s of a piece of this type and direction. */
export function railOffset(type, dir, s) {
	const i8 = (dir >> 1) & 7;
	switch (baseType(type)) {
		case "straight-rail": {
			let x;
			let y;
			if ((dir >> 1) & 1) {
				x = -1 + s / SQRT2;
				y = 1 - s / SQRT2;
			} else {
				x = 0;
				y = s - 1;
			}
			if ((dir >> 2) & 1) {
				[x, y] = [-y, x];
			}
			return [x, y];
		}
		case "half-diagonal-rail": {
			const [x, y] = halfDiagonal(s);
			return sym(i8 & 3, x, y);
		}
		case "curved-rail-a":
			return sym(i8, ...curveA(s));
		case "curved-rail-b":
			return sym(i8, ...curveB(s));
		case "legacy-curved-rail":
			return sym(i8, ...legacyCurve(s));
		case "legacy-straight-rail": {
			if (!((dir >> 1) & 1)) {
				const [ux, uy] = dirUnit(dir);
				return [(1 - s) * ux, (1 - s) * uy];
			}
			const [ax, ay] = dirUnit((dir + 14) % 16); // 45 degrees to the left
			const [bx, by] = dirUnit((dir + 4) % 16); // 90 degrees to the right
			return [ax + s * bx, ay + s * by];
		}
		case "rail-ramp": {
			const p = rampPlan(s);
			// north-facing ramp from (0, 8) to (0, -8); turned clockwise by the direction (4 = a quarter)
			let x = 0;
			let y = 8 - 16 * p;
			for (let q = 0; q < ((dir >> 2) & 3); q++) {
				[x, y] = [-y, x];
			}
			return [x, y];
		}
		default:
			throw new Error(`railOffset: unknown rail type ${type}`);
	}
}

/** The point at arc length s of a piece { type, x, y, dir }, and the unit tangent towards growing s. */
export function railPoint(r, s) {
	const L = railLength(r.type, r.dir);
	const c = Math.max(0, Math.min(L, s));
	const [ox, oy] = railOffset(r.type, r.dir, c);
	const h = 1e-4;
	const a = railOffset(r.type, r.dir, Math.max(0, c - h));
	const b = railOffset(r.type, r.dir, Math.min(L, c + h));
	const tx = b[0] - a[0];
	const ty = b[1] - a[1];
	const tl = Math.hypot(tx, ty) || 1;
	return { x: r.x + ox, y: r.y + oy, tx: tx / tl, ty: ty / tl };
}

/** Orientation (0 north, 0.25 east, turns in [0, 1)) of a direction vector. */
export function orientationOf(tx, ty) {
	return ((Math.atan2(tx, -ty) / (2 * Math.PI)) % 1 + 1) % 1;
}

/** Nearest point of a piece to (x, y): { s, d, x, y, tx, ty } by sampling every `step` and refining. */
export function projectOnRail(r, x, y, step = 0.05) {
	const L = railLength(r.type, r.dir);
	const n = Math.max(2, Math.ceil(L / step));
	let best = Infinity;
	let bs = 0;
	for (let i = 0; i <= n; i++) {
		const s = (L * i) / n;
		const [ox, oy] = railOffset(r.type, r.dir, s);
		const d = (r.x + ox - x) ** 2 + (r.y + oy - y) ** 2;
		if (d < best) {
			best = d;
			bs = s;
		}
	}
	// golden-section refinement in the bracket
	let lo = Math.max(0, bs - L / n);
	let hi = Math.min(L, bs + L / n);
	const f = (s) => {
		const [ox, oy] = railOffset(r.type, r.dir, s);
		return (r.x + ox - x) ** 2 + (r.y + oy - y) ** 2;
	};
	for (let it = 0; it < 40; it++) {
		const m1 = lo + (hi - lo) * 0.382;
		const m2 = lo + (hi - lo) * 0.618;
		if (f(m1) < f(m2)) {
			hi = m2;
		} else {
			lo = m1;
		}
	}
	const s = (lo + hi) / 2;
	const p = railPoint(r, s);
	return { s, d: Math.hypot(p.x - x, p.y - y), x: p.x, y: p.y, tx: p.tx, ty: p.ty };
}

/**
 * A rail path: pieces in travel order, each { type, x, y, dir, rev } where rev = true runs it from its back end to its
 * front end. pathPoint(path, u) is the point at arc length u from the start of the first piece.
 */
export function pathLength(path) {
	return path.reduce((n, r) => n + railLength(r.type, r.dir), 0);
}

export function pathPoint(path, u) {
	let rest = u;
	for (let i = 0; i < path.length; i++) {
		const r = path[i];
		const L = railLength(r.type, r.dir);
		if (rest <= L || i === path.length - 1) {
			const s = r.rev ? L - Math.min(L, Math.max(0, rest)) : Math.min(L, Math.max(0, rest));
			const p = railPoint(r, s);
			return r.rev ? { x: p.x, y: p.y, tx: -p.tx, ty: -p.ty } : p;
		}
		rest -= L;
	}
	return null;
}

/**
 * Rolling stock on a path (spec 3): carriage j has its front joint at arc length u0 - j * (joint + connection) and its
 * rear joint `joint` behind that; the carriage's position is the midpoint of the two joint points and its orientation
 * points from the rear joint to the front one. Joint distances are along the path. Returns [{ x, y, o }].
 */
export function placeTrain(path, u0, n, joint = 4, connection = 3) {
	const out = [];
	for (let j = 0; j < n; j++) {
		const uf = u0 - j * (joint + connection);
		const f = pathPoint(path, uf);
		const b = pathPoint(path, uf - joint);
		out.push({ x: (f.x + b.x) / 2, y: (f.y + b.y) / 2, o: orientationOf(f.x - b.x, f.y - b.y) });
	}
	return out;
}
