import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import express from "express";
import jwt from "jsonwebtoken";
import { addRoutes, authorize, classify, mintViewToken, proxiedCacheControl, VIEW_AUDIENCE } from "../http.js";
import { fakeUser, listen } from "./fakes.js";

const secret = Buffer.from("s3cr3t-s3cr3t-s3cr3t-s3cr3t-s3cr3t");
const now = () => Math.floor(Date.now() / 1000);
const userToken = (user: string, opts: jwt.SignOptions = {}, iat = now()) => jwt.sign({ user, iat }, secret, { audience: "user", ...opts });

function users() {
	const list = new Map([
		["alice", fakeUser("alice", ["timeshade.view"])],
		["bob", fakeUser("bob", [])],
		["carol", fakeUser("carol", ["timeshade.view"], now() + 100)],
	]);
	return { getByName: (name: string) => list.get(name) };
}

test("authorize: user tokens and view tokens of users with timeshade.view; everything else 401 or 403", () => {
	const u = users();
	assert.equal(authorize(undefined, secret, u), 401);
	assert.equal(authorize("garbage", secret, u), 401);
	assert.equal(authorize(userToken("alice"), secret, u), 200);
	assert.equal(authorize(userToken("bob"), secret, u), 403);
	assert.equal(authorize(userToken("nobody"), secret, u), 401);
	assert.equal(authorize(userToken("carol"), secret, u), 401, "issued before tokenValidAfter");
	assert.equal(authorize(jwt.sign({ user: "alice" }, secret, { audience: "host" }), secret, u), 401, "a host token");
	assert.equal(authorize(jwt.sign({ user: "alice" }, Buffer.from("other"), { audience: "user" }), secret, u), 401);
	assert.equal(authorize(jwt.sign({ user: "alice" }, secret, { audience: "user", algorithm: "HS512" }), secret, u), 401);
	const view = mintViewToken("alice", secret);
	assert.equal(authorize(view.token, secret, u), 200);
	assert.ok(Math.abs(view.expiresMs - (Date.now() + 86_400_000)) < 2000);
	assert.equal((jwt.decode(view.token) as jwt.JwtPayload).aud, VIEW_AUDIENCE);
	assert.equal(authorize(mintViewToken("bob", secret).token, secret, u), 403);
	assert.equal(authorize(jwt.sign({ user: "alice", iat: now() - 100 }, secret, { audience: VIEW_AUDIENCE, expiresIn: 10 }), secret, u), 401, "expired");
	// A view token is no user token: Clusterio's own check (audience "user") refuses it.
	assert.throws(() => jwt.verify(view.token, secret, { audience: "user" }));
});

test("classify and Cache-Control rules", () => {
	assert.equal(classify("/"), "viewer");
	assert.equal(classify("/index.html"), "viewer");
	assert.equal(classify("/assets/index-abc.js"), "viewer");
	assert.equal(classify("/api/v3/catalog"), "api");
	assert.equal(classify("/api/v3/"), undefined);
	assert.equal(classify("/api/v3/../../metrics"), undefined);
	assert.equal(classify("/api/v3/%2e%2e/x"), undefined);
	assert.equal(classify("/metrics"), undefined);
	assert.equal(classify("/healthz"), undefined);
	assert.equal(classify("/assets/a/b.js"), undefined);
	assert.equal(proxiedCacheControl("public, max-age=31536000, immutable", "api"), "private, max-age=31536000, immutable, no-transform");
	assert.equal(proxiedCacheControl("public, max-age=300", "viewer"), "public, max-age=300, no-transform");
	assert.equal(proxiedCacheControl(undefined, "api"), "no-transform");
});

type Seen = { path: string; auth?: string; accept?: string };

