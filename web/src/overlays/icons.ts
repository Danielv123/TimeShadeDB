// Icon lookup over the Clusterio mod-pack export (catalog.icons): sprites by the names tags
// and recipes use.
//
// Ported from illisis-minimap web/view/overlays.ts (iconIndex, recipeIcon) at 91a6461.
// Copyright (c) illisis-minimap contributors. Licensed under the Blue Oak Model License
// 1.0.0 (https://blueoakcouncil.org/license/1.0.0); keep this notice.

/** A sprite in the export's spritesheet. */
export interface Sprite {
  x: number;
  y: number;
  size: number;
}

/** The export's metadata.json: entries per prototype base name. */
export type IconMetadata = Record<string, { name: string; icon?: Sprite }[]>;

/**
 * Sprites by tag icon name (`"<signal type>/<name>"`; the export calls the `virtual` signal
 * type `virtual-signal`) and `recipe/<name>`. For the engine's IconAtlas.sprites.
 */
export function iconIndex(meta: IconMetadata): Map<string, Sprite> {
  const out = new Map<string, Sprite>();
  for (const [base, entries] of Object.entries(meta)) {
    const type = base === "virtual-signal" ? "virtual" : base;
    for (const e of entries) {
      if (e.icon) {
        out.set(`${type}/${e.name}`, e.icon);
      }
    }
  }
  return out;
}

/**
 * A recipe's sprite key: its own icon, else the same-named item's or fluid's, as the game
 * draws a recipe without one; undefined when there is none.
 */
export function recipeIcon(sprites: Map<string, Sprite> | undefined, recipe: string): string | undefined {
  return ["recipe", "item", "fluid"].map(type => `${type}/${recipe}`).find(key => sprites?.has(key));
}
