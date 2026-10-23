// The terrain renderer interface and the colour lookup tables shared by the renderers and
// workers. Renderers: WebGL2 palette-index textures (render_gl.ts) and a Canvas2D fallback
// with a 65,536-entry RGBA lookup table (render_2d.ts).

import type { DecodedDelta } from "../api/types";
import type { Viewport } from "./layer";

/** Pixel formats of TSV3/TSD3 (FORMAT.md 7.1). */
export const FORMAT_PAL8 = 1;
export const FORMAT_PAL16 = 2;
export const FORMAT_RGB565 = 3;

export const TILE = 512;

/** Pixels for a new texture: palette indices (pal8/pal16) or RGB565. 512x512 row-major. */
export interface TexturePixels {
  format: number;
  pixels: Uint8Array | Uint16Array;
  /** Smallest palette length that decodes the pixels (0 for rgb565). */
  paletteVersion: number;
  /** Optional RGBA expansion done elsewhere (a worker), used by the Canvas2D renderer. */
  rgba?: Uint8ClampedArray;
}

/** An opaque texture handle. */
export interface TileTexture {
  readonly bytes: number;
  readonly format: number;
}

export interface DrawCommand {
  tex: TileTexture;
  /** Source rectangle in texture pixels. */
  sx: number;
  sy: number;
  sw: number;
  sh: number;
  /** Destination rectangle in CSS pixels. */
  dx: number;
  dy: number;
  dw: number;
  dh: number;
}

export interface TileRenderer {
  readonly kind: "webgl2" | "canvas2d" | "null";
  /** Number of palette entries the renderer holds. */
  readonly paletteLength: number;
  /** Sets the palette (append-only growth of the same datastore palette, or a new one after reset). */
  setPalette(colors: Uint16Array): void;
  /** Forgets the palette (datastore switch). */
  resetPalette(): void;
  /** Creates a texture. Paletted pixels need paletteLength >= paletteVersion. */
  createTexture(p: TexturePixels): TileTexture;
  /** Patches chunks of a texture in place. Returns false when it cannot (format mismatch). */
  applyDelta(tex: TileTexture, delta: DecodedDelta): boolean;
  deleteTexture(tex: TileTexture): void;
  /** Clears the canvas and draws the commands in order. */
  render(viewport: Viewport, draws: readonly DrawCommand[]): void;
  /** Called by the renderer when every texture was lost (WebGL context loss). */
  onLost?: () => void;
  /** Reads back the canvas (RGBA, top row first) right after render(); for tests. */
  readPixels?(): { width: number; height: number; rgba: Uint8Array };
  dispose(): void;
}

/** Uncharted pixels draw opaque black. */
export const UNCHARTED_RGBA = 0xff000000;

let lut565: Uint32Array | null = null;

/** RGB565 -> RGBA8888 (as little-endian Uint32: 0xAABBGGRR) with bit replication; 0 -> black. */
export function rgb565Lut(): Uint32Array {
  if (lut565) {
    return lut565;
  }
  const lut = new Uint32Array(65536);
  for (let c = 0; c < 65536; c++) {
    const r5 = (c >> 11) & 31;
    const g6 = (c >> 5) & 63;
    const b5 = c & 31;
    const r = (r5 << 3) | (r5 >> 2);
    const g = (g6 << 2) | (g6 >> 4);
    const b = (b5 << 3) | (b5 >> 2);
    lut[c] = (0xff << 24) | (b << 16) | (g << 8) | r;
  }
  lut[0] = UNCHARTED_RGBA;
  lut565 = lut;
  return lut;
}

/** Palette index -> RGBA (Uint32, little-endian) for a palette of RGB565 colours. */
export function paletteLut(palette: Uint16Array): Uint32Array {
  const base = rgb565Lut();
  const out = new Uint32Array(palette.length);
  for (let i = 0; i < palette.length; i++) {
    out[i] = base[palette[i]];
  }
  return out;
}

/**
 * Expands pixels to RGBA through a lookup table: `lut` is the palette LUT for paletted
 * formats or rgb565Lut() for rgb565. Indices beyond the LUT draw black.
 */
export function expandRgba(pixels: Uint8Array | Uint16Array, lut: Uint32Array, out?: Uint32Array): Uint32Array {
  const n = pixels.length;
  const dst = out ?? new Uint32Array(n);
  const max = lut.length;
  for (let i = 0; i < n; i++) {
    const v = pixels[i];
    dst[i] = v < max ? lut[v] : UNCHARTED_RGBA;
  }
  return dst;
}

/** The LUT for a format: rgb565 uses the fixed table, paletted formats the palette's. */
export function lutFor(format: number, palLut: Uint32Array | null): Uint32Array {
  if (format === FORMAT_RGB565) {
    return rgb565Lut();
  }
  return palLut ?? new Uint32Array(1).fill(UNCHARTED_RGBA);
}

/** Texture bytes on the GPU for a format (Canvas2D always holds RGBA). */
export function textureBytes(format: number, rgba: boolean): number {
  if (rgba) {
    return TILE * TILE * 4;
  }
  return TILE * TILE * (format === FORMAT_PAL8 ? 1 : 2);
}

/** Top-left texture pixel of chunk block b (0..255) in a z0 tile. */
export function blockOrigin(b: number): [number, number] {
  return [(b & 15) * 32, (b >> 4) * 32];
}

/** Snaps a CSS-pixel rectangle to device pixels so neighbouring tiles share edges exactly. */
export function snapRect(dx: number, dy: number, dw: number, dh: number, dpr: number): [number, number, number, number] {
  const x0 = Math.round(dx * dpr);
  const y0 = Math.round(dy * dpr);
  const x1 = Math.round((dx + dw) * dpr);
  const y1 = Math.round((dy + dh) * dpr);
  return [x0, y0, x1 - x0, y1 - y0];
}
