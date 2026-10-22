// SPDX-License-Identifier: BlueOak-1.0.0
// The integrations wiring is ported from illisis/minimap controller.ts (Blue Oak Model License 1.0.0, see
// LICENSE-BlueOak-1.0.0.md).
//
// The controller side (PLAN.md §4.10): it runs the map server as a sidecar, forwards the instances' envelopes to its
// ingest, publishes the cluster's maps (titles, gridworld bounds, Universal Edges lines) as "map" messages, proxies
// the map behind Clusterio's auth, and mints view tokens for the web page.
import { randomBytes } from "node:crypto";
import * as path from "node:path";
import * as lib from "@clusterio/lib";
import type { ControllerPluginContext } from "@clusterio/controller";
import { findBinary } from "./binary.js";
import { addRoutes, mintViewToken } from "./http.js";
import { IngestQueue, type IngestQueueOptions } from "./ingest.js";
import {
	edgeLines, mapInfos, mapOf, tileOf, ueApi, type ClusterInstance, type EdgeApiLike, type Quad,
} from "./integrations.js";
import { EnvelopesEvent, ViewTokenRequest, ViewTokenResponse } from "./messages.js";
import { lifecycle } from "./runtime.js";
import { Sidecar, type SidecarOptions } from "./sidecar.js";

/** The source id of the controller's own envelopes (map messages). */
export const CONTROLLER_SRC = "clusterio:controller";
/** How often the maps are checked for changes; every 10 minutes they are sent whether changed or not. */
export const MAPS_EVERY_MS = 30_000;
const MAPS_RESEND_MS = 10 * 60_000;
const RESTART_FIELDS = new Set(["timeshade.binary_path", "timeshade.database_directory", "timeshade.mem_budget_mb"]);

export type ControllerOptions = {
	/** Run this instead of the timeshadedb binary (tests): the command and the arguments before "serve". */
	command?: string;
	commandArgs?: string[];
	/** More serve flags (tests: --clock data and short graces). */
	extraArgs?: string[];
	sidecar?: Partial<SidecarOptions>;
	queue?: Partial<IngestQueueOptions>;
	mapsEveryMs?: number;
	/** The clock stamped on map messages (tests that replay a recorded time with --clock data). */
	now?: () => number;
};