/** A sidecar stand-in, the controller's app (with Clusterio's compression middleware), and the proxy between them. */
async function setup(opts: { upstream?: boolean } = {}) {
	const seen: Seen[] = [];
	const up = await listen((req, res) => {
		seen.push({ path: req.url ?? "", auth: req.headers.authorization, accept: req.headers["accept-encoding"] as string | undefined });
		const url = new URL(req.url ?? "/", "http://x");
		if (url.pathname === "/api/v3/live") {
			res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store" });
			res.write("event: hello\ndata: {}\n\n");
			const t = setInterval(() => res.write("event: tile\ndata: {}\n\n"), 50);
			req.on("close", () => clearInterval(t));
			return;
		}
		if (url.pathname === "/api/v3/slow") {
			return; // never answers
		}
		const body = JSON.stringify({ path: url.pathname, search: url.search });
		res.writeHead(url.pathname === "/api/v3/missing" ? 404 : 200, {
			"Content-Type": url.pathname.startsWith("/assets/") ? "text/javascript" : "application/json",
			"Cache-Control": "public, max-age=31536000, immutable",
			"X-TSDB-Now": "123",
			"Set-Cookie": "leak=1",
			"ETag": "\"v1\"",
		});
		res.end(body);
	});
	const app = express();
	const require = createRequire(import.meta.url);
	app.use(require("compression")());
	addRoutes(app, {
		upstream: () => (opts.upstream === false ? undefined : { url: up.url, readToken: "sidecar-read" }),
		secret: () => secret,
		users: users(),
		timeoutMs: 300,
	});
	const web = await listen(app as unknown as import("node:http").RequestListener);
	return {
		seen, url: web.url,
		close: async () => {
			await web.close();
			await up.close();
		},
	};
}

test("proxy: the API needs a token of a user with timeshade.view; the sidecar gets its own read token only", async () => {
	const s = await setup();
	try {
		const none = await fetch(`${s.url}/api/timeshade/api/v3/catalog`);
		assert.equal(none.status, 401);
		assert.deepEqual(await none.json(), { error: "a valid Clusterio token is required", code: "unauthorized" });
		assert.equal(none.headers.get("cache-control"), "no-store");
		assert.equal((await fetch(`${s.url}/api/timeshade/api/v3/catalog`, { headers: { Authorization: `Bearer ${userToken("bob")}` } })).status, 403);
		assert.equal((await fetch(`${s.url}/api/timeshade/api/v3/catalog?token=nonsense`)).status, 401);
		assert.equal(s.seen.length, 0, "nothing reached the sidecar");

		const res = await fetch(`${s.url}/api/timeshade/api/v3/catalog?x=1`, { headers: { "X-Access-Token": userToken("alice"), "Accept-Encoding": "gzip" } });
		assert.equal(res.status, 200);
		assert.deepEqual(await res.json(), { path: "/api/v3/catalog", search: "?x=1" });
		assert.equal(res.headers.get("cache-control"), "private, max-age=31536000, immutable, no-transform");
		assert.equal(res.headers.get("x-tsdb-now"), "123");
		assert.equal(res.headers.get("etag"), "\"v1\"");
		assert.equal(res.headers.get("set-cookie"), null, "only listed headers come back");

		const view = mintViewToken("alice", secret).token;
		const q = await fetch(`${s.url}/api/timeshade/api/v3/tile/ds/0/0/0/1.bin?token=${encodeURIComponent(view)}&a=b`);
		assert.equal(q.status, 200);
		assert.deepEqual(await q.json(), { path: "/api/v3/tile/ds/0/0/0/1.bin", search: "?a=b" }, "the token is taken off the query");
		assert.ok(s.seen.every(x => x.auth === "Bearer sidecar-read"));
		assert.equal(s.seen[0].accept, "gzip");

		const missing = await fetch(`${s.url}/api/timeshade/api/v3/missing`, { headers: { Authorization: `Bearer ${view}` } });
		assert.equal(missing.status, 404, "upstream statuses pass through");
	} finally {
		await s.close();
	}
});

