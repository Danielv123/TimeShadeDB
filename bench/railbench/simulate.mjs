// Train sampling rules replayed on the railbench ground truth (README.md "Simulation"), with reads every core.STEP
// tiles as the capture makes them. For each rule: samples per train-hour, per train-km and per track kind, and how far
// the true head (leading carriage centre) lies at every moving tick from where that rule's samples put it.
//
//   node bench/railbench/simulate.mjs --run <name> [--radius 13] [--eps 0.25] [--sed 1] [--buf 16] [--rp-buf 1000000]
//       [--rules current,railshape,railshape-h,railpath,railpath-v,matched] [--reads capture|accel]
//       [--slow 120] [--stopstart-ticks 120] [--thin] [--max-gap N] [--work <dir>] [--out <file>]
//
// --reads accel schedules the next read where constant acceleration (measured since the previous read) puts the train
// core.STEP tiles on, instead of core.STEP tiles at the current speed (which waits up to core.SLOW ticks after a start).
//
// Rules:
//   current      capture/lua/core.lua train_read (chord 0.25 tile, SED 1 tile, heading 2 degrees, 16 reads, heartbeat);
//                described by the polyline of samples, walked linearly in time
//   railshape    pose only: no heading rule; a sample when no fixed-radius line/arc path (Dubins CSC, radius
//                sqrt(R^2 - 2^2), the leading carriage centre's radius in a curve) from the last sample fits the reads
//                within --eps across and --sed in time, using the leading carriage's own orientation as the heading
//   railshape-h  the same with the sample heading h the capture sends today (second carriage towards the first)
//   railpath     the rail path is known (the pieces the train ran over): a sample only when a read is more than --sed
//                tiles along the path from where constant speed between samples puts it (plus first, stop, start, end
//                switch, heartbeat and --rp-buf reads); described by the model (railgeom.mjs): the leading joint at the
//                time-interpolated arc length, the leading carriage centre between it and the joint 4 behind
//   railpath-v   railpath with each sample's speed: the arc length between samples is a cubic Hermite in time
//   railpath-t   railpath-v with the timing model of docs/v3/RAIL-GEOMETRY.md 8.3 (stand-then-accelerate from a
//                stop, brake-then-stand into one), what the capture and viewer now use
//   shape-v      railshape walked by the Hermite in time with the speeds, one tolerance (--eps) at the read's time
//   matched     the samples of railpath, but the path between two samples is the shortest rail path through the
//                network from the first sample's piece (in its direction of travel) to the second's: what a viewer
//                that has the rail layer but no piece sequence would draw. Counts how often that is not the true path.
import fs from "node:fs";
import path from "node:path";

import { readGroundTruth } from "./gt.mjs";
import { RailIndex } from "./rails.mjs";
import { RailNet } from "./railnet.mjs";
import { buildRoutes, shortestRoute } from "./routes.mjs";
import { KINDS, K, STOPSTART_TICKS, prepareTrain } from "./truth.mjs";
import { dubins, distance, pointAt, still } from "./dubins.mjs";

const args = process.argv.slice(2);
const option = (name, fallback) => {
	const i = args.indexOf(name);
	return i >= 0 && args[i + 1] !== undefined ? args[i + 1] : fallback;
};
const work = path.resolve(option("--work", process.env.RAILBENCH_WORK ?? "G:/project_files/timeshade-v3-work/railbench"));
const name = option("--run");
const dir = path.join(work, "runs", name);
// the rail centre line's curve radius (geometry.mjs); the leading carriage's centre runs on sqrt(R^2 - J^2) in a curve
const R = Number(option("--radius", 13));
const J = 2;
const Rc = Math.sqrt(R * R - J * J);
const EPS = Number(option("--eps", 0.25));
const SED = Number(option("--sed", 1));
const BUF = Number(option("--buf", 16));
const RP_BUF = Number(option("--rp-buf", 1e6));
// --thin: a full buffer keeps every other read instead of sending a sample (railpath rules), so the per-read check stays
// bounded while a sample pair may span any number of reads
const THIN = args.includes("--thin");
// --max-gap N: a moving train is sampled at least every N ticks (railpath rules), so a live viewer's data is never
// more than N ticks behind the train
const MAX_GAP = Number(option("--max-gap", Infinity));
const ruleNames = option("--rules", "current,railshape,railshape-h,railpath,railpath-v,matched").split(",");
// read schedule: "capture" as core.next_read (core.STEP tiles at the current speed, at most core.SLOW ticks), or
// "accel": core.STEP tiles at the current speed and the acceleration since the previous read
const READS = option("--reads", "capture");