export default async function loadControllerPlugin(context: ControllerPluginContext, options: ControllerOptions = {}) {
	const { controller, logger, plugin } = context;
	// New at every start; only this process knows them. The sidecar keeps them across its own restarts.
	const ingestToken = randomBytes(24).toString("hex");
	const readToken = randomBytes(24).toString("hex");
	const pluginDir = path.resolve(import.meta.dirname, "..", "..");

	const dbDir = () => {
		const configured = controller.config.get("timeshade.database_directory");
		return configured
			? path.resolve(configured)
			: path.resolve(controller.config.get("controller.database_directory"), "timeshade");
	};
	const commandLine = (): { command: string; args: string[] } | Error => {
		let command: string;
		let prefix: string[] = [];
		if (options.command) {
			command = options.command;
			prefix = options.commandArgs ?? [];
		} else {
			const found = findBinary({ configured: controller.config.get("timeshade.binary_path"), pluginDir });
			if (found instanceof Error) {
				return found;
			}
			command = found.path;
		}
		const mem = Math.round(controller.config.get("timeshade.mem_budget_mb") * 1024 * 1024);
		return {
			command,
			args: [
				...prefix, "serve", "--db", dbDir(), "--listen", "127.0.0.1:0", "--ingest-token", ingestToken,
				"--read-token", readToken, "--mem-budget", String(mem), "--exit-on-stdin-close", ...(options.extraArgs ?? []),
			],
		};
	};

	const sidecar = new Sidecar({ command: "", args: [], logger, ...options.sidecar });
	const startSidecar = () => {
		const line = commandLine();
		if (line instanceof Error) {
			lifecycle(logger, "timeshade.sidecar", "no_binary", { error: line.message }, "error");
			return;
		}
		sidecar.configure(line.command, line.args);
		sidecar.start();
	};

	// Past timeshade.queue_mb the queue continues on disk next to the controller's database, and what it holds when the
	// controller stops is sent after the next start.
	const queue = new IngestQueue({
		target: () => (sidecar.url ? { url: sidecar.url, token: ingestToken } : undefined),
		logger,
		maxQueueBytes: controller.config.get("timeshade.queue_mb") * 1024 * 1024,
		spoolDir: path.resolve(controller.config.get("controller.database_directory"), "timeshade-queue"),
		maxDiskBytes: controller.config.get("timeshade.queue_disk_mb") * 1024 * 1024,
		...options.queue,
	});

	// The integrations, each logged once per start when first seen.
	let gridworld = false;
	let ue: EdgeApiLike | undefined;
	const instances = (): ClusterInstance[] => [...controller.instances.values()].map(i => {
		const tile = tileOf(i.config as unknown as { get(field: string): unknown });
		if (tile && !gridworld) {
			gridworld = true;
			lifecycle(logger, "timeshade.integration", "gridworld");
		}
		return { id: i.id, name: String(i.config.get("instance.name")), tile };
	});
	const edges = async (): Promise<Map<string, Quad[]>> => {
		if (!ue) {
			ue = await ueApi(controller.pluginInfos);
			if (ue) {
				lifecycle(logger, "timeshade.integration", "universal_edges");
			}
		}
		if (!ue) {
			return new Map();
		}
		// An edge side on its instance's tile surface is on the world's map; the instance's other tiled surfaces are
		// only known to the instance, so their edges stay on the instance's own map.
		return edgeLines(ue.list(), (id, surface) => {
			const config = controller.instances.get(id)?.config as unknown as { get(field: string): unknown } | undefined;
			return mapOf({ id, tile: config ? tileOf(config) : null }, surface);
		});
	};

	// Map messages (docs/PROTOCOL.md 3): sent when a map changed, and all of them every 10 minutes and after each
	// sidecar start. The controller's envelopes count ticks at 60 a second from its start, so they anchor cleanly.
	const clock = options.now ?? Date.now;
	const startedMs = clock();
	const sent = new Map<string, string>();
	let lastFull = 0;
	let mapSeq = Date.now() * 1000;
	async function publishMaps(full = false) {
		const now = clock();
		if (full || now - lastFull >= MAPS_RESEND_MS) {
			full = true;
			lastFull = now;
		}
		let list;
		try {
			list = mapInfos(instances(), await edges());
		} catch (err: any) {
			logger.warn(`timeshade: reading the cluster's maps failed: ${err.message}`);
			return;
		}
		const tick = Math.floor((now - startedMs) * 60 / 1000);
		for (const info of list) {
			const json = JSON.stringify(info);
			if (!full && sent.get(info.key) === json) {
				continue;
			}
			sent.set(info.key, json);
			queue.push(JSON.stringify({
				v: 1, tick, msgs: [{ type: "map", info }], src: CONTROLLER_SRC, map: info.key, recv_ms: now, seq: ++mapSeq,
			}));
		}
	}
	const publish = (full = false) => {
		publishMaps(full).catch(err => logger.error(`timeshade: publishing maps failed:\n${err?.stack ?? err}`));
	};

	sidecar.on("ready", () => {
		queue.wake();
		publish(true);
	});
	startSidecar();
	const mapsTimer = setInterval(publish, options.mapsEveryMs ?? MAPS_EVERY_MS);
	mapsTimer.unref?.();

	controller.handle(EnvelopesEvent, async (event: EnvelopesEvent, src: lib.Address) => {
		// The source and instance come from the link, never from the payload.
		for (const env of event.envelopes) {
			queue.push(JSON.stringify({ ...env, src: `clusterio:${src.id}`, instance: src.id }));
		}
	});
	controller.handle(ViewTokenRequest, async (_request: ViewTokenRequest, src: lib.Address) => {
		const user = controller.wsServer.controlConnections.get(src.id)?.user;
		if (!user) {
			throw new lib.RequestError("view tokens are only given to user connections");
		}
		const { token, expiresMs } = mintViewToken(user.name, controller.authSecret);
		return new ViewTokenResponse(token, expiresMs, sidecar.state === "running");
	});
	addRoutes(controller.app, {
		upstream: () => (sidecar.url ? { url: sidecar.url, readToken } : undefined),
		secret: () => controller.authSecret,
		users: controller.users,
	});

	controller.hooks.instanceConfigFieldChanged.attach(plugin.name, async (_instance, field: string) => {
		if (field === "gridworld.tile" || field === "instance.name") {
			await publishMaps();
		}
	});
	controller.hooks.instanceStatusChanged.attach(plugin.name, async (_instance, prev) => {
		if (prev === undefined) {
			await publishMaps();
		}
	});
	controller.hooks.controllerConfigFieldChanged.attach(plugin.name, async (field: string) => {
		if (RESTART_FIELDS.has(field)) {
			lifecycle(logger, "timeshade.sidecar", "restart", { field });
			await sidecar.stop();
			startSidecar();
		} else if (field === "timeshade.queue_mb") {
			queue.setMaxQueueBytes(controller.config.get("timeshade.queue_mb") * 1024 * 1024);
		}
	});
	// Clusterio waits up to 15 s for this hook: up to 3 s to hand over what is queued (the rest stays on disk for the
	// next start), then the sidecar stops (its stdin closes; it is killed after 10 s).
	controller.hooks.shutdown.attach(plugin.name, async () => {
		clearInterval(mapsTimer);
		await queue.drain(3000);
		queue.close();
		await sidecar.stop();
	});

	return { sidecar, queue, publishMaps, tokens: { ingestToken, readToken } };
}
