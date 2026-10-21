// Least-squares helpers for the curve study (geometry.mjs): a piece's canonical frame, circle and polynomial fits.

import { dirVector } from "./rails.mjs";

/** The canonical frame of a curve piece: reference end, unit x (into the piece) and unit y (towards the turn). */
export function curveFrame(r) {
	// reference end: for curved-rail-a and legacy curves the orthogonal end (direction a multiple of 4), for
	// curved-rail-b the half-diagonal end (odd direction)
	const isB = /curved-rail-b/.test(r.type);
	let i0 = r.ends.findIndex(e => (isB ? e.dir % 2 === 1 : e.dir % 4 === 0));
	if (i0 < 0) {
		i0 = 0;
	}
	const e0 = r.ends[i0];
	const e1 = r.ends[1 - i0];
	let [ux, uy] = dirVector(e0.dir);
	// x points from e0 into the piece
	if ((e1.x - e0.x) * ux + (e1.y - e0.y) * uy < 0) {
		ux = -ux;
		uy = -uy;
	}
	// y towards the side the piece turns to (where e1 lies)
	let vx = -uy;
	let vy = ux;
	if ((e1.x - e0.x) * vx + (e1.y - e0.y) * vy < 0) {
		vx = -vx;
		vy = -vy;
	}
	return { ox: e0.x, oy: e0.y, ux, uy, vx, vy, i0 };
}

export function toFrame(f, x, y) {
	const dx = x - f.ox;
	const dy = y - f.oy;
	return [dx * f.ux + dy * f.uy, dx * f.vx + dy * f.vy];
}

/** Least-squares circle (Kasa, then Gauss-Newton on the geometric distance). Returns { cx, cy, r, rms, max }. */
export function fitCircle(pts) {
	const n = pts.length;
	let mx = 0;
	let my = 0;
	for (const [x, y] of pts) {
		mx += x;
		my += y;
	}
	mx /= n;
	my /= n;
	let suu = 0, svv = 0, suv = 0, suuu = 0, svvv = 0, suvv = 0, svuu = 0;
	for (const [x, y] of pts) {
		const u = x - mx;
		const v = y - my;
		suu += u * u;
		svv += v * v;
		suv += u * v;
		suuu += u * u * u;
		svvv += v * v * v;
		suvv += u * v * v;
		svuu += v * u * u;
	}
	const det = suu * svv - suv * suv;
	let a = (0.5 * (suuu + suvv) * svv - 0.5 * (svvv + svuu) * suv) / det;
	let b = (0.5 * (svvv + svuu) * suu - 0.5 * (suuu + suvv) * suv) / det;
	let cx = a + mx;
	let cy = b + my;
	let r = 0;
	for (const [x, y] of pts) {
		r += Math.hypot(x - cx, y - cy);
	}
	r /= n;
	for (let it = 0; it < 20; it++) {
		// Gauss-Newton on d_i = |p_i - c| - r
		let JtJ = [[0, 0, 0], [0, 0, 0], [0, 0, 0]];
		let Jtd = [0, 0, 0];
		for (const [x, y] of pts) {
			const dx = x - cx;
			const dy = y - cy;
			const L = Math.hypot(dx, dy) || 1e-12;
			const d = L - r;
			const g = [-dx / L, -dy / L, -1];
			for (let i = 0; i < 3; i++) {
				Jtd[i] += g[i] * d;
				for (let k = 0; k < 3; k++) {
					JtJ[i][k] += g[i] * g[k];
				}
			}
		}
		const s = solve3(JtJ, Jtd.map(v => -v));
		if (!s) {
			break;
		}
		cx += s[0];
		cy += s[1];
		r += s[2];
		if (Math.hypot(s[0], s[1], s[2]) < 1e-12) {
			break;
		}
	}
	let ss = 0;
	let mxe = 0;
	for (const [x, y] of pts) {
		const d = Math.abs(Math.hypot(x - cx, y - cy) - r);
		ss += d * d;
		mxe = Math.max(mxe, d);
	}
	return { cx, cy, r, rms: Math.sqrt(ss / n), max: mxe };
}

function solve3(A, b) {
	const m = A.map((row, i) => [...row, b[i]]);
	for (let c = 0; c < 3; c++) {
		let p = c;
		for (let r = c + 1; r < 3; r++) {
			if (Math.abs(m[r][c]) > Math.abs(m[p][c])) {
				p = r;
			}
		}
		if (Math.abs(m[p][c]) < 1e-15) {
			return null;
		}
		[m[c], m[p]] = [m[p], m[c]];
		for (let r = 0; r < 3; r++) {
			if (r !== c) {
				const f = m[r][c] / m[c][c];
				for (let k = c; k < 4; k++) {
					m[r][k] -= f * m[c][k];
				}
			}
		}
	}
	return [m[0][3] / m[0][0], m[1][3] / m[1][1], m[2][3] / m[2][2]];
}

/** Least-squares polynomial y = sum c_i x^i of the given degree (normal equations; x is a few tiles, well scaled). */
export function polyfit(pts, deg) {
	const n = deg + 1;
	const A = Array.from({ length: n }, () => new Float64Array(n + 1));
	for (const [x, y] of pts) {
		const pw = [1];
		for (let i = 1; i < 2 * n; i++) {
			pw.push(pw[i - 1] * x);
		}
		for (let i = 0; i < n; i++) {
			for (let k = 0; k < n; k++) {
				A[i][k] += pw[i + k];
			}
			A[i][n] += pw[i] * y;
		}
	}
	for (let c = 0; c < n; c++) {
		let p = c;
		for (let r = c + 1; r < n; r++) {
			if (Math.abs(A[r][c]) > Math.abs(A[p][c])) {
				p = r;
			}
		}
		[A[c], A[p]] = [A[p], A[c]];
		for (let r = 0; r < n; r++) {
			if (r !== c) {
				const f = A[r][c] / A[c][c];
				for (let k = c; k <= n; k++) {
					A[r][k] -= f * A[c][k];
				}
			}
		}
	}
	return Array.from({ length: n }, (_, i) => A[i][n] / A[i][i]);
}