// capture/lua/core.lua constants
const STEP = 4;
// core.SLOW: the longest wait between reads of a moving train (--slow overrides it to try shorter ones)
const SLOW = Number(option("--slow", 120));
const HEARTBEAT = 600;
const CHORD_EPS = 0.25;
const SED_EPS = 1;
const HEAD_EPS = 2 / 360;
const BUF_MAX = 16;
const SCAN = 30;

const turn = (a, b) => {
	const d = Math.abs(a - b) % 1;
	return Math.min(d, 1 - d);
};

/** core.off_chord */
function offChord(e, buf, c) {
	const dx = c.x - e.x;
	const dy = c.y - e.y;
	const dt = c.t - e.t;
	const len2 = dx * dx + dy * dy;
	for (const p of buf) {
		const px = p.x - e.x;
		const py = p.y - e.y;
		if (len2 < 1e-12) {
			if (px * px + py * py > CHORD_EPS * CHORD_EPS) {
				return true;
			}
		} else {
			const cross = dx * py - dy * px;
			if (cross * cross > CHORD_EPS * CHORD_EPS * len2) {
				return true;
			}
		}
		const f = dt > 0 ? (p.t - e.t) / dt : 0;
		if ((px - dx * f) ** 2 + (py - dy * f) ** 2 > SED_EPS * SED_EPS) {
			return true;
		}
	}
	return false;
}

/** The rail-shape path between two reads, with heading field hk ("th": own orientation, "hh": the sample heading). */
function railShape(e, c, hk) {
	if (Math.hypot(c.x - e.x, c.y - e.y) < 1e-6) {
		return still({ x: e.x, y: e.y, h: e[hk] });
	}
	const p = dubins({ x: e.x, y: e.y, h: e[hk] }, { x: c.x, y: c.y, h: c[hk] }, Rc);
	// a path much longer than the chord is a loop the headings' small errors asked for, not a rail route
	const chord = Math.hypot(c.x - e.x, c.y - e.y);
	return p && p.len <= 1.5 * chord + 2 ? p : null;
}

/** A cubic Hermite from pose e to pose c (tangent lengths = the chord), as a polyline path for distance and pointAt. */
function hermitePath(e, c, hk) {
	const L = Math.hypot(c.x - e.x, c.y - e.y);
	const t0 = [Math.sin(e[hk]) * L, -Math.cos(e[hk]) * L];
	const t1 = [Math.sin(c[hk]) * L, -Math.cos(c[hk]) * L];
	const pts = [];
	for (let i = 0; i <= 32; i++) {
		const u = i / 32;
		const h00 = 2 * u ** 3 - 3 * u * u + 1;
		const h10 = u ** 3 - 2 * u * u + u;
		const h01 = -2 * u ** 3 + 3 * u * u;
		const h11 = u ** 3 - u * u;
		pts.push([h00 * e.x + h10 * t0[0] + h01 * c.x + h11 * t1[0], h00 * e.y + h10 * t0[1] + h01 * c.y + h11 * t1[1]]);
	}
	const segs = [];
	let acc = 0;
	for (let i = 0; i + 1 < pts.length; i++) {
		const dx = pts[i + 1][0] - pts[i][0];
		const dy = pts[i + 1][1] - pts[i][1];
		const len = Math.hypot(dx, dy);
		if (len < 1e-9) {
			continue;
		}
		segs.push({ kind: "L", x: pts[i][0], y: pts[i][1], h: Math.atan2(dx, -dy), len, s0: acc });
		acc += len;
	}
	return { type: "H", len: acc, segs };
}

