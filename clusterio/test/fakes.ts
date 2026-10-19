// Helpers for the plugin's tests: Clusterio fakes (logger, instance and controller contexts) and small utilities.
// Not a test file: the runner only picks up *.test.js.
import { mkdtemp } from "node:fs/promises";
import * as http from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import express from "express";
import * as lib from "@clusterio/lib";

export type Line = { level: string; message: string; meta: Record<string, any> };

/** A logger that keeps its lines. */
export function fakeLogger(lines: Line[] = []): lib.Logger {
	const log = (level: string, message: string, meta: Record<string, any> = {}) => {
		lines.push({ level, message, meta });
	};
	return {
		log,
		info: (message: string, meta?: Record<string, any>) => log("info", message, meta),
		verbose: (message: string, meta?: Record<string, any>) => log("verbose", message, meta),
		warn: (message: string, meta?: Record<string, any>) => log("warn", message, meta),
		error: (message: string, meta?: Record<string, any>) => log("error", message, meta),
	} as unknown as lib.Logger;
}

export const tempDir = (prefix: string) => mkdtemp(join(tmpdir(), `tsdb-clusterio-${prefix}-`));

export async function until(check: () => boolean | Promise<boolean>, ms = 5000, what = "condition"): Promise<void> {
	const deadline = Date.now() + ms;
	while (!(await check())) {
		if (Date.now() > deadline) {
			throw new Error(`timed out waiting for ${what}`);
		}
		await new Promise(resolve => setTimeout(resolve, 10));
	}
}

export const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms));

type Hook = (...args: any[]) => any;
/** A hook collection: attach() keeps one handler per hook, call(name, ...) runs it. */
export function fakeHooks(names: string[]) {
	const handlers: Record<string, Hook> = {};
	const hooks: Record<string, { attach(owner: string, fn: Hook): void }> = {};
	for (const name of names) {
		hooks[name] = {
			attach: (_owner: string, fn: Hook) => {
				handlers[name] = fn;
			},
		};
	}
	return { hooks, call: async (name: string, ...args: any[]) => handlers[name]?.(...args), handlers };
}

/** A config: get() of a missing field throws as Clusterio's does ("No field named"). */
export function fakeConfig(fields: Record<string, unknown>) {
	return {
		fields,
		get(name: string) {
			if (!(name in fields)) {
				throw new lib.InvalidField(`No field named '${name}'`);
			}
			return fields[name];
		},
		set(name: string, value: unknown) {
			fields[name] = value;
		},
	};
}

/** A user as the controller's UserManager returns it. */
export function fakeUser(name: string, permissions: string[], tokenValidAfter = 0) {
	return {
		name,
		tokenValidAfter,
		checkPermission(permission: string) {
			if (!permissions.includes(permission)) {
				throw new lib.PermissionError("Permission denied");
			}
		},
	};
}

export type FakeInstance = ReturnType<typeof fakeInstanceContext>;

/** An instance with what the plugin touches: its IPC handler, sendTo, sendRcon, four hooks, config and status. */
export function fakeInstanceContext(id: number, fields: Record<string, unknown> = {}, send?: (event: any) => void) {
	const lines: Line[] = [];
	const sent: any[] = [];
	const rcon: string[] = [];
	const { hooks, call } = fakeHooks(["start", "exit", "instanceConfigFieldChanged", "controllerConnectionEvent"]);
	let ipc: Hook | undefined;
	const config = fakeConfig({ "instance.name": `inst${id}`, "timeshade.capture_rails": false, ...fields });
	const instance = {
		id,
		status: "running",
		config,
		server: {
			handle: (channel: string, fn: Hook) => {
				if (channel !== "timeshade:out") {
					throw new Error(`unexpected channel ${channel}`);
				}
				ipc = fn;
			},
		},
		sendTo: (dst: string, event: any) => {
			if (dst !== "controller") {
				throw new Error(`unexpected destination ${dst}`);
			}
			sent.push(event);
			send?.(event);
		},
		sendRcon: async (command: string, expectEmpty: boolean, plugin: string) => {
			if (!expectEmpty || plugin !== "timeshade") {
				throw new Error("sendRcon called without expectEmpty or the plugin name");
			}
			rcon.push(command);
			return "";
		},
		hooks,
	};
	const context = { instance, logger: fakeLogger(lines), plugin: { name: "timeshade" } } as any;
	return { context, instance, lines, sent, rcon, call, ipc: (data: unknown) => ipc!(data) };
}

/** A running express app on a random port. */
export async function listen(app: http.RequestListener) {
	const server = http.createServer(app);
	await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
	const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
	return {
		server,
		url,
		close: () => new Promise<void>(resolve => {
			server.closeAllConnections();
			server.close(() => resolve());
		}),
	};
}

export type FakeController = Awaited<ReturnType<typeof fakeControllerContext>>;

/**
 * A controller with what the plugin touches: config, instances, users, the auth secret, a real express app on a
 * random port, handle() for link messages, control connections and hooks.
 */
export async function fakeControllerContext(fields: Record<string, unknown> = {}) {
	const lines: Line[] = [];
	const dir = await tempDir("controller");
	const config = fakeConfig({
		"controller.database_directory": dir,
		"timeshade.binary_path": null,
		"timeshade.database_directory": null,
		"timeshade.mem_budget_mb": 64,
		"timeshade.queue_mb": 64,
		...fields,
	});
	const handlers = new Map<any, Hook>();
	const instances = new Map<number, { id: number; config: ReturnType<typeof fakeConfig> }>();
	const users = new Map<string, ReturnType<typeof fakeUser>>();
	const controls = new Map<number, { user: ReturnType<typeof fakeUser> }>();
	const { hooks, call } = fakeHooks([
		"instanceConfigFieldChanged", "instanceStatusChanged", "controllerConfigFieldChanged", "shutdown",
	]);
	const app = express();
	const authSecret = Buffer.from("0123456789abcdef0123456789abcdef");
	const controller = {
		config,
		app,
		authSecret,
		pluginInfos: [] as { name: string; packagePath: string }[],
		instances: {
			get: (id: number) => instances.get(id),
			values: () => instances.values(),
		},
		users: { getByName: (name: string) => users.get(name) },
		wsServer: { controlConnections: controls },
		handle: (Class: any, fn: Hook) => {
			handlers.set(Class, fn);
		},
		hooks,
	};
	const web = await listen(app as unknown as http.RequestListener);
	const context = { controller, logger: fakeLogger(lines), plugin: { name: "timeshade" } } as any;
	return {
		context, controller, lines, dir, web, users, instances, controls, call,
		addInstance(id: number, fields: Record<string, unknown> = {}) {
			const record = { id, config: fakeConfig({ "instance.name": `inst${id}`, ...fields }) };
			instances.set(id, record);
			return record;
		},
		/** Deliver a link message to the handler the plugin registered. */
		deliver(Class: any, message: any, src: { type: string; id: number }) {
			const fn = handlers.get(Class);
			if (!fn) {
				throw new Error(`no handler for ${Class.name}`);
			}
			return fn(message, src);
		},
	};
}
