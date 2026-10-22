// The layers the viewer can draw above terrain, in menu order: movers
// (trains, vehicles, players), tags, recipes, UE edges, instance bounds and rails. Terrain
// is drawn by the map engine (web/src/map/) and is not a layer.

import type { LayerFactory, MapLayer } from "../map/layer";
import { MoverLayer } from "../movers/layer";
import { EdgesLayer, InstancesLayer, RailsLayer, RecipesLayer, TagsLayer } from "../overlays/layers";

export const layers: LayerFactory[] = [
  { id: "movers", label: "Trains, vehicles and players", defaultOn: true, create: () => new MoverLayer() },
  { id: "tags", label: "Map tags", defaultOn: true, create: () => new TagsLayer() },
  { id: "recipes", label: "Recipes", defaultOn: true, create: () => new RecipesLayer() },
  { id: "edges", label: "Universal edges", defaultOn: true, create: () => new EdgesLayer() },
  { id: "instances", label: "Instance bounds", defaultOn: false, create: () => new InstancesLayer() },
  { id: "rails", label: "Rail entities (debug)", defaultOn: false, create: () => new RailsLayer() },
];

/** Creates the enabled layers sorted by draw order. `enabled` defaults to each layer's defaultOn. */
export function createLayers(enabled?: ReadonlySet<string>): MapLayer[] {
  return layers
    .filter(l => (enabled ? enabled.has(l.id) : l.defaultOn))
    .map(l => l.create())
    .sort((a, b) => a.order - b.order);
}
