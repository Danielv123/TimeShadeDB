// Canvas2D fallback renderer, for when WebGL2 is unavailable. Each tile becomes a
// 512x512 RGBA canvas filled through a 65,536-entry lookup table (renderer.ts), the method
// measured about 23x faster than illisis's per-pixel decode path. Drawn scaled with image
// smoothing off.

import type { DecodedDelta } from "../api/types";
import type { Viewport } from "./layer";
import {
  blockOrigin,
  expandRgba,
  FORMAT_RGB565,
  lutFor,
  paletteLut,
  snapRect,
  TILE,
  textureBytes,
  type DrawCommand,
  type TexturePixels,
  type TileRenderer,
  type TileTexture,
} from "./renderer.ts";

type AnyCanvas = HTMLCanvasElement | OffscreenCanvas;
type AnyCtx = CanvasRenderingContext2D | OffscreenCanvasRenderingContext2D;

interface CanvasTexture extends TileTexture {
  canvas: AnyCanvas | null;
  ctx: AnyCtx | null;
}

function makeCanvas(w: number, h: number): AnyCanvas {
  if (typeof OffscreenCanvas !== "undefined") {
    return new OffscreenCanvas(w, h);
  }
  const c = document.createElement("canvas");
  c.width = w;
  c.height = h;
  return c;
}

export class Canvas2DRenderer implements TileRenderer {
  readonly kind = "canvas2d" as const;
  onLost?: () => void;
  private readonly canvas: HTMLCanvasElement;
  private readonly ctx: CanvasRenderingContext2D;
  private palette = new Uint16Array(0);
  private palLut: Uint32Array | null = null;
  private readonly scratch = new Uint32Array(TILE * TILE);
  private readonly chunkScratch = new Uint32Array(1024);

  constructor(canvas: HTMLCanvasElement) {
    this.canvas = canvas;
    const ctx = canvas.getContext("2d", { alpha: false });
    if (!ctx) {
      throw new Error("Canvas2D is not available");
    }
    this.ctx = ctx;
  }

  get paletteLength(): number {
    return this.palette.length;
  }

  /** The palette lookup table (for workers that expand tiles). */
  get lut(): Uint32Array | null {
    return this.palLut;
  }

  setPalette(colors: Uint16Array): void {
    this.palette = colors.slice();
    this.palLut = paletteLut(this.palette);
  }

  resetPalette(): void {
    this.palette = new Uint16Array(0);
    this.palLut = null;
  }

  createTexture(p: TexturePixels): TileTexture {
    const canvas = makeCanvas(TILE, TILE);
    const ctx = canvas.getContext("2d", { alpha: false }) as AnyCtx | null;
    if (!ctx) {
      throw new Error("Canvas2D is not available");
    }
    let rgba = p.rgba;
    if (!rgba) {
      const out = expandRgba(p.pixels, lutFor(p.format, this.palLut), this.scratch);
      rgba = new Uint8ClampedArray(out.buffer, out.byteOffset, out.byteLength);
    }
    ctx.putImageData(new ImageData(rgba as Uint8ClampedArray<ArrayBuffer>, TILE, TILE), 0, 0);
    return { canvas, ctx, format: p.format, bytes: textureBytes(p.format, true) } as CanvasTexture;
  }

  applyDelta(handle: TileTexture, delta: DecodedDelta): boolean {
    const t = handle as CanvasTexture;
    if (!t.ctx || delta.header.format === FORMAT_RGB565) {
      return false;
    }
    const lut = lutFor(delta.header.format, this.palLut);
    for (const e of delta.entries) {
      const out = expandRgba(e.pixels, lut, this.chunkScratch);
      const [x, y] = blockOrigin(e.block);
      t.ctx.putImageData(new ImageData(new Uint8ClampedArray(out.buffer as ArrayBuffer), 32, 32), x, y);
    }
    return true;
  }

  deleteTexture(handle: TileTexture): void {
    const t = handle as CanvasTexture;
    if (t.canvas) {
      // Release the backing store early.
      t.canvas.width = 0;
      t.canvas.height = 0;
    }
    t.canvas = null;
    t.ctx = null;
  }

  render(vp: Viewport, draws: readonly DrawCommand[]): void {
    const w = Math.max(1, Math.round(vp.width * vp.dpr));
    const h = Math.max(1, Math.round(vp.height * vp.dpr));
    if (this.canvas.width !== w || this.canvas.height !== h) {
      this.canvas.width = w;
      this.canvas.height = h;
    }
    const ctx = this.ctx;
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.imageSmoothingEnabled = false;
    ctx.fillStyle = "#000";
    ctx.fillRect(0, 0, w, h);
    for (const d of draws) {
      const t = d.tex as CanvasTexture;
      if (!t.canvas) {
        continue;
      }
      const [x, y, dw, dh] = snapRect(d.dx, d.dy, d.dw, d.dh, vp.dpr);
      if (dw <= 0 || dh <= 0 || x >= w || y >= h || x + dw <= 0 || y + dh <= 0) {
        continue;
      }
      ctx.drawImage(t.canvas, d.sx, d.sy, d.sw, d.sh, x, y, dw, dh);
    }
  }

  readPixels(): { width: number; height: number; rgba: Uint8Array } {
    const w = this.canvas.width;
    const h = this.canvas.height;
    const d = this.ctx.getImageData(0, 0, w, h).data;
    return { width: w, height: h, rgba: new Uint8Array(d.buffer, d.byteOffset, d.byteLength) };
  }

  dispose(): void {
    // Textures are released by the engine.
  }
}
