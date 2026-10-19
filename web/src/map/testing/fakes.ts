// A renderer that records textures and draw commands instead of drawing (tests). It keeps a
// copy of each texture's pixels and applies deltas to it, so tests can check what a texture
// holds after in-place updates.

import type { DecodedDelta } from "../../api/types";
import type { Viewport } from "../layer";
import { textureBytes, type DrawCommand, type TexturePixels, type TileRenderer, type TileTexture } from "../renderer.ts";

export interface FakeTexture extends TileTexture {
  id: number;
  pixels: Uint8Array | Uint16Array;
  deleted: boolean;
}

export class FakeRenderer implements TileRenderer {
  readonly kind = "null" as const;
  onLost?: () => void;
  palette = new Uint16Array(0);
  textures: FakeTexture[] = [];
  lastDraws: DrawCommand[] = [];
  renders = 0;
  private nextId = 1;

  get paletteLength(): number {
    return this.palette.length;
  }

  setPalette(colors: Uint16Array): void {
    this.palette = colors.slice();
  }

  resetPalette(): void {
    this.palette = new Uint16Array(0);
  }

  createTexture(p: TexturePixels): TileTexture {
    if (p.format !== 3 && this.palette.length < p.paletteVersion) {
      throw new Error(`texture needs palette ${p.paletteVersion}, have ${this.palette.length}`);
    }
    const t: FakeTexture = { id: this.nextId++, format: p.format, bytes: textureBytes(p.format, false), pixels: p.pixels.slice(), deleted: false };
    this.textures.push(t);
    return t;
  }

  applyDelta(tex: TileTexture, d: DecodedDelta): boolean {
    const t = tex as FakeTexture;
    if (t.deleted || (d.header.format === 2 && t.format === 1)) {
      return false;
    }
    for (const e of d.entries) {
      for (let p = 0; p < 1024; p++) {
        const i = ((e.block >> 4) * 32 + (p >> 5)) * 512 + (e.block & 15) * 32 + (p & 31);
        t.pixels[i] = e.pixels[p];
      }
    }
    return true;
  }

  deleteTexture(tex: TileTexture): void {
    (tex as FakeTexture).deleted = true;
  }

  render(_vp: Viewport, draws: readonly DrawCommand[]): void {
    this.renders++;
    this.lastDraws = [...draws];
  }

  live(): FakeTexture[] {
    return this.textures.filter(t => !t.deleted);
  }

  dispose(): void {}
}