function offShape(hk) {
	return (e, buf, c) => {
		const p = railShape(e, c, hk);
		if (!p) {
			return true;
		}
		const dt = c.t - e.t;
		const tmp = [0, 0, 0];
		for (const q of buf) {
			if (distance(p, q.x, q.y).d > EPS) {
				return true;
			}
			const f = dt > 0 ? (q.t - e.t) / dt : 0;
			pointAt(p, f * p.len, tmp);
			if (Math.hypot(q.x - tmp[0], q.y - tmp[1]) > SED) {
				return true;
			}
		}
		return false;
	};
}

/** railpath: off when a read is more than SED along the path from its time-interpolated place, or the route broke. */
function offPath(e, buf, c) {
	if (e.run !== c.run || Number.isNaN(e.U) || Number.isNaN(c.U)) {
		return true;
	}
	const dt = c.t - e.t;
	for (const q of buf) {
		if (q.run !== e.run || Number.isNaN(q.U)) {
			return true;
		}
		const f = dt > 0 ? (q.t - e.t) / dt : 0;
		if (Math.abs(q.U - (e.U + (c.U - e.U) * f)) > SED) {
			return true;
		}
	}
	return false;
}

/** The arc length at fraction f between samples e and c: cubic Hermite in time with the samples' speeds (tiles a tick). */
export function hermiteU(e, c, f) {
	const dt = c.t - e.t;
	const f2 = f * f;
	const f3 = f2 * f;
	return (2 * f3 - 3 * f2 + 1) * e.U + (f3 - 2 * f2 + f) * dt * e.v + (-2 * f3 + 3 * f2) * c.U + (f3 - f2) * dt * c.v;
}

/**
 * The timing model of a sample pair (docs/v3/RAIL-GEOMETRY.md 8): arc length at fraction f between samples e and c with
 * speeds. A pair that starts at rest (e.v = 0) stands until uniform acceleration from rest reaches c with c's speed,
 * when that takes less than the pair's time; one that ends at rest decelerates uniformly from e's speed and stands.
 * Otherwise a cubic Hermite in time with both speeds.
 */
export function timingU(e, c, f) {
	const T = c.t - e.t;
	const L = c.U - e.U;
	if (T > 0 && L > 0) {
		if (e.v <= 0 && c.v > 0 && (2 * L) / c.v < T) {
			const tm = (2 * L) / c.v;
			const s = T * f - (T - tm);
			return s <= 0 ? e.U : e.U + (c.v * s * s) / (2 * tm);
		}
		if (c.v <= 0 && e.v > 0 && (2 * L) / e.v < T) {
			const tm = (2 * L) / e.v;
			const s = Math.min(T * f, tm);
			return e.U + e.v * s - (e.v * s * s) / (2 * tm);
		}
	}
	return Math.max(Math.min(e.U, c.U), Math.min(Math.max(e.U, c.U), hermiteU(e, c, f)));
}

/** railpath-t: as railpath-v with the stand-and-accelerate timing model (timingU). */
function offPathT(e, buf, c) {
	if (e.run !== c.run || Number.isNaN(e.U) || Number.isNaN(c.U)) {
		return true;
	}
	const dt = c.t - e.t;
	for (const q of buf) {
		if (q.run !== e.run || Number.isNaN(q.U)) {
			return true;
		}
		if (Math.abs(q.U - timingU(e, c, dt > 0 ? (q.t - e.t) / dt : 0)) > SED) {
			return true;
		}
	}
	return false;
}

/** railpath-v: as offPath, against the Hermite in time with both samples' speeds. */
function offPathV(e, buf, c) {
	if (e.run !== c.run || Number.isNaN(e.U) || Number.isNaN(c.U)) {
		return true;
	}
	const dt = c.t - e.t;
	for (const q of buf) {
		if (q.run !== e.run || Number.isNaN(q.U)) {
			return true;
		}
		if (Math.abs(q.U - hermiteU(e, c, dt > 0 ? (q.t - e.t) / dt : 0)) > SED) {
			return true;
		}
	}
	return false;
}

/** Arc length at fraction f of a path of length len between samples e and c: cubic Hermite in time with their speeds. */
function hermiteS(len, e, c, f) {
	const dt = c.t - e.t;
	const f2 = f * f;
	const f3 = f2 * f;
	return Math.max(0, Math.min(len, (f3 - 2 * f2 + f) * dt * e.v + (-2 * f3 + 3 * f2) * len + (f3 - f2) * dt * c.v));
}

