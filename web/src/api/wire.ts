// Decoders for the v3 wire formats (docs/v3/FORMAT.md section 7): TSV3 tiles, TSD3 tile
// deltas, TSP3 palettes and MVW3 mover windows. Reference: internal/v3/core/wire (Go).
// Bodies arrive already decompressed (HTTP Content-Encoding). Safe to use in workers.
// This file imports types only, so `node --test` can load it directly (wire.test.ts).

import type {
  Carriage,
  DecodedDelta,
  DecodedTile,
  DeltaHeader,
  MoverMeta,
  MoverRow,
  MoverWindow,
  TileFormat,
  TileHeader,
  WindowKind,
} from "./types";

export const TILE_SIZE = 512;
export const TILE_PIXELS = TILE_SIZE * TILE_SIZE;
export const CHUNK_PIXELS = 1024;
export const HEADER_SIZE = 32;
/** Position units per tile and orientation units per turn in MVW3. */
export const POS_UNITS = 16;
export const ORIENT_UNITS = 65536;

const MOVER_HAS_TICKS = 1;
const WIRE_HAS_CARRIAGES = 1 << 6;
const FLAG_GONE = 1 << 1;
const FLAG_NO_TICK = 1 << 4;

export class WireError extends Error {
  constructor(message: string) {
    super(`wire: ${message}`);
    this.name = "WireError";
  }
}

function view(buf: ArrayBuffer | ArrayBufferView): DataView {
  return ArrayBuffer.isView(buf) ? new DataView(buf.buffer, buf.byteOffset, buf.byteLength) : new DataView(buf);
}

function magic(v: DataView, offset: number): string {
  return String.fromCharCode(v.getUint8(offset), v.getUint8(offset + 1), v.getUint8(offset + 2), v.getUint8(offset + 3));
}

function pixelBytes(format: number): number {
  return format === 1 ? 1 : 2;
}

/**
 * Pixels of a body region as a typed array. Pal8 is a zero-copy view; 16-bit formats are a
 * zero-copy view when the offset is 2-aligned (always true for whole tile bodies), else a copy.
 */
function pixelArray(v: DataView, offset: number, count: number, format: number): Uint8Array | Uint16Array {
  const start = v.byteOffset + offset;
  if (format === 1) {
    return new Uint8Array(v.buffer, start, count);
  }
  if (start % 2 === 0) {
    return new Uint16Array(v.buffer, start, count);
  }
  const out = new Uint16Array(count);
  for (let i = 0; i < count; i++) {
    out[i] = v.getUint16(offset + 2 * i, true);
  }
  return out;
}

export function decodeTileHeader(buf: ArrayBuffer | ArrayBufferView): TileHeader {
  const v = view(buf);
  if (v.byteLength < HEADER_SIZE || magic(v, 0) !== "TSV3") {
    throw new WireError("not a TSV3 tile");
  }
  const format = v.getUint8(4);
  if (format < 1 || format > 3) {
    throw new WireError(`tile format ${format}`);
  }
  if (v.getUint16(24, true) !== TILE_SIZE || v.getUint16(26, true) !== TILE_SIZE) {
    throw new WireError("tile size");
  }
  return {
    format: format as TileFormat,
    z: v.getInt8(5),
    live: (v.getUint8(6) & 1) !== 0,
    x: v.getInt32(8, true),
    y: v.getInt32(12, true),
    version: v.getUint32(16, true),
    paletteVersion: v.getUint32(20, true),
  };
}

/** Decodes a TSV3 tile. Pixels are a view into `buf` (no copy) when possible. */
export function decodeTile(buf: ArrayBuffer | ArrayBufferView): DecodedTile {
  const header = decodeTileHeader(buf);
  const v = view(buf);
  if (v.byteLength !== HEADER_SIZE + TILE_PIXELS * pixelBytes(header.format)) {
    throw new WireError(`tile body length ${v.byteLength}`);
  }
  return { header, pixels: pixelArray(v, HEADER_SIZE, TILE_PIXELS, header.format) };
}

