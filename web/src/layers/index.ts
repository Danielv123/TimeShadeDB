// The layers the viewer can draw above terrain, in menu order. Owned by WP8 after WP0:
// add movers (trains, vehicles, players), tags, recipes, UE edges, instance bounds and
// rails here. Terrain is drawn by the map engine (WP7) and is not a layer.

import type { LayerFactory, MapLayer } from "../map/layer";

export const layers: LayerFactory[] = [];

/** Creates the enabled layers sorted by draw order. `enabled` defaults to each layer's defaultOn. */
export function createLayers(enabled?: ReadonlySet<string>): MapLayer[] {
  return layers
    .filter(l => (enabled ? enabled.has(l.id) : l.defaultOn))
    .map(l => l.create())
    .sort((a, b) => a.order - b.order);
}
