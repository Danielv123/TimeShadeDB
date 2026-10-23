// A stand-in for `timeshadedb serve-v3` in the sidecar and controller tests, run with node. It prints
// "listening on <addr>" (unless FAKE_MODE=silent), records ingest bodies, answers GET /api/v3/* with what it got,
// exits on POST /_crash, after FAKE_CRASH_MS, and when its stdin closes (as --exit-on-stdin-close does).
import * as http from "node:http";
import type { AddressInfo } from "node:net";
import * as zlib from "node:zlib";

const mode = process.env.FAKE_MODE ?? "ok";
const args = process.argv.slice(2);
const flag = (name: string) => {
	const i = args.indexOf(name);
	return i >= 0 ? args[i + 1] : undefined;
};
const ingestToken = flag("--ingest-token");
const readToken = flag("--read-token");
const received: string[] = [];
let ingestCalls = 0;
// FAKE_INGEST: "ok", or a list of statuses to answer the first ingest calls with, e.g. "503,503".
const statuses = (process.env.FAKE_INGEST ?? "").split(",").filter(Boolean).map(Number);

const server = http.createServer((req, res) => {
	const chunks: Buffer[] = [];
	req.on("data", c => chunks.push(c));
	req.on("end", () => {
		const url = new URL(req.url ?? "/", "http://x");
		if (req.method === "POST" && url.pathname === "/_crash") {
			res.end("bye");
			process.exit(3);
		}
		if (req.method === "GET" && url.pathname === "/_received") {
			res.setHeader("Content-Type", "application/json");
			res.end(JSON.stringify({ lines: received, calls: ingestCalls, args }));
			return;
		}
		if (req.method === "POST" && url.pathname === "/api/v3/ingest") {
			ingestCalls += 1;
			if (req.headers.authorization !== `Bearer ${ingestToken}`) {
				res.statusCode = 401;
				res.end("{}");
				return;
			}
			const status = statuses.shift();
			if (status && status !== 200) {
				res.statusCode = status;
				res.setHeader("Retry-After", "0");
				res.end(JSON.stringify({ error: "fake", code: "overloaded" }));
				return;
			}
			let body = Buffer.concat(chunks);
			if (req.headers["content-encoding"] === "gzip") {
				body = zlib.gunzipSync(body);
			}
			const lines = body.toString("utf8").split("\n").filter(Boolean);
			received.push(...lines);
			res.setHeader("Content-Type", "application/json");
			res.end(JSON.stringify({ lines: lines.length, bad_lines: 0, bad_messages: 0 }));
			return;
		}
		if (url.pathname.startsWith("/api/v3/")) {
			if (req.headers.authorization !== `Bearer ${readToken}`) {
				res.statusCode = 401;
				res.end("{}");
				return;
			}
			res.setHeader("Content-Type", "application/json");
			res.setHeader("Cache-Control", "public, max-age=10");
			res.end(JSON.stringify({ path: url.pathname, query: url.search }));
			return;
		}
		res.statusCode = 404;
		res.end();
	});
});

server.listen(0, "127.0.0.1", () => {
	if (mode !== "silent") {
		const addr = server.address() as AddressInfo;
		process.stdout.write(`listening on 127.0.0.1:${addr.port}\n`);
		process.stdout.write("fake sidecar up\n");
	}
	process.stderr.write("fake sidecar log line\n");
});
process.stdin.on("end", () => process.exit(0));
process.stdin.resume();
if (process.env.FAKE_CRASH_MS) {
	setTimeout(() => process.exit(4), Number(process.env.FAKE_CRASH_MS));
}