export function decodeTileDelta(buf: ArrayBuffer | ArrayBufferView): DecodedDelta {
  const v = view(buf);
  if (v.byteLength < HEADER_SIZE || magic(v, 0) !== "TSD3") {
    throw new WireError("not a TSD3 delta");
  }
  const format = v.getUint8(4);
  if (format < 1 || format > 3 || v.getInt8(5) !== 0) {
    throw new WireError(`delta format ${format}`);
  }
  const header: DeltaHeader = {
    format: format as TileFormat,
    count: v.getUint16(6, true),
    x: v.getInt32(8, true),
    y: v.getInt32(12, true),
    from: v.getUint32(16, true),
    to: v.getUint32(20, true),
    paletteVersion: v.getUint32(24, true),
  };
  const entry = 4 + CHUNK_PIXELS * pixelBytes(format);
  if (header.count > 256 || v.byteLength !== HEADER_SIZE + entry * header.count) {
    throw new WireError(`delta length ${v.byteLength} for ${header.count} entries`);
  }
  const entries: DecodedDelta["entries"] = [];
  for (let i = 0; i < header.count; i++) {
    const o = HEADER_SIZE + i * entry;
    const block = v.getUint16(o, true);
    if (block > 255 || (i > 0 && block <= entries[i - 1].block)) {
      throw new WireError(`delta block ${block}`);
    }
    entries.push({ block, pixels: pixelArray(v, o + 4, CHUNK_PIXELS, format) });
  }
  return { header, entries };
}

/** Decodes a TSP3 palette to RGB565 colours. */
export function decodePalette(buf: ArrayBuffer | ArrayBufferView): Uint16Array {
  const v = view(buf);
  if (v.byteLength < 8 || magic(v, 0) !== "TSP3") {
    throw new WireError("not a TSP3 palette");
  }
  const n = v.getUint32(4, true);
  if (n < 1 || n > 65536 || v.byteLength !== 8 + 2 * n) {
    throw new WireError(`palette of ${n} entries in ${v.byteLength} bytes`);
  }
  const out = new Uint16Array(n);
  for (let i = 0; i < n; i++) {
    out[i] = v.getUint16(8 + 2 * i, true);
  }
  if (out[0] !== 0) {
    throw new WireError("palette entry 0 is not 0");
  }
  return out;
}

/** Expands RGB565 to [r, g, b] with bit replication (FORMAT.md 3). */
export function rgb565ToRgb(c: number): [number, number, number] {
  const r = (c >> 11) & 31;
  const g = (c >> 5) & 63;
  const b = c & 31;
  return [(r << 3) | (r >> 2), (g << 2) | (g >> 4), (b << 3) | (b >> 2)];
}

/** The RGB565 colour of pixel i of a decoded tile or delta entry. */
export function pixelColor(pixels: Uint8Array | Uint16Array, format: TileFormat, i: number, palette: Uint16Array | null): number {
  const v = pixels[i];
  if (format === 3) {
    return v;
  }
  if (!palette || v >= palette.length) {
    throw new WireError(`palette index ${v} beyond palette`);
  }
  return palette[v];
}

class Column {
  private off = 0;
  private readonly bytes: Uint8Array;

  constructor(bytes: Uint8Array) {
    this.bytes = bytes;
  }

  uvarint(): number {
    // Values fit in 53 bits in practice (ticks, times); use multiplication, not shifts.
    let result = 0;
    let mul = 1;
    for (let i = 0; i < 10; i++) {
      if (this.off >= this.bytes.length) {
        throw new WireError("bad varint");
      }
      const b = this.bytes[this.off++];
      result += (b & 0x7f) * mul;
      if (b < 0x80) {
        return result;
      }
      mul *= 128;
    }
    throw new WireError("bad varint");
  }

  varint(): number {
    const u = this.uvarint();
    // zigzag: even -> u/2, odd -> -(u+1)/2
    return u % 2 === 0 ? u / 2 : -(u + 1) / 2;
  }

  byte(): number {
    if (this.off >= this.bytes.length) {
      throw new WireError("column too short");
    }
    return this.bytes[this.off++];
  }

  done(): boolean {
    return this.off === this.bytes.length;
  }
}

