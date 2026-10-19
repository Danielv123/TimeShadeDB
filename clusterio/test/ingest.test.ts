import { test } from "node:test";
import assert from "node:assert/strict";
import * as zlib from "node:zlib";
import { IngestQueue } from "../ingest.js";
import { fakeLogger, listen, until, type Line } from "./fakes.js";

type Got = { auth?: string; type?: string; encoding?: string; lines: string[] };

/** An ingest endpoint answering with answer(request number, lines). */
async function endpoint(answer: (n: number, lines: string[]) => { status: number; body?: unknown; retryAfter?: string }) {
	const got: Got[] = [];
	const srv = await listen((req, res) => {
		const chunks: Buffer[] = [];
		req.on("data", c => chunks.push(c));
		req.on("end", () => {
			const lines = zlib.gunzipSync(Buffer.concat(chunks)).toString().split("\n").filter(Boolean);
			got.push({ auth: req.headers.authorization, type: req.headers["content-type"], encoding: req.headers["content-encoding"], lines });
			const a = answer(got.length, lines);
			res.statusCode = a.status;
			if (a.retryAfter) {
				res.setHeader("Retry-After", a.retryAfter);
			}
			res.end(JSON.stringify(a.body ?? {}));
		});
	});
	return { ...srv, got };
}

const ok = () => ({ status: 200, body: { bad_lines: 0 } });

test("lines are posted in order as gzip NDJSON with the bearer token, in batches up to maxBatchBytes", async () => {
	const ep = await endpoint(ok);
	const q = new IngestQueue({ target: () => ({ url: ep.url, token: "tok" }), logger: fakeLogger(), maxBatchBytes: 100 });
	const lines = Array.from({ length: 20 }, (_, i) => JSON.stringify({ i, pad: "x".repeat(20) }));
	for (const l of lines) {
		q.push(l);
	}
	assert.equal(await q.drain(5000), true);
	assert.deepEqual(ep.got.flatMap(g => g.lines), lines);
	assert.ok(ep.got.length > 5, "split into batches");
	assert.ok(ep.got.every(g => g.auth === "Bearer tok" && g.type === "application/x-ndjson" && g.encoding === "gzip"));
	assert.equal(q.stats.sentLines, 20);
	await ep.close();
});

test("503 and network errors are retried (Retry-After honoured) without losing or reordering lines", async () => {
	const ep = await endpoint(n => (n <= 2 ? { status: 503, retryAfter: "0" } : ok()));
	const lines: Line[] = [];
	const q = new IngestQueue({ target: () => ({ url: ep.url, token: "t" }), logger: fakeLogger(lines), minBackoffMs: 10 });
	q.push("a");
	q.push("b");
	assert.equal(await q.drain(5000), true);
	// "a" left alone (the request starts at the first push); the retry carries both.
	assert.deepEqual(ep.got.map(g => g.lines), [["a"], ["a", "b"], ["a", "b"]]);
	assert.equal(q.stats.retries, 2);
	await ep.close();

	// Nothing listens: network errors, then the endpoint comes back at another address.
	let url = "http://127.0.0.1:9";
	const q2 = new IngestQueue({ target: () => ({ url, token: "t" }), logger: fakeLogger(), minBackoffMs: 10, maxBackoffMs: 20 });
	q2.push("c");
	await until(() => q2.stats.retries >= 2, 5000, "retries");
	const ep2 = await endpoint(ok);
	url = ep2.url;
	assert.equal(await q2.drain(5000), true);
	assert.deepEqual(ep2.got.map(g => g.lines), [["c"]]);
	await ep2.close();
});

test("400 and 413 split the batch; a single refused line is dropped and logged", async () => {
	const ep = await endpoint((_n, lines) => (lines.includes("bad") ? { status: 413 } : ok()));
	const lines: Line[] = [];
	const q = new IngestQueue({ target: () => ({ url: ep.url, token: "t" }), logger: fakeLogger(lines) });
	for (const l of ["1", "2", "3", "bad", "5", "6"]) {
		q.push(l);
	}
	assert.equal(await q.drain(5000), true);
	const accepted = ep.got.filter(g => !g.lines.includes("bad")).flatMap(g => g.lines);
	assert.deepEqual(accepted, ["1", "2", "3", "5", "6"]);
	assert.equal(q.stats.refusedLines, 1);
	assert.ok(lines.some(l => l.message === "timeshade.ingest refused a line"));
	await ep.close();
});

test("no target: lines wait (bounded, oldest dropped) and go once wake() says the sidecar is ready", async () => {
	const ep = await endpoint(ok);
	let target: { url: string; token: string } | undefined;
	const lines: Line[] = [];
	const q = new IngestQueue({ target: () => target, logger: fakeLogger(lines), maxQueueBytes: 30, idleMs: 60_000 });
	for (let i = 0; i < 10; i++) {
		q.push(`line-${i}`); // 7 bytes with the newline
	}
	assert.equal(q.length, 4);
	assert.equal(q.stats.droppedLines, 6);
	assert.ok(lines.some(l => l.message === "timeshade.ingest queue full, oldest dropped"));
	target = { url: ep.url, token: "t" };
	q.wake();
	assert.equal(await q.drain(5000), true);
	assert.deepEqual(ep.got.flatMap(g => g.lines), ["line-6", "line-7", "line-8", "line-9"]);
	await ep.close();
});

test("bad lines reported by ingest are counted and warned about; 401 is logged as an error and retried", async () => {
	const ep = await endpoint(n => (n === 1 ? { status: 200, body: { bad_lines: 2, errors: ["e1"] } } : n === 2 ? { status: 401 } : ok()));
	const lines: Line[] = [];
	const q = new IngestQueue({ target: () => ({ url: ep.url, token: "t" }), logger: fakeLogger(lines), minBackoffMs: 5 });
	q.push("x");
	assert.equal(await q.drain(5000), true);
	q.push("y");
	assert.equal(await q.drain(5000), true);
	assert.equal(q.stats.badLines, 2);
	assert.ok(lines.some(l => l.message === "timeshade.ingest refused part of a batch" && l.meta.errors[0] === "e1"));
	assert.ok(lines.some(l => l.message === "timeshade.ingest unauthorized" && l.level === "error"));
	assert.deepEqual(ep.got.map(g => g.lines), [["x"], ["y"], ["y"]]);
	await ep.close();
});

test("lines in flight are not dropped by the queue bound", async () => {
	let release!: () => void;
	const gate = new Promise<void>(resolve => {
		release = resolve;
	});
	const got: string[][] = [];
	const srv = await listen((req, res) => {
		const chunks: Buffer[] = [];
		req.on("data", c => chunks.push(c));
		req.on("end", async () => {
			got.push(zlib.gunzipSync(Buffer.concat(chunks)).toString().split("\n").filter(Boolean));
			if (got.length === 1) {
				await gate;
				res.statusCode = 503;
				res.setHeader("Retry-After", "0");
				res.end("{}");
				return;
			}
			res.end("{}");
		});
	});
	const q = new IngestQueue({ target: () => ({ url: srv.url, token: "t" }), logger: fakeLogger(), maxQueueBytes: 20, maxBatchBytes: 10 });
	q.push("first-1");
	await until(() => got.length === 1, 5000, "the first request");
	for (const l of ["a-11", "b-22", "c-33", "d-44"]) {
		q.push(l);
	}
	release();
	assert.equal(await q.drain(5000), true);
	assert.deepEqual(got.slice(1).flat(), ["first-1", "c-33", "d-44"], "the in-flight line survives; the oldest queued go");
	await srv.close();
});
