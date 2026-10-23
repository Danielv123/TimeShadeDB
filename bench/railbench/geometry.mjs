// Curve geometry study from the railbench ground truth (README.md "Geometry"): checks the rail model of
// docs/v3/RAIL-GEOMETRY.md (railgeom.mjs) against where the game really put every carriage.
//
//   node bench/railbench/geometry.mjs --run <name> [--run <name2> ...] [--every 10] [--train-every 30] [--work <dir>]
//       [--out <file>]
//
// For every moving carriage on every --every-th recorded line of a train:
//   - its joints (bogies) are put at centre +- 2 along its orientation (a carriage's position is the midpoint of its two
//     joints and its orientation the direction between them; the joints are 4 apart along the rail, so the true
//     half-chord is 2 on a straight and 1.998 on a radius-13 curve) and projected on the model of the piece under them:
//     the distance is the model's residual, per rail type. Curve points are also measured against a best-fit circle
//     per curve type in its canonical frame and against the quadratic Bezier through the piece's ends.
//   - the carriage centre's distance from the rail centre line (it lies inside a curve: the joint chord's sagitta).
// For every train on every --train-every-th line: the joints are chained along the rail path (pieces joined at their
// ends); each carriage's front joint is moved along the path until the model's carriage centre (the midpoint of the
// joints at U and U - 4, in the game's own rail distance) lies abreast of the recorded one, and what is left across the
// track is the residual of "joints 4 apart along the rail"; the front joints' spacing should be 7 (joint 4 +
// connection 3); then every carriage is placed by the model from the leading carriage's front joint alone
// (railgeom.placeTrain) and compared with where the game put it.
// For the leading carriage on consecutive ticks while it is on one piece: how far its front joint moved along s against
// the train's speed, and in plan.
// Headings: the sample heading h (second carriage towards the first, as the capture sends it) and the leading carriage's
// own orientation, against the rail tangent at the leading carriage's centre.
import fs from "node:fs";
import path from "node:path";

import { readGroundTruth } from "./gt.mjs";
import { RailNet } from "./railnet.mjs";
import { fitTrain } from "./trainfit.mjs";
import { placeTrain, orientationOf, railPoint, railLength, pathPoint } from "./railgeom.mjs";
import { curveFrame, toFrame, fitCircle } from "./fit.mjs";
import { dirVector } from "./rails.mjs";

const args = process.argv.slice(2);
const runs = args.flatMap((a, i) => (a === "--run" ? [args[i + 1]] : []));
const option = (name, fallback) => {
	const i = args.indexOf(name);
	return i >= 0 && args[i + 1] !== undefined ? args[i + 1] : fallback;
};
const work = path.resolve(option("--work", process.env.RAILBENCH_WORK ?? "G:/project_files/timeshade-v3-work/railbench"));
const EVERY = Number(option("--every", 10));
const TRAIN_EVERY = Number(option("--train-every", 30));
const J = 2;

/** Value collector with quantiles. */
export class Q {
	constructor() {
		this.v = [];
	}
	add(x) {
		this.v.push(x);
	}
	summary(d = 4) {
		if (!this.v.length) {
			return { n: 0 };
		}
		const s = Float64Array.from(this.v).sort();
		const q = (p) => Number(s[Math.min(s.length - 1, Math.floor(p * (s.length - 1)))].toFixed(d));
		let sum = 0;
		for (const x of s) {
			sum += x;
		}
		return { n: s.length, mean: Number((sum / s.length).toFixed(d)), p50: q(0.5), p90: q(0.9), p99: q(0.99), max: q(1) };
	}
}

const bucket = (m, k) => {
	let q = m.get(k);
	if (!q) {
		q = new Q();
		m.set(k, q);
	}
	return q;
};
const typeKey = (r) => r.type.replace(/^elevated-/, "");
const angDeg = (ax, ay, bx, by) => {
	const d = Math.atan2(ax * by - ay * bx, ax * bx + ay * by);
	return (Math.abs(d) * 180) / Math.PI;
};

/** Distance to the quadratic Bezier through a curve piece's ends, control point where the end tangents meet. */
function bezierDist(r, x, y) {
	const a = r.ends[0];
	const b = r.ends[1];
	const u = dirVector(a.dir);
	const v = dirVector(b.dir);
	const den = u[0] * v[1] - u[1] * v[0];
	if (Math.abs(den) < 1e-9) {
		return null;
	}
	const t = ((b.x - a.x) * v[1] - (b.y - a.y) * v[0]) / den;
	const cx = a.x + u[0] * t;
	const cy = a.y + u[1] * t;
	let best = Infinity;
	for (let i = 0; i <= 400; i++) {
		const s = i / 400;
		const w = 1 - s;
		const px = w * w * a.x + 2 * w * s * cx + s * s * b.x;
		const py = w * w * a.y + 2 * w * s * cy + s * s * b.y;
		best = Math.min(best, (px - x) ** 2 + (py - y) ** 2);
	}
	return Math.sqrt(best);
}

