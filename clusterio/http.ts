// SPDX-License-Identifier: BlueOak-1.0.0
// The map behind Clusterio's auth (docs/v3/API.md 1.3): /api/timeshade/* on the controller's web server is proxied to
// the sidecar. The API (/api/timeshade/api/v3/*) needs a Clusterio user token, or a view token minted for the web
// page, of a user with timeshade.view; the viewer's own files (/api/timeshade/, /api/timeshade/assets/*) are open, as
// they are on the sidecar. The sidecar listens on 127.0.0.1 with its own random read token, which only this proxy
// knows; the client's token never reaches it.
// authorize() is ported from illisis/minimap http.ts (Blue Oak Model License 1.0.0, see LICENSE-BlueOak-1.0.0.md).
import * as http from "node:http";
import jwt from "jsonwebtoken";
import type { Application, Request, Response } from "express";

/** The audience of view tokens; Clusterio's own routes only accept "user", so a view token opens nothing else. */
export const VIEW_AUDIENCE = "timeshade.view";
export const VIEW_PERMISSION = "timeshade.view";
/** View tokens last a day; the web page fetches a new one before then. */
export const VIEW_TTL_SECONDS = 24 * 3600;
export const PREFIX = "/api/timeshade";

/** The user lookup Clusterio's own token check uses (controller routes.ts validateUserToken). */
export type Users = {
	getByName(name: string): { tokenValidAfter: number; checkPermission(permission: string): void } | undefined;
};

/** The token of a request: X-Access-Token, Authorization: Bearer, or ?token= (EventSource cannot set headers). */
export function requestToken(header: (name: string) => string | undefined, query: URLSearchParams): string | undefined {
	const token = header("x-access-token");
	if (token) {
		return token;
	}
	const authorization = header("authorization");
	if (authorization && /^Bearer /i.test(authorization)) {
		return authorization.slice("Bearer ".length).trim();
	}
	return query.get("token") ?? undefined;
}

/**
 * Clusterio's user token check (audience "user", issued after the user's tokenValidAfter), also accepting view tokens
 * (audience "timeshade.view", which expire), then timeshade.view. Returns the status to answer, 200 when allowed.
 */
export function authorize(token: string | undefined, secret: Buffer, users: Users): 200 | 401 | 403 {
	if (!token) {
		return 401;
	}
	let user: ReturnType<Users["getByName"]>;
	try {
		const payload = jwt.verify(token, secret, { audience: ["user", VIEW_AUDIENCE], algorithms: ["HS256"] });
		if (typeof payload === "string" || typeof payload.user !== "string") {
			return 401;
		}
		user = users.getByName(payload.user);
		if (!user || !payload.iat || payload.iat < user.tokenValidAfter) {
			return 401;
		}
	} catch {
		return 401;
	}
	try {
		user.checkPermission(VIEW_PERMISSION);
	} catch {
		return 403;
	}
	return 200;
}

/** A view token for `user`, signed with the controller's secret. */
export function mintViewToken(user: string, secret: Buffer, ttlSeconds = VIEW_TTL_SECONDS): { token: string; expiresMs: number } {
	const iat = Math.floor(Date.now() / 1000);
	const token = jwt.sign({ user, iat }, secret, { audience: VIEW_AUDIENCE, expiresIn: ttlSeconds, algorithm: "HS256" });
	return { token, expiresMs: (iat + ttlSeconds) * 1000 };
}

export type Upstream = { url: string; readToken: string };

export type ProxyDeps = {
	/** The sidecar now, or undefined while it is not running. */
	upstream: () => Upstream | undefined;
	secret: () => Buffer;
	users: Users;
	/** Upstream timeout for everything but live events. */
	timeoutMs?: number;
	agent?: http.Agent;
};

const REQUEST_HEADERS = ["accept", "accept-encoding", "if-none-match", "if-modified-since", "range"];
const RESPONSE_HEADERS = [
	"content-type", "content-encoding", "content-length", "content-range", "accept-ranges", "etag", "last-modified",
	"vary", "x-tsdb-now", "retry-after",
];

/** A path the proxy forwards: "viewer" (open), "api" (auth), or undefined (404). */
export function classify(rel: string): "viewer" | "api" | undefined {
	if (/(^|\/)\.\.?(\/|$)/.test(rel) || /%(2e|2f|5c)/i.test(rel) || rel.includes("\\")) {
		return undefined;
	}
	if (rel === "/" || rel === "/index.html" || /^\/assets\/[^/]+$/.test(rel)) {
		return "viewer";
	}
	if (rel.startsWith("/api/v3/") && rel.length > "/api/v3/".length) {
		return "api";
	}
	return undefined;
}