/** shape-v: the rail-shaped path with the own orientation, walked by the Hermite in time with the samples' speeds. */
function offShapeV(e, buf, c) {
	const p = railShape(e, c, "th");
	if (!p) {
		return true;
	}
	const dt = c.t - e.t;
	const tmp = [0, 0, 0];
	for (const q of buf) {
		pointAt(p, hermiteS(p.len, e, c, dt > 0 ? (q.t - e.t) / dt : 0), tmp);
		if (Math.hypot(q.x - tmp[0], q.y - tmp[1]) > EPS) {
			return true;
		}
	}
	return false;
}

/** core.train_read, with the off test, the heading rule and the buffer size as parameters. */
function trainRead(m, r, rule) {
	const out = [];
	let e = m.e;
	let why = null;
	if (!e) {
		why = "first";
	} else if (!r.moving) {
		if (m.moving) {
			why = "stop";
		}
	} else if (m.fwd !== undefined && r.fwd !== m.fwd) {
		why = "es";
	} else if (!m.moving) {
		why = "start";
	} else if (r.t - e.t >= HEARTBEAT || (rule.path && r.t - e.t >= MAX_GAP)) {
		why = "heartbeat";
	}
	let buf = m.buf ?? [];
	if (why !== "first" && buf.length > 0) {
		if (why === "es" || rule.off(e, buf, r)) {
			const v = buf[buf.length - 1];
			out.push({ read: v, why: "chord" });
			e = v;
			buf = [];
		}
	}
	if (why === null && r.moving) {
		if (rule.heading && r.h !== null && e.h !== null && turn(r.h, e.h) > HEAD_EPS) {
			why = "heading";
		} else if (rule.shape && buf.length === 0 && !railShape(e, r, rule.shape)) {
			why = "shape"; // no rail-shaped path from the last sample to this read: send it
		} else if (rule.path && buf.length === 0 && (e.run !== r.run || Number.isNaN(r.U))) {
			why = "shape"; // the route broke (a projection failed): send it
		} else if (buf.length >= rule.buf) {
			if (THIN && rule.path) {
				buf = buf.filter((_, i) => i % 2 === 1);
			} else {
				why = "full";
			}
		}
	}
	if (why) {
		out.push({ read: r, why });
		m.e = r;
		m.buf = [];
	} else {
		if (r.moving) {
			buf.push(r);
		}
		m.e = e;
		m.buf = buf;
	}
	m.moving = r.moving;
	m.fwd = r.fwd;
	return out;
}

const allRules = {
	current: { off: offChord, heading: true, buf: BUF_MAX, describe: "polyline" },
	railshape: { off: offShape("th"), heading: false, shape: "th", buf: BUF, describe: "shape" },
	"railshape-h": { off: offShape("hh"), heading: false, shape: "hh", buf: BUF, describe: "shape" },
	"shape-v": { off: offShapeV, heading: false, shape: "th", buf: BUF, describe: "shape", speeds: true },
	railpath: { off: offPath, heading: false, path: true, buf: RP_BUF, describe: "route" },
	"railpath-v": { off: offPathV, heading: false, path: true, buf: RP_BUF, describe: "route", speeds: true },
	"railpath-t": { off: offPathT, heading: false, path: true, buf: RP_BUF, describe: "route", speeds: true, timing: true },
	matched: { off: offPath, heading: false, path: true, buf: RP_BUF, describe: "matched" },
};

// ---------------------------------------------------------------------------------------------------------------