const out = { runs, every: EVERY, train_every: TRAIN_EVERY, joint_half: J };
const resid = new Map(); // type -> Q of joint distance to the model
const residBez = new Map();
const centreOff = new Map(); // type -> Q of carriage centre distance to the rail
const canon = new Map(); // curve type -> canonical points for the circle fit
const sRate = new Map(); // type -> Q of (ds / speed)
const planRate = new Map(); // type -> Q of (plan step / speed)
const ownFit = new Map(); // type -> Q of a carriage centre off the model placed from its own front joint
const spacingC = new Q();
const place = { pos: new Q(), angle: new Q(), posCurve: new Q(), angleCurve: new Q(), trains: 0, failed: 0, cars: 0 };
const placeByType = new Map(); // type of the piece under the carriage's front joint -> Q of placement error
const heading = { curve: { h: new Q(), own: new Q() }, straight: { h: new Q(), own: new Q() } };
const kinks = {};
let joints = 0;
let unplaced = 0;

for (const name of runs) {
	const dir = path.join(work, "runs", name);
	const t0 = performance.now();
	const gt = readGroundTruth(path.join(dir, "write", "script-output", "railbench"));
	const net = new RailNet(gt.rails);
	console.log(`${name}: ${gt.trains.length} trains, ${gt.rails.length} rails (${((performance.now() - t0) / 1000).toFixed(1)} s)`);
	for (const tr of gt.trains) {
		const n = tr.n;
		const stride = tr.stride;
		let lastLead = null;
		const cx = new Float64Array(n);
		const cy = new Float64Array(n);
		const ux = new Float64Array(n);
		const uy = new Float64Array(n);
		for (let k = 0; k < tr.ticks.length; k++) {
			const spd = Math.abs(tr.speed[k]) / 1e5;
			if (spd === 0) {
				lastLead = null;
				continue;
			}
			const base = k * stride;
			const fwd = tr.speed[k] > 0;
			const doJoints = k % EVERY === 0;
			const doTrain = k % TRAIN_EVERY === 0;
			// carriage axes pointing towards the train's front
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
			// the leading carriage's front joint (in the direction of travel) on its piece, every tick, while the whole
			// carriage is on one piece (for the rate check): placed by Newton steps as in the placement below
			{
				const i = fwd ? 0 : n - 1;
				const sgn = fwd ? 1 : -1;
				const tx = sgn * ux[i];
				const ty = sgn * uy[i];
				const f = net.near(tr.surface, cx[i] + J * tx, cy[i] + J * ty, tx, ty, 0.3, 0.95);
				const b = net.near(tr.surface, cx[i] - J * tx, cy[i] - J * ty, tx, ty, 0.3, 0.95);
				let cur = null;
				if (f.length === 1 && b.length === 1 && f[0].r === b[0].r) {
					const r = f[0].r;
					const rev = f[0].s < b[0].s;
					const path = [{ type: r.type, x: r.x, y: r.y, dir: r.dir, rev }];
					let u = rev ? r.L - f[0].s : f[0].s;
					for (let it = 0; it < 6; it++) {
						const pa = pathPoint(path, u);
						const pb = pathPoint(path, u - 4);
						const d = ((pa.x + pb.x) / 2 - cx[i]) * tx + ((pa.y + pb.y) / 2 - cy[i]) * ty;
						u -= d;
					}
					if (u >= 4.01 && u <= r.L - 0.01) {
						const p = pathPoint(path, u);
						cur = { r, u, x: p.x, y: p.y, k };
					}
				}
				if (cur && lastLead && lastLead.r === cur.r && lastLead.k === k - 1 && tr.ticks[k - 1] === tr.ticks[k] - 1) {
					bucket(sRate, typeKey(cur.r)).add(Math.abs(cur.u - lastLead.u) / spd);
					bucket(planRate, typeKey(cur.r)).add(Math.hypot(cur.x - lastLead.x, cur.y - lastLead.y) / spd);
				}
				lastLead = cur;
			}
			if (!doJoints && !doTrain) {
				continue;
			}
			// joints from the front of the train: A_i (front of carriage i), then B_i
			const proj = [];
			for (let i = 0; i < n; i++) {
				for (const sgn of [1, -1]) {
					const jx = cx[i] + sgn * J * ux[i];
					const jy = cy[i] + sgn * J * uy[i];
					const c = net.near(tr.surface, jx, jy, ux[i], uy[i], 0.3, 0.9);
					proj.push(c.length ? c[0] : null);
					if (doJoints && c.length) {
						joints++;
						const p = c[0];
						bucket(resid, typeKey(p.r)).add(p.d);
						if (/curved/.test(p.r.type)) {
							const b = bezierDist(p.r, jx, jy);
							if (b !== null) {
								bucket(residBez, typeKey(p.r)).add(b);
							}
							// only points clearly inside one piece go to the circle fit
							if (c.length === 1 && p.s > 0.05 && p.s < p.r.L - 0.05) {
								let t = canon.get(typeKey(p.r));
								if (!t) {
									t = [];
									canon.set(typeKey(p.r), t);
								}
								if (t.length < 400000) {
									t.push(toFrame(curveFrame(p.r), jx, jy));
								}
							}
						}
					} else if (doJoints) {
						unplaced++;
					}
				}
				if (doJoints) {
					const c = net.near(tr.surface, cx[i], cy[i], ux[i], uy[i], 0.4, 0.9);
					if (c.length) {
						bucket(centreOff, typeKey(c[0].r)).add(c[0].d);
					}
				}
			}
			if (doJoints) {
				// headings at the leading carriage
				const i0 = fwd ? 0 : n - 1;
				const c = net.near(tr.surface, cx[i0], cy[i0], ux[i0], uy[i0], 0.4, 0.9);
				if (c.length === 1) {
					const p = c[0];
					const sgn = fwd ? 1 : -1;
					let tx = p.tx;
					let ty = p.ty;
					if (tx * ux[i0] * sgn + ty * uy[i0] * sgn < 0) {
						tx = -tx;
						ty = -ty;
					}
					const kind = /curved/.test(p.r.type) ? "curve" : "straight";
					heading[kind].own.add(angDeg(sgn * ux[i0], sgn * uy[i0], tx, ty));
					if (n >= 2) {
						const i1 = fwd ? 1 : n - 2;
						heading[kind].h.add(angDeg(cx[i0] - cx[i1], cy[i0] - cy[i1], tx, ty));
					}
				}
			}
			if (!doTrain) {
				continue;
			}
			// train placement from the rail path (trainfit.mjs): each carriage's front joint on the chained pieces, then
			// every carriage placed by the model from the leading carriage's front joint alone
			place.trains++;
			const fit = fitTrain(net, tr, k);
			if (!fit) {
				place.failed++;
				continue;
			}
			for (let i = 0; i < n; i++) {
				// what is left is across the track: the model with the joints 4 apart along the rail
				bucket(ownFit, typeKey(fit.front[i].r)).add(fit.residual[i]);
				if (i > 0) {
					spacingC.add(Math.abs(fit.Uf[i - 1] - fit.Uf[i] - 7));
				}
			}
			const placed = placeTrain(fit.path, fit.Uf[0], n, 4, 3);
			for (let i = 0; i < n; i++) {
				const e = Math.hypot(placed[i].x - cx[i], placed[i].y - cy[i]);
				const ou = placed[i].o * 2 * Math.PI;
				const a = angDeg(Math.sin(ou), -Math.cos(ou), ux[i], uy[i]);
				place.pos.add(e);
				place.angle.add(a);
				place.cars++;
				const fr = fit.front[i].r;
				bucket(placeByType, typeKey(fr) + (fr.layer ? " (elevated)" : "")).add(e);
				if (/curved/.test(fr.type) || /curved/.test(fit.back[i].r.type)) {
					place.posCurve.add(e);
					place.angleCurve.add(a);
				}
			}
		}
	}
}

