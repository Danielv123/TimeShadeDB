// SPDX-License-Identifier: BlueOak-1.0.0
// Ported from illisis/minimap integrations.ts (Blue Oak Model License 1.0.0, see LICENSE-BlueOak-1.0.0.md).
//
// The optional integrations: gridworld's tile config, read from each instance's config, and Universal
// Edges' edges, through UE's own module file. Neither is a dependency; both are found at run time. Map keys are raw
// strings (docs/PROTOCOL.md 1): `gw-<world>` for a gridworld world, `i-<instance id>` for everything else, the same
// keys the illisis importer produces (it unescapes illisis's directory names).
import * as path from "node:path";
import { pathToFileURL } from "node:url";

/** The fields of gridworld.tile the map reads (gridworld/world/contracts.ts). Lua gets the whole object. */
export type Tile = { world: string; x: number; y: number; size: number; surface: string; exclude?: string[] };
export type ClusterInstance = { id: number; name: string; tile: Tile | null };

/** A rectangle or segment [x1, y1, x2, y2] in world tiles (core.Quad). */
export type Quad = [number, number, number, number];

/** core.InstanceInfo. */
export type InstanceInfo = { id: string; name: string; bounds?: Quad };

/** core.MapInfo, the payload of a "map" message (docs/PROTOCOL.md 3). */
export type MapInfo = {
	key: string;
	title: string;
	tile_size?: number;
	instances: InstanceInfo[];
	edges?: Record<string, Quad[]>;
};

/**
 * gridworld.tile, or null: unset, or gridworld not loaded, which Clusterio's config reports by throwing "No field
 * named" (clusterio/packages/lib/src/config/classes.ts).
 */
export function tileOf(config: { get(field: string): unknown }): Tile | null {
	let tile: unknown;
	try {
		tile = config.get("gridworld.tile");
	} catch {
		return null;
	}
	if (!tile || typeof tile !== "object") {
		return null;
	}
	const t = tile as Partial<Tile>;
	const int = (v: unknown) => typeof v === "number" && Number.isInteger(v);
	if (typeof t.world !== "string" || typeof t.surface !== "string" || !int(t.x) || !int(t.y) || !int(t.size)) {
		return null;
	}
	return tile as Tile;
}

/** The map key of a gridworld world. */
export const worldMap = (world: string) => `gw-${world}`;
/** The map key of an instance's own surfaces. */
export const instanceMap = (id: number) => `i-${id}`;

/**
 * A tile instance's tiled surfaces are its world's map: its tile.surface always, and each surface its capture marked
 * tiled in a surfaces message (`tiled`). Every other surface, and every surface of an instance that is no tile, is the
 * instance's own map.
 */
export function mapOf(
	instance: { id: number; tile: Tile | null }, surface: string, tiled?: ReadonlySet<string>,
): string {
	return instance.tile && (surface === instance.tile.surface || tiled?.has(surface))
		? worldMap(instance.tile.world)
		: instanceMap(instance.id);
}

/**
 * The catalog's titles, tile sizes, instance bounds and edges, from the instances as they are now. Every instance has
 * its own map (its non-tiled surfaces); every gridworld world has one more.
 */
export function mapInfos(instances: ClusterInstance[], edges: Map<string, Quad[]> = new Map()): MapInfo[] {
	const maps = new Map<string, MapInfo>();
	for (const i of [...instances].sort((a, b) => a.id - b.id)) {
		maps.set(instanceMap(i.id), { key: instanceMap(i.id), title: i.name, instances: [{ id: String(i.id), name: i.name }] });
		if (i.tile) {
			const { world, x, y, size } = i.tile;
			const h = size / 2;
			const key = worldMap(world);
			let info = maps.get(key);
			if (!info) {
				info = { key, title: `Gridworld ${world}`, tile_size: size, instances: [] };
				maps.set(key, info);
			}
			// gridworld/world/grid.ts: [xT - T/2, xT + T/2) on each axis.
			info.instances.push({ id: String(i.id), name: i.name, bounds: [(x * size) - h, (y * size) - h, (x * size) + h, (y * size) + h] });
		}
	}
	for (const [mapSurface, quads] of edges) {
		const cut = mapSurface.indexOf("|");
		const info = maps.get(mapSurface.slice(0, cut));
		if (info) {
			(info.edges ??= {})[mapSurface.slice(cut + 1)] = quads;
		}
	}
	return [...maps.values()].sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
}

/** rot(1, 0, facing) (gridworld/world/grid.ts): the direction a side runs along its edge. */
const ALONG: Record<number, [number, number]> = { 0: [1, 0], 4: [0, 1], 8: [-1, 0], 12: [0, -1] };

/** The part of UE's edge record the lines need (universal_edges/edges/contracts.ts). */
type SideLike = { instanceId: number; surface: string; origin: [number, number]; facing: number };
export type EdgeRecordLike = { deleted: boolean; length: number; a: SideLike; b: SideLike };
export type EdgeApiLike = { list(): EdgeRecordLike[] };

/**
 * UE's live edges as lines: each side from its origin along rot(1, 0, facing) for the edge's length, on its own
 * instance's map and surface, keyed "<map>|<surface>". Endpoints are put in order, so gridworld's coincident sides
 * draw once.
 */
export function edgeLines(
	records: EdgeRecordLike[],
	mapOfSide: (instance: number, surface: string) => string,
): Map<string, Quad[]> {
	const lines = new Map<string, Map<string, Quad>>();
	for (const r of records.filter(x => !x.deleted)) {
		for (const side of [r.a, r.b]) {
			const along = ALONG[side.facing];
			if (!along) {
				continue;
			}
			const [dx, dy] = along;
			const [x, y] = side.origin;
			const ends = [[x, y], [x + (r.length * dx), y + (r.length * dy)]]
				.sort((p, q) => p[0] - q[0] || p[1] - q[1]);
			const line = [...ends[0], ...ends[1]] as Quad;
			const key = `${mapOfSide(side.instanceId, side.surface)}|${side.surface}`;
			if (!lines.has(key)) {
				lines.set(key, new Map());
			}
			lines.get(key)!.set(line.join(), line);
		}
	}
	return new Map([...lines].map(([key, m]) => [key, [...m.values()]]));
}

/**
 * UE's edgeApi() (universal_edges/edges/index.ts), imported from the file Clusterio's loader imports, so both share
 * its singleton. Undefined while UE isn't in the plugin
 * list, isn't loaded, or hasn't set its API: the caller asks again later.
 */
export async function ueApi(infos: { name: string; packagePath: string }[]): Promise<EdgeApiLike | undefined> {
	const ue = infos.find(p => p.name === "universal_edges");
	if (!ue) {
		return undefined;
	}
	try {
		// Entrypoints are imported from the package's directory (plugin_loader.ts loadPlugin), so this URL is the one
		// Clusterio's import cached.
		const file = path.join(path.dirname(ue.packagePath), "dist", "node", "edges", "index.js");
		const mod = await import(pathToFileURL(file).href);
		const api = mod.edgeApi?.();
		return api && typeof api.list === "function" ? api as EdgeApiLike : undefined;
	} catch {
		return undefined;
	}
}
