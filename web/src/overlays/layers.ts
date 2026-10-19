// Overlay layers drawn above terrain: chart tags, machine recipes, universal-edges lines,
// instance bounds and (debug) rail entities. Tags and recipes follow illisis-minimap
// web/layers.ts (zoom thresholds, icon then text, text outlined so it reads on any
// terrain). Game text reaches the page only through fillText/strokeText.

import type { Interval, IntervalKind, TimeMs } from "../api/types";
import type { FrameState, IconAtlas, LayerContext, MapLayer } from "../map/layer";
import { OverlayFeed } from "./feed";
import { recipeIcon } from "./icons";

/** Tags show from this zoom (CSS px per tile) in; recipes once a 3-tile machine is 8 px. */
export const TAG_SCALE = 0.5;
export const RECIPE_SCALE = 8 / 3;
/** Rails (debug layer) show from this zoom in. */
export const RAIL_SCALE = 2;
/** Instance names show from this zoom in. */
export const NAME_SCALE = 0.1;

function screen(f: FrameState, x: number, y: number): [number, number] {
  return [(x - f.camera.x) * f.camera.scale + f.viewport.width / 2, (y - f.camera.y) * f.camera.scale + f.viewport.height / 2];
}

function label(ctx: CanvasRenderingContext2D, text: string, x: number, y: number): void {
  ctx.font = "12px sans-serif";
  ctx.textBaseline = "middle";
  ctx.lineWidth = 3;
  ctx.strokeStyle = "#000";
  ctx.fillStyle = "#fff";
  ctx.strokeText(text, x, y);
  ctx.fillText(text, x, y);
}

/** Shared base: a feed of one interval kind for the shown datastore, and the icon atlas. */
abstract class IntervalLayer implements MapLayer {
  abstract readonly id: string;
  abstract readonly order: number;
  protected abstract readonly kind: IntervalKind;
  protected abstract readonly minScale: number;
  protected feed: OverlayFeed | null = null;
  protected atlas: IconAtlas | null = null;
  private kinds: Set<IntervalKind> | null = null;

  attach(ctx: LayerContext): void {
    this.detach();
    if (!ctx.datastore) {
      return;
    }
    this.kinds = new Set([this.kind]);
    this.feed = new OverlayFeed(ctx.source, ctx.datastore.id, [this.kind], () => ctx.requestRedraw());
    void ctx.icons().then(a => {
      this.atlas = a;
      ctx.requestRedraw();
    });
  }

  detach(): void {
    this.feed?.stop();
    this.feed = null;
    this.atlas = null;
  }

  setTime(_time: TimeMs, _live: boolean): void {
    // Overlays are queried at frame.time on every draw.
  }

  draw2d(ctx: CanvasRenderingContext2D, frame: FrameState): void {
    if (!this.feed || !this.kinds || frame.camera.scale < this.minScale) {
      return;
    }
    const items = this.feed.query(frame.visible, frame.time, frame.live, this.kinds);
    if (items) {
      this.drawItems(ctx, frame, items);
    }
  }

  protected abstract drawItems(ctx: CanvasRenderingContext2D, frame: FrameState, items: Interval[]): void;

  /** An icon `size` px square at (sx, sy), or its name as text without a sprite; then `text`. */
  protected mark(ctx: CanvasRenderingContext2D, sx: number, sy: number, size: number, icon: string, text: string, frame: FrameState): void {
    if (sx < -size || sy < -size || sx > frame.viewport.width + size || sy > frame.viewport.height + size) {
      return;
    }
    const sprite = icon ? this.atlas?.sprites.get(icon) : undefined;
    if (this.atlas && sprite) {
      ctx.drawImage(this.atlas.image, sprite.x, sprite.y, sprite.size, sprite.size, sx - size / 2, sy - size / 2, size, size);
    }
    const words = sprite || icon === "" ? text : `${icon.slice(icon.indexOf("/") + 1)} ${text}`.trim();
    if (words !== "") {
      label(ctx, words, sx + size / 2 + 2, sy);
    }
  }
}

export class TagsLayer extends IntervalLayer {
  readonly id = "tags";
  readonly order = 30;
  protected readonly kind = "tag" as const;
  protected readonly minScale = TAG_SCALE;