/** A read of line k of train tr: leading carriage centre, h as the capture sends it, the true heading. */
function readOf(tr, k, tick, rt) {
	const n = tr.n;
	const fwd = tr.dir[k] > 0;
	const b = k * tr.stride;
	const i0 = fwd ? 0 : n - 1;
	const x = tr.cars[b + 3 * i0] / 256;
	const y = tr.cars[b + 3 * i0 + 1] / 256;
	const own = tr.cars[b + 3 * i0 + 2] / 65536;
	let h;
	let th;
	if (n > 1) {
		const i1 = fwd ? 1 : n - 2;
		const sx = tr.cars[b + 3 * i1] / 256;
		const sy = tr.cars[b + 3 * i1 + 1] / 256;
		h = Math.round(((Math.atan2(x - sx, sy - y) / (2 * Math.PI) % 1 + 1) % 1) * 1000) / 1000;
		// the leading carriage's own axis, turned to point away from the second carriage
		let o = own * 2 * Math.PI;
		if (Math.sin(o) * (x - sx) - Math.cos(o) * (y - sy) < 0) {
			o += Math.PI;
		}
		th = o;
	} else {
		// one carriage: its orientation, half a turn on when it runs backwards
		h = fwd ? own : (own + 0.5) % 1;
		th = h * 2 * Math.PI;
	}
	return { x, y, t: tick, h, th, hh: h * 2 * Math.PI, v: Math.abs(tr.speed[k]) / 1e5, moving: tr.speed[k] !== 0 && tr.ticks[k] === tick, fwd, k,
		U: rt.U[k], run: rt.run[k] };
}

const gt = readGroundTruth(path.join(dir, "write", "script-output", "railbench"));
const rails = new RailIndex(gt.rails);
const net = new RailNet(gt.rails);
const res = {};
let tBuild = performance.now();
const rts = new Map();
for (const tr of gt.trains) {
	prepareTrain(tr, rails, Number(option("--stopstart-ticks", STOPSTART_TICKS)));
	rts.set(tr, buildRoutes(tr, net));
}
{
	let lines = 0;
	let known = 0;
	for (const [tr, rt] of rts) {
		for (let k = 0; k < tr.ticks.length; k++) {
			if (tr.speed[k] !== 0) {
				lines++;
				if (!Number.isNaN(rt.U[k])) {
					known++;
				}
			}
		}
	}
	res.routes = { moving_lines: lines, on_route: known, seconds: (performance.now() - tBuild) / 1000 };
	console.log(`routes: ${known} of ${lines} moving lines have an arc length on a route (${res.routes.seconds.toFixed(1)} s)`);
}

const q = (a, p) => {
	if (!a.length) {
		return null;
	}
	const s = Float64Array.from(a).sort();
	return Number(s[Math.min(s.length - 1, Math.floor(p * (s.length - 1)))].toFixed(3));
};
const qs = (a) => ({ n: a.length, p50: q(a, 0.5), p90: q(a, 0.9), p99: q(a, 0.99), max: q(a, 1) });

