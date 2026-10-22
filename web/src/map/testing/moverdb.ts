// Serves the movers of a real v3 database (written by `timeshadedb import-minimap --db`) next
// to the mock terrain, so the movers layer can be checked in a browser before the v3 server
// (WP6) exists. Reads the WP4 on-disk layout (FORMAT.md 5.5, internal/movers/doc.go):
//
//   <db>/movers/<surface id>/meta.json, days/<day>.json, windows/<day>/<start>.mvw,
//   keyframes/<day>/<t>.mvw (one zstd frame of an MVW3 body each)
//
// and <db>/maps.json (instances and UE edges for the catalog). Node only (node:zlib zstd).

import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { zstdDecompressSync } from "node:zlib";
import type { CatalogMovers, MapInfo } from "../../api/types.ts";
import type { MockDataset } from "./dataset.ts";

export interface MoverDb {
  dir: string;
}

function utcDay(t: number): string {
  return new Date(t).toISOString().slice(0, 10);
}

/** The catalog mover summary of a surface, or null when the database has no movers for it. */
export function moverSummary(db: MoverDb, sfc: string): CatalogMovers | null {
  const base = join(db.dir, "movers", sfc);
  if (!existsSync(join(base, "meta.json"))) {
    return null;
  }
  const meta = JSON.parse(readFileSync(join(base, "meta.json"), "utf8")) as { horizon: number; first: number; last: number };
  const days = existsSync(join(base, "days"))
    ? readdirSync(join(base, "days"))
        .filter(f => f.endsWith(".json"))
        .map(f => f.slice(0, -5))
        .sort()
    : [];
  return { horizon: meta.horizon, first: meta.first, last: meta.last, days };
}

/**
 * Adds the database's mover summaries, instances and edges to the dataset's catalog, for the
 * surfaces the catalog already has.
 */
export function attachMoverDb(data: MockDataset, db: MoverDb): void {
  const mapsPath = join(db.dir, "maps.json");
  const maps = existsSync(mapsPath) ? (JSON.parse(readFileSync(mapsPath, "utf8")) as MapInfo[]) : [];
  for (const m of data.catalog.maps) {
    const info = maps.find(x => x.key === m.key);
    if (info) {
      m.instances = info.instances ?? [];
      for (const s of m.surfaces) {
        s.edges = info.edges?.[s.name] ?? s.edges;
      }
    }
    for (const s of m.surfaces) {
      s.movers = moverSummary(db, s.id);
    }
  }
}

/** Answers a `movers/...` route (API.md 3) from the database: body and cache class, or null (404). */
export function moverRoute(db: MoverDb, parts: string[]): { body: Uint8Array; type: string; cache: string } | null {
  const [, sfc, kind, file] = parts;
  if (!sfc || sfc.includes("..") || !kind) {
    return null;
  }
  const base = join(db.dir, "movers", sfc);
  const imm = "public, max-age=31536000, immutable";
  try {
    if (kind === "day" && file?.endsWith(".json")) {
      const date = file.slice(0, -5);
      const p = join(base, "days", `${date}.json`);
      if (!/^\d{4}-\d{2}-\d{2}$/.test(date) || !existsSync(p)) {
        return null;
      }
      const meta = moverSummary(db, sfc);
      const day = JSON.parse(readFileSync(p, "utf8")) as Record<string, unknown>;
      // FORMAT.md: "final" once the horizon passed the end of the day.
      day.final = !!meta && meta.horizon >= Date.parse(`${date}T00:00:00Z`) + 86_400_000;
      return { body: new TextEncoder().encode(JSON.stringify(day)), type: "application/json", cache: day.final ? imm : "public, max-age=10" };
    }
    if ((kind === "window" || kind === "keyframe") && file?.endsWith(".bin")) {
      const t = Number(file.slice(0, -4));
      if (!Number.isSafeInteger(t)) {
        return null;
      }
      const p = join(base, kind === "window" ? "windows" : "keyframes", utcDay(t), `${t}.mvw`);
      if (!existsSync(p)) {
        return null;
      }
      return { body: new Uint8Array(zstdDecompressSync(readFileSync(p))), type: "application/octet-stream", cache: imm };
    }
  } catch {
    return null;
  }
  return null;
}