/** Cache-Control behind auth: private, and no-transform so the controller's compression leaves the body alone. */
export function proxiedCacheControl(value: string | undefined, kind: "viewer" | "api"): string {
	let v = value ?? "";
	if (kind === "api") {
		v = v.replace(/\bpublic\b/, "private");
	}
	if (!/\bno-transform\b/.test(v)) {
		v = v ? `${v}, no-transform` : "no-transform";
	}
	return v;
}

function error(res: Response, status: number, code: string, message: string) {
	res.status(status).set("Cache-Control", "no-store").json({ error: message, code });
}

/** Adds the proxy routes to the controller's express app. */
export function addRoutes(app: Application, deps: ProxyDeps): void {
	const agent = deps.agent ?? new http.Agent({ keepAlive: true, maxSockets: 64 });
	const timeoutMs = deps.timeoutMs ?? 30_000;

	app.all(`${PREFIX}{/*rest}`, (req: Request, res: Response) => {
		const url = new URL(req.originalUrl, "http://controller");
		const rel = url.pathname.slice(PREFIX.length);
		const token = requestToken(name => req.get(name), url.searchParams);
		// Clusterio's request log redacts these, but nothing later needs them either.
		delete req.headers.authorization;
		delete req.headers["x-access-token"];
		url.searchParams.delete("token");
		if (req.method !== "GET" && req.method !== "HEAD") {
			error(res, 405, "bad_request", "method not allowed");
			return;
		}
		if (rel === "") {
			// The viewer resolves its files relative to the page, so it needs the trailing slash.
			res.redirect(302, `timeshade/${url.search}`);
			return;
		}
		const kind = classify(rel);
		if (!kind) {
			error(res, 404, "not_found", "not found");
			return;
		}
		if (kind === "viewer" && rel === "/" && !url.searchParams.has("data")) {
			// The viewer's API base defaults to /api/v3/; behind the proxy it is here.
			url.searchParams.set("data", `${PREFIX}/api/v3/`);
			if (token) {
				url.searchParams.set("token", token);
			}
			res.redirect(302, `./${url.search}`);
			return;
		}
		if (kind === "api") {
			const status = authorize(token, deps.secret(), deps.users);
			if (status !== 200) {
				error(res, status, status === 401 ? "unauthorized" : "forbidden",
					status === 401 ? "a valid Clusterio token is required" : "the timeshade.view permission is required");
				return;
			}
		}
		const up = deps.upstream();
		if (!up) {
			res.set("Retry-After", "5");
			error(res, 503, "unavailable", "the map server is not running");
			return;
		}
		forward(req, res, up, rel + url.search, kind, agent, timeoutMs);
	});
}

function forward(
	req: Request, res: Response, up: Upstream, path: string, kind: "viewer" | "api", agent: http.Agent, timeoutMs: number,
) {
	const target = new URL(up.url);
	const headers: http.OutgoingHttpHeaders = { authorization: `Bearer ${up.readToken}` };
	for (const name of REQUEST_HEADERS) {
		const v = req.headers[name];
		if (v !== undefined) {
			headers[name] = v;
		}
	}
	const live = path.startsWith("/api/v3/live?") || path === "/api/v3/live";
	const upReq = http.request({
		host: target.hostname, port: target.port, method: req.method, path, headers, agent: live ? false : agent,
	}, upRes => {
		const status = upRes.statusCode ?? 502;
		res.status(status);
		for (const name of RESPONSE_HEADERS) {
			const v = upRes.headers[name];
			if (v !== undefined) {
				// setHeader, not express's set(), which would add a charset to the content type.
				res.setHeader(name, v);
			}
		}
		const cc = upRes.headers["cache-control"];
		res.setHeader("Cache-Control", proxiedCacheControl(Array.isArray(cc) ? cc.join(", ") : cc, kind));
		if (/^text\/event-stream/.test(String(upRes.headers["content-type"] ?? ""))) {
			res.setHeader("X-Accel-Buffering", "no");
			res.flushHeaders();
		}
		if (live) {
			upReq.setTimeout(0);
		}
		upRes.pipe(res);
		upRes.on("error", () => res.destroy());
	});
	if (!live) {
		upReq.setTimeout(timeoutMs, () => upReq.destroy(new Error("upstream timeout")));
	}
	upReq.on("error", err => {
		if (!res.headersSent) {
			const timeout = err.message === "upstream timeout";
			error(res, timeout ? 504 : 502, "unavailable", timeout ? "the map server timed out" : "the map server is unreachable");
		} else {
			res.destroy();
		}
	});
	// The client went away: stop the upstream request (a live stream would otherwise run on).
	res.on("close", () => {
		if (!res.writableFinished) {
			upReq.destroy();
		}
	});
	upReq.end();
}