test("proxy: viewer files are open; the bare page gets its API base; other paths, methods and traversal are refused", async () => {
	const s = await setup();
	try {
		const asset = await fetch(`${s.url}/api/timeshade/assets/index-abc.js`);
		assert.equal(asset.status, 200);
		assert.equal(asset.headers.get("cache-control"), "public, max-age=31536000, immutable, no-transform");
		const page = await fetch(`${s.url}/api/timeshade/?data=%2Fapi%2Ftimeshade%2Fapi%2Fv3%2F`);
		assert.equal(page.status, 200);
		const bare = await fetch(`${s.url}/api/timeshade`, { redirect: "manual" });
		assert.equal(bare.status, 302);
		assert.equal(bare.headers.get("location"), "timeshade/");
		const noData = await fetch(`${s.url}/api/timeshade/?map=gw-w&token=abc`, { redirect: "manual" });
		assert.equal(noData.status, 302);
		const loc = new URL(noData.headers.get("location")!, `${s.url}/api/timeshade/`);
		assert.equal(loc.pathname, "/api/timeshade/");
		assert.deepEqual(Object.fromEntries(loc.searchParams), { map: "gw-w", data: "/api/timeshade/api/v3/", token: "abc" });
		assert.equal((await fetch(`${s.url}/api/timeshade/metrics`)).status, 404);
		assert.equal((await fetch(`${s.url}/api/timeshade/healthz`)).status, 404);
		assert.equal((await fetch(`${s.url}/api/timeshade/api/v3/..%2f..%2fmetrics`, { headers: { Authorization: `Bearer ${userToken("alice")}` } })).status, 404);
		assert.equal((await fetch(`${s.url}/api/timeshade/api/v3/ingest`, { method: "POST", body: "{}", headers: { Authorization: `Bearer ${userToken("alice")}` } })).status, 405);
		assert.ok(s.seen.every(x => !x.path.includes("metrics") && !x.path.includes("ingest")));
	} finally {
		await s.close();
	}
});

test("proxy: live events stream through unbuffered despite the compression middleware; closing the client ends upstream", async () => {
	const s = await setup();
	try {
		const ctrl = new AbortController();
		const res = await fetch(`${s.url}/api/timeshade/api/v3/live?sfc=a.b&token=${userToken("alice")}`, {
			signal: ctrl.signal, headers: { "Accept-Encoding": "gzip" },
		});
		assert.equal(res.status, 200);
		assert.equal(res.headers.get("content-type"), "text/event-stream");
		assert.equal(res.headers.get("content-encoding"), null, "not compressed by the controller");
		const reader = res.body!.getReader();
		let text = "";
		const t0 = Date.now();
		while ((text.match(/event: tile/g) ?? []).length < 3) {
			const { value } = await reader.read();
			text += Buffer.from(value!).toString();
		}
		assert.ok(Date.now() - t0 < 2000, "events arrive as they are sent");
		assert.match(text, /^event: hello/);
		ctrl.abort();
		assert.equal(s.seen[0].path, "/api/v3/live?sfc=a.b");
	} finally {
		await s.close();
	}
});

test("proxy: 503 while the sidecar is down, 504 when it does not answer", async () => {
	const down = await setup({ upstream: false });
	try {
		const res = await fetch(`${down.url}/api/timeshade/api/v3/catalog`, { headers: { Authorization: `Bearer ${userToken("alice")}` } });
		assert.equal(res.status, 503);
		assert.equal(res.headers.get("retry-after"), "5");
		assert.equal((await fetch(`${down.url}/api/timeshade/api/v3/catalog`)).status, 401, "auth comes first");
	} finally {
		await down.close();
	}
	const s = await setup();
	try {
		const res = await fetch(`${s.url}/api/timeshade/api/v3/slow`, { headers: { Authorization: `Bearer ${userToken("alice")}` } });
		assert.equal(res.status, 504);
	} finally {
		await s.close();
	}
});