// model facts: tangents at the piece ends (the kinks where pieces meet), for the piece built facing north
for (const type of ["straight-rail", "half-diagonal-rail", "curved-rail-a", "curved-rail-b", "legacy-straight-rail",
	"legacy-curved-rail", "rail-ramp"]) {
	const r = { type, x: 0, y: 0, dir: 0 };
	const L = railLength(type, 0);
	const a = railPoint(r, 0);
	const b = railPoint(r, L);
	kinks[type] = { length: Number(L.toFixed(6)), start_deg: Number((orientationOf(a.tx, a.ty) * 360).toFixed(3)),
		end_deg: Number((orientationOf(b.tx, b.ty) * 360).toFixed(3)) };
}

out.joints = joints;
out.unplaced_joints = unplaced;
out.model_residual = Object.fromEntries([...resid].map(([k, q]) => [k, q.summary()]));
out.bezier_residual = Object.fromEntries([...residBez].map(([k, q]) => [k, q.summary()]));
out.centre_offset = Object.fromEntries([...centreOff].map(([k, q]) => [k, q.summary()]));
out.s_per_speed = Object.fromEntries([...sRate].map(([k, q]) => [k, q.summary(5)]));
out.plan_per_speed = Object.fromEntries([...planRate].map(([k, q]) => [k, q.summary(5)]));
out.carriage_from_own_joint = Object.fromEntries([...ownFit].map(([k, q]) => [k, q.summary()]));
out.pitch_error = spacingC.summary();
out.placement = { trains: place.trains, failed: place.failed, carriages: place.cars, position: place.pos.summary(),
	angle_deg: place.angle.summary(3), position_on_curves: place.posCurve.summary(), angle_deg_on_curves: place.angleCurve.summary(3),
	position_by_type: Object.fromEntries([...placeByType].map(([k, q]) => [k, q.summary()])) };
