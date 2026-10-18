// Grid and id helpers (docs/v3/FORMAT.md sections 1 and 2). Mirrors internal/v3/core
// keys.go and grid.go. No imports, so `node --test` can load it directly.

export const CHUNK_SIZE = 32;
export const TILE_CHUNKS = 16;
export const TILE_SIZE = 512;
/** World tile where tile (0, 0) starts at every level. */
export const GRID_ORIGIN = -256;
export const MIN_Z = -10;
export const MAX_COORD = 2048;

/** World tiles per pixel side at level z (2^-z). */
export function scale(z: number): number {
  return 2 ** -z;
}

/** World tiles a tile at level z covers per side. */
export function tileSpan(z: number): number {
  return TILE_SIZE * scale(z);
}

/** The tile at level z holding world position (x, y). */
export function tileAt(x: number, y: number, z: number): { z: number; x: number; y: number } {
  const span = tileSpan(z);
  return { z, x: Math.floor((x - GRID_ORIGIN) / span), y: Math.floor((y - GRID_ORIGIN) / span) };
}

/** World coordinates of a tile's top-left corner. */
export function tileOrigin(t: { z: number; x: number; y: number }): [number, number] {
  const span = tileSpan(t.z);
  return [GRID_ORIGIN + t.x * span, GRID_ORIGIN + t.y * span];
}

export function parentTile(t: { z: number; x: number; y: number }): { z: number; x: number; y: number } {
  return { z: t.z - 1, x: Math.floor(t.x / 2), y: Math.floor(t.y / 2) };
}

/** Children in the order top-left, top-right, bottom-left, bottom-right. */
export function childTiles(t: { z: number; x: number; y: number }): { z: number; x: number; y: number }[] {
  if (t.z >= 0) {
    throw new Error("z0 tiles have no children");
  }
  const z = t.z + 1;
  return [
    { z, x: 2 * t.x, y: 2 * t.y },
    { z, x: 2 * t.x + 1, y: 2 * t.y },
    { z, x: 2 * t.x, y: 2 * t.y + 1 },
    { z, x: 2 * t.x + 1, y: 2 * t.y + 1 },
  ];
}

/** Whether a tile is inside the addressable grid. */
export function validTile(t: { z: number; x: number; y: number }): boolean {
  if (t.z > 0 || t.z < MIN_Z || !Number.isInteger(t.z) || !Number.isInteger(t.x) || !Number.isInteger(t.y)) {
    return false;
  }
  const lim = Math.max(1, MAX_COORD >> -t.z);
  return Math.abs(t.x) <= lim && Math.abs(t.y) <= lim;
}

/** The z0 tile and block (0..255) of chunk (cx, cy). */
export function chunkTile(cx: number, cy: number): { tile: { z: 0; x: number; y: number }; block: number } {
  const ox = cx + 8;
  const oy = cy + 8;
  const tx = Math.floor(ox / TILE_CHUNKS);
  const ty = Math.floor(oy / TILE_CHUNKS);
  return { tile: { z: 0, x: tx, y: ty }, block: ox - tx * TILE_CHUNKS + (oy - ty * TILE_CHUNKS) * TILE_CHUNKS };
}

/** Index into a 512x512 tile of pixel pos (0..1023) of block b. */
export function tilePixelIndex(block: number, pos: number): number {
  const bx = block % TILE_CHUNKS;
  const by = Math.floor(block / TILE_CHUNKS);
  return (by * CHUNK_SIZE + Math.floor(pos / CHUNK_SIZE)) * TILE_SIZE + bx * CHUNK_SIZE + (pos % CHUNK_SIZE);
}

/** EscapeName: keep [a-z0-9_-], write every other UTF-8 byte as ~XX (upper-case hex). */
export function escapeName(s: string): string {
  let out = "";
  for (const b of new TextEncoder().encode(s)) {
    const c = String.fromCharCode(b);
    if ((b >= 0x61 && b <= 0x7a) || (b >= 0x30 && b <= 0x39) || c === "_" || c === "-") {
      out += c;
    } else {
      out += "~" + b.toString(16).toUpperCase().padStart(2, "0");
    }
  }
  return out;
}

/** Inverse of escapeName; throws on non-canonical input. */
export function unescapeName(s: string): string {
  if (s === "") {
    throw new Error("empty name");
  }
  const bytes: number[] = [];
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (/[a-z0-9_-]/.test(c)) {
      bytes.push(c.charCodeAt(0));
    } else if (c === "~" && /^[0-9A-F]{2}$/.test(s.slice(i + 1, i + 3))) {
      bytes.push(parseInt(s.slice(i + 1, i + 3), 16));
      i += 2;
    } else {
      throw new Error(`bad name ${JSON.stringify(s)}`);
    }
  }
  const out = new TextDecoder("utf-8", { fatal: true }).decode(new Uint8Array(bytes));
  if (escapeName(out) !== s) {
    throw new Error(`non-canonical name ${JSON.stringify(s)}`);
  }
  return out;
}

export function datastoreId(k: { map: string; surface: string; force: string }): string {
  return `${escapeName(k.map)}.${escapeName(k.surface)}.${escapeName(k.force)}`;
}

export function surfaceId(k: { map: string; surface: string }): string {
  return `${escapeName(k.map)}.${escapeName(k.surface)}`;
}

export function parseDatastoreId(id: string): { map: string; surface: string; force: string } {
  const p = id.split(".");
  if (p.length !== 3) {
    throw new Error(`bad datastore id ${JSON.stringify(id)}`);
  }
  return { map: unescapeName(p[0]), surface: unescapeName(p[1]), force: unescapeName(p[2]) };
}

/** UTC day name of a time, as day indexes and timelines are named. */
export function utcDay(t: number): string {
  return new Date(t).toISOString().slice(0, 10);
}
