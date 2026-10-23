import { test } from "node:test";
import assert from "node:assert/strict";
import * as path from "node:path";
import { Sidecar, type SidecarOptions } from "../sidecar.js";
import { fakeLogger, until, type Line } from "./fakes.js";

const fake = path.join(import.meta.dirname, "fake_sidecar.js");

function sidecar(env: Record<string, string> = {}, extra: Partial<SidecarOptions> = {}) {
	const lines: Line[] = [];
	const s = new Sidecar({
		command: process.execPath,
		args: [fake, "serve-v3", "--ingest-token", "i", "--read-token", "r"],
		env: { ...process.env, ...env },
		logger: fakeLogger(lines),
		minBackoffMs: 50,
		maxBackoffMs: 400,
		...extra,
	});
	return { s, lines };
}

async function received(url: string) {
	const res = await fetch(`${url}/_received`);
	return await res.json() as { lines: string[]; calls: number; args: string[] };
}

test("spawn: ready with the address of its first stdout line; later lines and stderr are logged; stop closes stdin", async () => {
	const { s, lines } = sidecar();
	const states: string[] = [];
	s.on("state", st => states.push(st));
	s.start();
	const url = await s.ready(10_000);
	assert.match(url, /^http:\/\/127\.0\.0\.1:\d+$/);
	assert.equal(s.url, url);
	assert.equal(s.state, "running");
	assert.deepEqual((await received(url)).args.slice(0, 1), ["serve-v3"]);
	await until(() => lines.some(l => l.message === "timeshade sidecar: fake sidecar up")
		&& lines.some(l => l.message === "timeshade sidecar: fake sidecar log line" && l.level === "info"));
	await s.stop();
	assert.equal(s.state, "stopped");
	assert.equal(s.url, undefined);
	assert.equal(s.crashes, 0);
	assert.deepEqual(states, ["starting", "running", "stopped"]);
	await assert.rejects(fetch(url), "the process is gone");
});

test("crash: an exit nobody asked for restarts it, on a new port, and it serves again", async () => {
	const { s, lines } = sidecar();
	s.start();
	const first = await s.ready(10_000);
	const exits: unknown[] = [];
	s.on("exit", (code: number) => exits.push(code));
	await fetch(`${first}/_crash`, { method: "POST" }).catch(() => undefined);
	await until(() => s.state !== "running", 5000, "the crash to be seen");
	const second = await s.ready(10_000);
	assert.notEqual(second, first);
	assert.equal(s.crashes, 1);
	assert.equal(s.starts, 2);
	assert.deepEqual(exits, [3]);
	assert.ok(lines.some(l => l.message === "timeshade.sidecar exited" && l.level === "warn" && /code 3/.test(l.meta.reason)));
	assert.deepEqual((await received(second)).calls, 0);
	await s.stop();
});

test("crash loop: the delay doubles up to the maximum and resets after a stable run", async () => {
	const { s, lines } = sidecar({ FAKE_CRASH_MS: "30" }, { minBackoffMs: 20, maxBackoffMs: 160 });
	s.start();
	await until(() => s.crashes >= 6, 15_000, "six crashes");
	await s.stop();
	const delays = lines.filter(l => l.message === "timeshade.sidecar exited").map(l => l.meta.restartInMs);
	assert.deepEqual(delays.slice(0, 6), [20, 40, 80, 160, 160, 160]);
	assert.equal(s.state, "stopped");
	const crashes = s.crashes;
	await new Promise(resolve => setTimeout(resolve, 300));
	assert.equal(s.crashes, crashes, "no restart after stop");

	const stable = sidecar({ FAKE_CRASH_MS: "120" }, { minBackoffMs: 20, maxBackoffMs: 1000, stableMs: 50 });
	stable.s.start();
	await until(() => stable.s.crashes >= 3, 10_000, "three crashes");
	await stable.s.stop();
	const d2 = stable.lines.filter(l => l.message === "timeshade.sidecar exited").map(l => l.meta.restartInMs);
	assert.deepEqual(d2.slice(0, 3), [20, 20, 20], "each run outlived stableMs");
});

test("a process that never prints its address is killed after the ready timeout and restarted", async () => {
	const { s, lines } = sidecar({ FAKE_MODE: "silent" }, { readyTimeoutMs: 300, minBackoffMs: 5000 });
	s.start();
	await until(() => s.state === "backoff", 5000, "the kill");
	assert.ok(lines.some(l => /did not print its address/.test(l.message)));
	assert.equal(s.crashes, 1);
	await s.stop();
	assert.equal(s.state, "stopped");
});

test("a binary that does not exist is retried with backoff, and stop() ends that", async () => {
	const lines: Line[] = [];
	const s = new Sidecar({
		command: path.join(import.meta.dirname, "no-such-binary.exe"), args: [], logger: fakeLogger(lines),
		minBackoffMs: 20, maxBackoffMs: 40,
	});
	s.start();
	await until(() => s.crashes >= 2, 5000, "two failed starts");
	assert.ok(lines.some(l => l.message === "timeshade.sidecar exited" && /ENOENT/.test(l.meta.reason)));
	await s.stop();
	assert.equal(s.state, "stopped");
});

test("stop kills a process that ignores its closed stdin; restart applies a new command line", async () => {
	// node -e that ignores stdin: stdin close does not end it.
	const lines: Line[] = [];
	const s = new Sidecar({
		command: process.execPath,
		args: ["-e", "process.stdout.write('listening on 127.0.0.1:1\\n'); setInterval(() => {}, 1000)"],
		logger: fakeLogger(lines), stopTimeoutMs: 200,
	});
	s.start();
	await s.ready(10_000);
	const t0 = Date.now();
	await s.stop();
	assert.ok(Date.now() - t0 >= 150, "waited for the stop timeout");
	assert.equal(s.crashes, 0);
	s.configure(process.execPath, [fake, "serve-v3"]);
	await s.restart();
	const url = await s.ready(10_000);
	assert.notEqual(url, "http://127.0.0.1:1");
	await s.stop();
});