for (const rname of ruleNames) {
	const rule = allRules[rname];
	const st = { samples: new Float64Array(KINDS.length), why: {}, reads: 0, cross: [], sed: [], sedByKind: KINDS.map(() => []),
		failed: 0, pairs: 0, mismatched: 0, unmatched: 0, facing: 0 };
	let movingTicks = 0;
	let dist = 0;
	for (const tr of gt.trains) {
		if (tr.ticks.length === 0) {
			continue;
		}
		const rt = rts.get(tr);
		const m = {};
		const samples = [];
		// tick by tick from the train's first line: a tick without a line is a standing tick at the last line's place
		let next = null;
		let wasMoving = false;
		let lastRead = null;
		let k = 0;
		const last = tr.ticks[tr.ticks.length - 1];
		for (let tick = tr.ticks[0]; tick <= last; tick++) {
			while (k + 1 < tr.ticks.length && tr.ticks[k + 1] <= tick) {
				k++;
			}
			const moving = tr.ticks[k] === tick && tr.speed[k] !== 0;
			if (moving) {
				movingTicks++;
				dist += Math.abs(tr.speed[k]) / 1e5;
			}
			if (moving && !wasMoving && next === null) {
				// the scan (every SCAN ticks) finds a train that started
				next = Math.ceil(tick / SCAN) * SCAN;
			}
			if (!moving && wasMoving && next === null) {
				next = tick; // left the moving set: the scan's stop read (approximated as immediate)
			}
			wasMoving = moving;
			if (next !== null && tick >= next) {
				const r = readOf(tr, k, tick, rt);
				st.reads++;
				for (const em of trainRead(m, r, rule)) {
					samples.push(em.read);
					st.why[em.why] = (st.why[em.why] ?? 0) + 1;
					const kk = em.read.moving ? tr.kindOf(em.read.k) : K.stopstart;
					st.samples[kk]++;
				}
				const v = Math.abs(tr.speed[k]) / 1e5;
				let wait = Math.min(SLOW, Math.max(1, Math.ceil(STEP / v)));
				if (READS === "accel" && lastRead && tick > lastRead.t) {
					// the time to go core.STEP tiles at constant acceleration (measured since the previous read)
					const a = (v - lastRead.v) / (tick - lastRead.t);
					if (a > 1e-6) {
						wait = Math.min(wait, Math.max(1, Math.ceil((-v + Math.sqrt(v * v + 2 * a * STEP)) / a)));
					}
				}
				lastRead = { t: tick, v };
				next = moving ? tick + wait : null;
			}
		}
		// what the samples describe, against every moving tick between two consecutive samples
		for (let i = 0; i + 1 < samples.length; i++) {
			const a = samples[i];
			const b = samples[i + 1];
			if (a.fwd !== b.fwd || b.t - a.t > HEARTBEAT * 2) {
				continue;
			}
			let p = null;
			let route = null;
			let ua = 0;
			let ub = 0;
			if (rule.describe === "shape") {
				p = railShape(a, b, rule.shape);
				if (!p) {
					// no rail-shaped path: a cubic Hermite with the end headings stands in (as the viewer's fallback)
					st.failed++;
					p = hermitePath(a, b, rule.shape);
				}
			} else if (rule.describe === "route" || rule.describe === "matched") {
				if (a.run !== b.run || a.run < 0 || Number.isNaN(a.U) || Number.isNaN(b.U)) {
					st.failed++;
					continue;
				}
				route = rt.routes[a.run];
				ua = a.U;
				ub = b.U;
				if (rule.describe === "matched" && ub - ua > 0.01) {
					st.pairs++;
					// the pieces under both samples' leading joints and the direction of travel on the first
					const ia = route.pieces.findIndex((x, ii) => route.cum[ii] <= ua && ua <= route.cum[ii + 1]);
					const ib = route.pieces.findIndex((x, ii) => route.cum[ii] <= ub && ub <= route.cum[ii + 1]);
					const pa = route.pieces[ia];
					const pb = route.pieces[ib];
					for (let ii = ia + 1; ii <= ib; ii++) {
						// facing switches passed: the entry end of a piece where the previous piece could go elsewhere
						const prev = route.pieces[ii - 1];
						if (net.neighbours(prev.r, prev.rev ? 0 : 1).length > 1) {
							st.facing++;
						}
					}
					const sa = pa.rev ? pa.r.L - (ua - route.cum[ia]) : ua - route.cum[ia];
					const sb = pb.rev ? pb.r.L - (ub - route.cum[ib]) : ub - route.cum[ib];
					const m2 = shortestRoute(net, { r: pa.r, rev: pa.rev, s: sa }, { r: pb.r, s: sb }, ub - ua + 60);
					if (!m2) {
						st.unmatched++;
					} else if (Math.abs((m2.ub - m2.ua) - (ub - ua)) > 0.05) {
						st.mismatched++;
						// draw on the matched route: the head runs over its length in the same time; the joint 4 behind
						// comes from the true route (the part behind the first sample is known)
						const mr = m2.route;
						const off = m2.ua;
						const len = m2.ub - m2.ua;
						route = {
							head: (u) => {
								const v = off + ((u - ua) / (ub - ua)) * len;
								const A = mr.point(v);
								const B = v - 4 >= 0 ? mr.point(v - 4) : rt.routes[a.run].point(u - 4);
								return [(A.x + B.x) / 2, (A.y + B.y) / 2];
							},
						};
					}
				}
			}
			const tmp = [0, 0, 0];
			for (let kk = a.k + 1; kk < b.k; kk++) {
				if (tr.speed[kk] === 0) {
					continue;
				}
				const qr = readOf(tr, kk, tr.ticks[kk], rt);
				const f = (qr.t - a.t) / (b.t - a.t);
				let cross = null;
				let sed;
				if (route) {
					const [hx, hy] = route.head(rule.timing ? timingU({ ...a, U: ua }, { ...b, U: ub }, f) : rule.speeds ? hermiteU({ ...a, U: ua }, { ...b, U: ub }, f) : ua + (ub - ua) * f);
					sed = Math.hypot(qr.x - hx, qr.y - hy);
				} else if (p) {
					cross = distance(p, qr.x, qr.y).d;
					pointAt(p, rule.speeds ? hermiteS(p.len, a, b, f) : f * p.len, tmp);
					sed = Math.hypot(qr.x - tmp[0], qr.y - tmp[1]);
				} else {
					const dx = b.x - a.x;
					const dy = b.y - a.y;
					const l = Math.hypot(dx, dy) || 1e-9;
					let u = ((qr.x - a.x) * dx + (qr.y - a.y) * dy) / (l * l);
					u = Math.max(0, Math.min(1, u));
					cross = Math.hypot(qr.x - a.x - dx * u, qr.y - a.y - dy * u);
					sed = Math.hypot(qr.x - a.x - dx * f, qr.y - a.y - dy * f);
				}
				if (cross !== null) {
					st.cross.push(cross);
				}
				st.sed.push(sed);
				if (sed > 0.5 && process.env.SIM_WORST) {
					(st.worst ??= []).push({ train: tr.id, tick: qr.t, sed: +sed.toFixed(3), kind: KINDS[tr.kindOf(kk)], a: { t: a.t, v: +a.v.toFixed(4), moving: a.moving, U: +a.U?.toFixed(2) }, b: { t: b.t, v: +b.v.toFixed(4), moving: b.moving, U: +b.U?.toFixed(2) }, v: +qr.v.toFixed(4), U: +qr.U?.toFixed(2) });
				}
				st.sedByKind[tr.kindOf(kk)].push(sed);
			}
		}
	}
	if (!res.movingHours) {
		res.movingHours = movingTicks / 216000;
		res.km = dist / 1000;
	}
	const total = st.samples.reduce((x, y) => x + y, 0);
	res[rname] = {
		samples: total, per_train_hour: total / res.movingHours, per_train_km: total / res.km, reads: st.reads, why: st.why,
		by_kind: Object.fromEntries(KINDS.map((k, i) => [k, st.samples[i]])), failed_pairs: st.failed,
		cross_track: st.cross.length ? qs(st.cross) : null, position_at_time: qs(st.sed),
		position_by_kind: Object.fromEntries(KINDS.map((k, i) => [k, qs(st.sedByKind[i])])),
	};
	if (rule.describe === "matched") {
		res[rname].matching = { pairs: st.pairs, not_the_true_path: st.mismatched, no_path: st.unmatched,
			facing_switches_passed: st.facing, facing_switches_per_km: st.facing / res.km };
	}
	const r = res[rname];
	if (st.worst) {
		st.worst.sort((x, y) => y.sed - x.sed);
		r.worst = st.worst.slice(0, 400);
		r.over_half = st.worst.length;
	}
	console.log(`${rname}: ${total} samples (${r.per_train_hour.toFixed(0)}/train-hour, ${r.per_train_km.toFixed(1)}/train-km) from `
		+ `${st.reads} reads; why ${JSON.stringify(st.why)}`);
	console.log(`  by kind ${JSON.stringify(r.by_kind)}`);
	console.log(`  head vs described: across ${JSON.stringify(r.cross_track)}; at its time ${JSON.stringify(r.position_at_time)}`
		+ `${st.failed ? `; ${st.failed} sample pairs without a path` : ""}`);
	if (r.matching) {
		console.log(`  matching: ${JSON.stringify(r.matching)}`);
	}
}
res.params = { R, Rc, EPS, SED, BUF, RP_BUF, READS, SLOW, rules: ruleNames };
const outPath = option("--out", path.join(dir, `simulate-R${R}-e${EPS}-s${SED}-b${BUF}-${READS}${SLOW === 120 ? "" : `-slow${SLOW}`}.json`));
fs.writeFileSync(outPath, JSON.stringify(res, null, 2));
console.log(`-> ${outPath}`);