/** Decodes an MVW3 body. */
export function decodeMoverWindow(buf: ArrayBuffer | ArrayBufferView): MoverWindow {
  const v = view(buf);
  if (v.byteLength < 32 || magic(v, 0) !== "MVW3") {
    throw new WireError("not an MVW3 body");
  }
  if (v.getUint8(5) !== 1) {
    throw new WireError(`MVW3 version ${v.getUint8(5)}`);
  }
  const kind = v.getUint8(4);
  if (kind < 1 || kind > 3) {
    throw new WireError(`window kind ${kind}`);
  }
  const flags = v.getUint16(6, true);
  const start = Number(v.getBigInt64(8, true));
  const len = v.getUint32(16, true);
  const nMovers = v.getUint32(20, true);
  const nRows = v.getUint32(24, true);
  const metaLen = v.getUint32(28, true);
  let off = 32;
  if (metaLen > v.byteLength - off) {
    throw new WireError("mover table length");
  }
  const bytes = new Uint8Array(v.buffer, v.byteOffset, v.byteLength);
  const movers = JSON.parse(new TextDecoder().decode(bytes.subarray(off, off + metaLen))) as MoverMeta[];
  if (!Array.isArray(movers) || movers.length !== nMovers) {
    throw new WireError("mover table size");
  }
  off += metaLen;
  const cols: Column[] = [];
  for (let i = 0; i < 8; i++) {
    if (v.byteLength - off < 4) {
      throw new WireError(`missing column ${i}`);
    }
    const n = v.getUint32(off, true);
    off += 4;
    if (n > v.byteLength - off) {
      throw new WireError(`column ${i} length`);
    }
    cols.push(new Column(bytes.subarray(off, off + n)));
    off += n;
  }
  if (off !== v.byteLength) {
    throw new WireError("trailing bytes");
  }
  const [cCount, cDT, cTick, cX, cY, cO, cFlags, cCar] = cols;
  const hasTicks = (flags & MOVER_HAS_TICKS) !== 0;
  const rows: MoverRow[] = [];
  for (let m = 0; m < nMovers; m++) {
    const n = cCount.uvarint();
    if (n > nRows - rows.length) {
      throw new WireError("row counts exceed the row total");
    }
    let t = start;
    let tick = 0;
    let x = 0;
    let y = 0;
    let o = 0;
    for (let k = 0; k < n; k++) {
      t += cDT.varint();
      const dTick = hasTicks ? cTick.varint() : 0;
      const dx = cX.varint();
      const dy = cY.varint();
      const dO = cO.varint();
      const f = cFlags.byte();
      const rowFlags = f & ~WIRE_HAS_CARRIAGES;
      let rowTick = 0;
      if ((rowFlags & FLAG_NO_TICK) === 0) {
        tick += dTick;
        rowTick = tick;
      }
      let rx = 0;
      let ry = 0;
      let ro = 0;
      if ((rowFlags & FLAG_GONE) === 0) {
        x += dx;
        y += dy;
        o = (o + dO + ORIENT_UNITS) % ORIENT_UNITS;
        rx = x / POS_UNITS;
        ry = y / POS_UNITS;
        ro = o / ORIENT_UNITS;
      }
      let carriages: Carriage[] | null = null;
      if ((f & WIRE_HAS_CARRIAGES) !== 0) {
        const nc = cCar.uvarint();
        if (nc > 10000) {
          throw new WireError(`${nc} carriages`);
        }
        carriages = [];
        for (let i = 0; i < nc; i++) {
          const cx = x + cCar.varint();
          const cy = y + cCar.varint();
          const co = cCar.uvarint();
          carriages.push({ x: cx / POS_UNITS, y: cy / POS_UNITS, o: co / ORIENT_UNITS });
        }
      }
      rows.push({ mover: m, t, tick: rowTick, x: rx, y: ry, o: ro, flags: rowFlags, carriages });
    }
  }
  if (!cols.every(c => c.done())) {
    throw new WireError("unread column bytes");
  }
  if (rows.length !== nRows) {
    throw new WireError(`decoded ${rows.length} rows, header says ${nRows}`);
  }
  return { kind: kind as WindowKind, start, len, movers, rows };
}