  protected drawItems(ctx: CanvasRenderingContext2D, frame: FrameState, items: Interval[]): void {
    for (const iv of items) {
      const [sx, sy] = screen(frame, iv.x, iv.y);
      this.mark(ctx, sx, sy, 16, iv.icon ?? "", iv.text ?? "", frame);
    }
  }
}

export class RecipesLayer extends IntervalLayer {
  readonly id = "recipes";
  readonly order = 20;
  protected readonly kind = "recipe" as const;
  protected readonly minScale = RECIPE_SCALE;

  protected drawItems(ctx: CanvasRenderingContext2D, frame: FrameState, items: Interval[]): void {
    if (!this.atlas) {
      return; // the icon, not the name, as illisis draws recipes
    }
    const size = Math.min(32, 3 * frame.camera.scale);
    for (const iv of items) {
      const icon = iv.recipe ? recipeIcon(this.atlas.sprites, iv.recipe) : undefined;
      if (icon) {
        const [sx, sy] = screen(frame, iv.x, iv.y);
        this.mark(ctx, sx, sy, size, icon, "", frame);
      }
    }
  }
}

/** Rail entities from the optional rail layer (WP9), as short grey strokes along their direction. Debug aid; off by default. */
export class RailsLayer extends IntervalLayer {
  readonly id = "rails";
  readonly order = 15;
  protected readonly kind = "rail" as const;
  protected readonly minScale = RAIL_SCALE;

  protected drawItems(ctx: CanvasRenderingContext2D, frame: FrameState, items: Interval[]): void {
    const k = frame.camera.scale;
    ctx.strokeStyle = "rgba(200,200,200,0.8)";
    ctx.lineWidth = Math.max(1, 0.4 * k);
    ctx.beginPath();
    for (const iv of items) {
      const [sx, sy] = screen(frame, iv.x, iv.y);
      // Factorio 2.0 rail directions are 16-way (0 north); drawn about one tile either side.
      const a = ((iv.dir ?? 0) / 16) * 2 * Math.PI;
      const dx = Math.sin(a) * k;
      const dy = -Math.cos(a) * k;
      ctx.moveTo(sx - dx, sy - dy);
      ctx.lineTo(sx + dx, sy + dy);
    }
    ctx.stroke();
  }
}

/** Universal-edges lines of the surface (catalog surface.edges), 2 px at every zoom. */
export class EdgesLayer implements MapLayer {
  readonly id = "edges";
  readonly order = 10;
  private lines: [number, number, number, number][] = [];

  attach(ctx: LayerContext): void {
    this.lines = ctx.surface.edges ?? [];
  }

  detach(): void {
    this.lines = [];
  }

  draw2d(ctx: CanvasRenderingContext2D, frame: FrameState): void {
    if (this.lines.length === 0) {
      return;
    }
    ctx.strokeStyle = "#ffd700";
    ctx.lineWidth = 2;
    ctx.beginPath();
    for (const [x1, y1, x2, y2] of this.lines) {
      ctx.moveTo(...screen(frame, x1, y1));
      ctx.lineTo(...screen(frame, x2, y2));
    }
    ctx.stroke();
  }
}

/** Clusterio instance bounds (catalog map.instances), outlined and named. */
export class InstancesLayer implements MapLayer {
  readonly id = "instances";
  readonly order = 5;
  private instances: { name: string; bounds: [number, number, number, number] }[] = [];

  attach(ctx: LayerContext): void {
    this.instances = [];
    for (const i of ctx.map.instances) {
      if (i.bounds) {
        this.instances.push({ name: i.name, bounds: i.bounds });
      }
    }
  }

  detach(): void {
    this.instances = [];
  }

  draw2d(ctx: CanvasRenderingContext2D, frame: FrameState): void {
    ctx.strokeStyle = "rgba(255,255,255,0.35)";
    ctx.lineWidth = 1;
    for (const i of this.instances) {
      const [x1, y1] = screen(frame, i.bounds[0], i.bounds[1]);
      const [x2, y2] = screen(frame, i.bounds[2], i.bounds[3]);
      ctx.strokeRect(x1, y1, x2 - x1, y2 - y1);
      if (frame.camera.scale >= NAME_SCALE) {
        label(ctx, i.name, x1 + 4, y1 + 10);
      }
    }
  }
}