out.heading = Object.fromEntries(Object.entries(heading).map(([k, v]) => [k, { h_vs_tangent_deg: v.h.summary(3),
	own_vs_tangent_deg: v.own.summary(3) }]));
out.piece_end_tangents = kinks;
out.circle_fit = {};
for (const [type, pts] of canon) {
	if (pts.length < 50) {
		continue;
	}
	const f = fitCircle(pts);
	const res = new Q();
	for (const [x, y] of pts) {
		res.add(Math.abs(Math.hypot(x - f.cx, y - f.cy) - f.r));
	}
	out.circle_fit[type] = { points: pts.length, radius: Number(f.r.toFixed(4)), centre: [Number(f.cx.toFixed(4)), Number(f.cy.toFixed(4))],
		residual: res.summary() };
}

const fmt = (s) => (s.n ? `p50 ${s.p50} p90 ${s.p90} p99 ${s.p99} max ${s.max} (n ${s.n})` : "none");
console.log(`\njoints projected: ${joints}, on no piece: ${unplaced}`);
console.log("model residual (joint to the rail model), tiles:");
for (const [k, s] of Object.entries(out.model_residual)) {
	console.log(`  ${k.padEnd(22)} ${fmt(s)}`);
}
console.log("quadratic Bezier through the ends, tiles:");
for (const [k, s] of Object.entries(out.bezier_residual)) {
	console.log(`  ${k.padEnd(22)} ${fmt(s)}`);
}
console.log("best-fit circle per curve type (canonical frame):");
for (const [k, c] of Object.entries(out.circle_fit)) {
	console.log(`  ${k.padEnd(22)} r ${c.radius} centre (${c.centre}) residual ${fmt(c.residual)}`);
}
console.log("carriage centre off the rail centre line, tiles:");
for (const [k, s] of Object.entries(out.centre_offset)) {
	console.log(`  ${k.padEnd(22)} ${fmt(s)}`);
}
console.log("leading joint: s moved / speed, plan moved / speed:");
for (const k of Object.keys(out.s_per_speed)) {
	console.log(`  ${k.padEnd(22)} s ${fmt(out.s_per_speed[k])}; plan ${fmt(out.plan_per_speed[k])}`);
}
console.log("carriage centre off the model placed from its own front joint (joints 4 apart along the rail), tiles:");
for (const [k, s] of Object.entries(out.carriage_from_own_joint)) {
	console.log(`  ${k.padEnd(22)} ${fmt(s)}`);
}
console.log(`front joint to front joint along the rail, |dU - 7| ${fmt(out.pitch_error)}`);
console.log(`placement from the leading joint and the rail path: ${place.trains} train-lines (${place.failed} not chained), `
	+ `${place.cars} carriages: position ${fmt(out.placement.position)}; angle deg ${fmt(out.placement.angle_deg)}`);
console.log(`  on curves: position ${fmt(out.placement.position_on_curves)}; angle deg ${fmt(out.placement.angle_deg_on_curves)}`);
for (const [k, s] of Object.entries(out.placement.position_by_type)) {
	console.log(`    front joint on ${k.padEnd(30)} ${fmt(s)}`);
}
for (const [k, v] of Object.entries(out.heading)) {
	console.log(`heading on ${k}: h vs tangent ${fmt(v.h_vs_tangent_deg)}; leading carriage's own vs tangent ${fmt(v.own_vs_tangent_deg)}`);
}
console.log(`piece end tangents (deg): ${JSON.stringify(kinks)}`);
const outPath = option("--out", path.join(work, "geometry.json"));
fs.writeFileSync(outPath, JSON.stringify(out, null, 2));
console.log(`-> ${outPath}`);
