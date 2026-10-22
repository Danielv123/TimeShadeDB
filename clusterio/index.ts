import type * as lib from "@clusterio/lib";
import { EnvelopesEvent, ViewTokenRequest } from "./messages.js";

declare module "@clusterio/lib" {
	export interface ControllerConfigFields {
		"timeshade.binary_path": string | null;
		"timeshade.database_directory": string | null;
		"timeshade.mem_budget_mb": number;
		"timeshade.queue_mb": number;
		"timeshade.queue_disk_mb": number;
	}
	export interface InstanceConfigFields {
		"timeshade.capture_rails": boolean;
		"timeshade.extra_forces": string;
	}
	export interface Permissions {
		"timeshade.view": never;
	}
}

export const plugin: lib.PluginDeclaration = {
	// The save-patch module in module/ must have the plugin's name (docs/writing-plugins.md).
	name: "timeshade",
	title: "TimeShadeDB map",
	description: "One map of the cluster from the game's own chart: terrain, trains, vehicles, players, tags and " +
		"recipes, live and through time, stored by a TimeShadeDB server the controller runs.",
	controllerEntrypoint: "dist/node/controller.js",
	instanceEntrypoint: "dist/node/instance.js",
	features: ["SavePatching", "ScriptCommands"],
	controllerConfigFields: {
		"timeshade.binary_path": {
			title: "TimeShadeDB binary",
			description: "Path to the timeshadedb executable. Unset: the TIMESHADEDB_BINARY environment variable, " +
				"then the binary package for this platform (an optional dependency of the plugin), then bin/ in the " +
				"plugin directory. A change restarts the map server.",
			type: "string",
			optional: true,
		},
		"timeshade.database_directory": {
			title: "Database directory",
			description: "Where the map is stored. Unset: timeshade/ in the controller's database directory. A change " +
				"restarts the map server (the old directory is left as it is).",
			type: "string",
			optional: true,
		},
		"timeshade.mem_budget_mb": {
			title: "Memory budget (MiB)",
			description: "The map server's cache and buffer budget (serve --mem-budget). A change restarts it.",
			type: "number",
			initialValue: 256,
			validator: (value: number) => {
				if (!(value >= 32)) {
					throw new Error("timeshade.mem_budget_mb must be at least 32");
				}
			},
		},
		"timeshade.queue_mb": {
			title: "Forwarding queue (MiB)",
			description: "Capture data kept in memory while the map server is down or slow; past it the queue continues " +
				"on disk (timeshade-queue/ in the controller's database directory).",
			type: "number",
			initialValue: 64,
			validator: (value: number) => {
				if (!(value >= 1)) {
					throw new Error("timeshade.queue_mb must be at least 1");
				}
			},
		},
		"timeshade.queue_disk_mb": {
			title: "Forwarding queue on disk (MiB)",
			description: "Capture data kept on disk past the memory queue, also across controller restarts; past it the " +
				"oldest is dropped.",
			type: "number",
			initialValue: 4096,
			validator: (value: number) => {
				if (!(value >= 1)) {
					throw new Error("timeshade.queue_disk_mb must be at least 1");
				}
			},
		},
	},
	instanceConfigFields: {
		"timeshade.capture_rails": {
			title: "Capture rails",
			description: "Also record rail entities (an optional map layer; trains can be matched to the track).",
			type: "boolean",
			initialValue: false,
		},
		"timeshade.extra_forces": {
			title: "Extra forces",
			description: "Comma-separated forces to capture besides those with players (which are always captured), " +
				"e.g. \"player\" on a server nobody has joined yet.",
			type: "string",
			initialValue: "",
		},
	},
	messages: [EnvelopesEvent, ViewTokenRequest],
	permissions: [
		{
			name: "timeshade.view",
			title: "View the map",
			description: "See the TimeShadeDB map page and read its data through the controller.",
			grantByDefault: true,
		},
	],
	webEntrypoint: "./web",
	routes: ["/timeshade"],
};
