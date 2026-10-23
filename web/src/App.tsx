import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import logoDarkUrl from "../images/logo_dark_small.png";
import logoLightUrl from "../images/logo_light_small.png";
import type { Catalog, TimeMs } from "./api/types";
import { layers as layerFactories } from "./layers/index";
import type { LayerHit } from "./map/layer";
import { createLoader, DirectLoader } from "./map/loader";
import { urlDatastore, urlScale, type BootResult } from "./map/boot";
import { parseDatastoreId } from "./api/grid";
import { createControl } from "./map/control";
import { perfEvent } from "./perf";
import { defaultSelection, MapView, type MapViewState, type RendererChoice, type Selection } from "./map/view";

export type Boot = BootResult;

const SPEEDS: [number, string][] = [
  [60_000, "1 min/s"],
  [600_000, "10 min/s"],
  [3_600_000, "1 h/s"],
  [6 * 3_600_000, "6 h/s"],
  [86_400_000, "1 day/s"],
];

interface UrlState {
  sel: Selection | null;
  camera: { x: number; y: number; scale: number } | null;
  t: number | null;
  layers: string[] | null;
}

/**
 * Page URL state. Datastore: `ds` (a catalog datastore id, as the WP11 suite passes it) or
 * `map` + `surface` + `force`; camera: `x`, `y` and `s` (CSS px per tile) or `z` (a level,
 * 2^z px per tile); `t` (absent = live); `layers`. See boot.ts urlDatastore/urlScale.
 */
function readUrl(): UrlState {
  const p = new URLSearchParams(location.search);
  const num = (k: string): number | null => {
    const v = p.get(k);
    if (v === null || v.trim() === "") {
      return null;
    }
    const n = Number(v);
    return Number.isFinite(n) ? n : null;
  };
  const ds = urlDatastore(p);
  const x = num("x");
  const y = num("y");
  const s = urlScale(p);
  return {
    sel: ds ? selectionOf(ds) : null,
    camera: x !== null && y !== null && s !== null ? { x, y, scale: s } : null,
    t: num("t"),
    layers: p.has("layers") ? (p.get("layers") ?? "").split(",").filter(Boolean) : null,
  };
}

function selectionOf(ds: string): Selection | null {
  try {
    return parseDatastoreId(ds);
  } catch {
    return null;
  }
}

function writeUrl(s: MapViewState, layers: string[]): void {
  const p = new URLSearchParams(location.search);
  // Written back in one form: map/surface/force and s (ds and z are only read).
  p.delete("ds");
  p.delete("z");
  if (s.selection) {
    p.set("map", s.selection.map);
    p.set("surface", s.selection.surface);
    p.set("force", s.selection.force);
  }
  p.set("x", s.camera.x.toFixed(1));
  p.set("y", s.camera.y.toFixed(1));
  p.set("s", Number(s.camera.scale.toPrecision(4)).toString());
  if (s.live) {
    p.delete("t");
  } else {
    p.set("t", String(Math.round(s.time)));
  }
  p.set("layers", layers.join(","));
  const next = `${location.pathname}?${p.toString()}${location.hash}`;
  if (next !== `${location.pathname}${location.search}${location.hash}`) {
    history.replaceState(null, "", next);
  }
}

function formatTime(t: TimeMs): string {
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function formatAgo(ms: number): string {
  if (ms < 60_000) {
    return `${Math.max(0, Math.round(ms / 1000))} s ago`;
  }
  const min = Math.round(ms / 60_000);
  if (min < 120) {
    return `${min} min ago`;
  }
  const h = Math.round(min / 60);
  return h < 72 ? `${h} h ago` : `${Math.round(h / 24)} days ago`;
}

function formatBytes(n: number): string {
  return n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MiB` : `${Math.round(n / 1024)} KiB`;
}

export function App({ boot }: { boot: Promise<Boot> }) {
  const [ready, setReady] = useState<Boot | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [state, setState] = useState<MapViewState | null>(null);
  const [hover, setHover] = useState<{ hit: LayerHit | null; world: [number, number] } | null>(null);
  const [showLogo, setShowLogo] = useState(false);
  const initialUrl = useMemo(readUrl, []);
  const [enabledLayers, setEnabledLayers] = useState<string[]>(() => initialUrl.layers ?? layerFactories.filter(l => l.defaultOn).map(l => l.id));
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<MapView | null>(null);
  const urlTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const layersRef = useRef(enabledLayers);
  layersRef.current = enabledLayers;

  useEffect(() => {
    let cancelled = false;
    boot.then(
      b => {
        if (!cancelled) {
          setReady(b);
          setCatalog(b.catalog);
        }
      },
      err => {
        if (!cancelled) {
          setError(`Could not load the map catalog: ${err instanceof Error ? err.message : String(err)}`);
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, [boot]);

  useEffect(() => {
    const t = setTimeout(() => setShowLogo(true), 4000);
    return () => clearTimeout(t);
  }, []);

  useEffect(() => {
    if (!ready || !host.current) {
      return;
    }
    const params = new URLSearchParams(location.search);
    let v: MapView;
    try {
      v = new MapView({
        container: host.current,
        source: ready.source,
        catalog: ready.catalog,
        renderer: (params.get("renderer") as RendererChoice | null) ?? "auto",
        loader: params.get("loader") === "direct" ? new DirectLoader() : params.get("loader") === "worker" ? createLoader() : undefined,
        layers: layerFactories,
        enabledLayers: layersRef.current,
        camera: initialUrl.camera ?? undefined,
        onState: s => {
          setState(s);
          if (s.tiles.visible > 0 && s.tiles.full === s.tiles.visible) {
            setShowLogo(true);
          }
          if (!urlTimer.current) {
            urlTimer.current = setTimeout(() => {
              urlTimer.current = null;
              writeUrl(v.state(), layersRef.current);
            }, 400);
          }
        },
        onHover: (hit, world) => setHover({ hit, world }),
        onCatalog: c => setCatalog(c),
        viewSeed: ready.seed,
      });
    } catch (err) {
      console.error(err);
      setError(`Could not start the map renderer: ${err instanceof Error ? err.message : String(err)}`);
      return;
    }
    view.current = v;
    const fromUrl = initialUrl.sel !== null && v.select(initialUrl.sel);
    if (!fromUrl) {
      const sel = defaultSelection(ready.catalog);
      if (sel) {
        v.select(sel, { fit: !initialUrl.camera });
      }
    } else if (!initialUrl.camera) {
      v.fit();
    }
    if (initialUrl.t !== null) {
      v.setTime(initialUrl.t);
    }
    // Hooks for tests and benchmarks (WP11): drive the view from the console or Playwright.
    (window as unknown as { __tsdbMap?: unknown }).__tsdbMap = {
      view: v,
      setCamera: (x: number, y: number, scale: number) => v.setCamera({ x, y, scale }),
      setTime: (t: number) => v.setTime(t),
      goLive: () => v.goLive(),
      play: (speed?: number) => v.play(speed),
      pause: () => v.pause(),
      fit: () => v.fit(),
      state: () => v.state(),
    };
    // The TsdbControl contract (web/src/api/types.ts) the WP11 benchmark drives.
    const hostEl = host.current;
    window.__tsdbControl = createControl(v, {
      size: () => ({ width: hostEl.clientWidth, height: hostEl.clientHeight }),
      onSeek: t => perfEvent("time.change", t === undefined ? { live: true } : { t }),
    });
    return () => {
      v.destroy();
      view.current = null;
      delete window.__tsdbControl;
      if (urlTimer.current) {
        clearTimeout(urlTimer.current);
        urlTimer.current = null;
      }
    };
  }, [ready, initialUrl]);

  const select = useCallback(
    (next: Partial<Selection>) => {
      const v = view.current;
      const c = catalog;
      if (!v || !c) {
        return;
      }
      const cur = v.selection ?? defaultSelection(c);
      const mapKey = next.map ?? cur?.map;
      const map = c.maps.find(m => m.key === mapKey) ?? c.maps[0];
      if (!map) {
        return;
      }
      const surfaceName = next.map && next.map !== cur?.map ? undefined : (next.surface ?? cur?.surface);
      const surface = map.surfaces.find(s => s.name === surfaceName) ?? map.surfaces.find(s => s.datastores.length > 0) ?? map.surfaces[0];
      if (!surface) {
        return;
      }
      const forceName = next.force ?? cur?.force;
      const ds = surface.datastores.find(d => d.force === forceName) ?? surface.datastores[0];
      const changedSurface = !cur || cur.map !== map.key || cur.surface !== surface.name;
      v.select({ map: map.key, surface: surface.name, force: ds?.force ?? "player" }, { fit: changedSurface });
    },
    [catalog],
  );

  const toggleLayer = useCallback((id: string) => {
    setEnabledLayers(prev => {
      const next = prev.includes(id) ? prev.filter(l => l !== id) : [...prev, id];
      view.current?.setLayers(next);
      return next;
    });
  }, []);

  const sel = state?.selection ?? null;
  const map = catalog?.maps.find(m => m.key === sel?.map) ?? null;
  const surface = map?.surfaces.find(s => s.name === sel?.surface) ?? null;
  const range = state?.range ?? [0, 0];
  const v = view.current;
  const now = range[1];

  return (
    <main className="app">
      <header className="topbar">
        {/* The logo (3x its drawn size, about 24 KB): loaded after the first full paint, so it never competes with tiles. */}
        <picture className="brand">
          {showLogo ? <source srcSet={logoDarkUrl} media="(prefers-color-scheme: dark)" /> : null}
          {showLogo ? <img className="brandLogo" src={logoLightUrl} alt="TimeShadeDB" width={92} height={28} decoding="async" /> : <span className="brandText">TimeShadeDB</span>}
        </picture>
        {catalog && catalog.maps.length > 0 ? (
          <nav className="pickers" aria-label="Map selection">
            <label className="picker">
              <span>Map</span>
              <select value={sel?.map ?? ""} onChange={e => select({ map: e.target.value })}>
                {catalog.maps.map(m => (
                  <option key={m.key} value={m.key}>
                    {m.title || m.key}
                  </option>
                ))}
              </select>
            </label>
            <label className="picker">
              <span>Surface</span>
              <select value={sel?.surface ?? ""} onChange={e => select({ surface: e.target.value })}>
                {(map?.surfaces ?? []).map(s => (
                  <option key={s.name} value={s.name}>
                    {s.name}
                  </option>
                ))}
              </select>
            </label>
            {surface && surface.datastores.length > 1 ? (
              <label className="picker">
                <span>Force</span>
                <select value={sel?.force ?? ""} onChange={e => select({ force: e.target.value })}>
                  {surface.datastores.map(d => (
                    <option key={d.force} value={d.force}>
                      {d.force}
                    </option>
                  ))}
                </select>
              </label>
            ) : null}
          </nav>
        ) : null}
        {layerFactories.length > 0 ? (
          <fieldset className="layerToggles">
            <legend>Layers</legend>
            {layerFactories.map(l => (
              <label key={l.id} className="layerToggle">
                <input type="checkbox" checked={enabledLayers.includes(l.id)} onChange={() => toggleLayer(l.id)} />
                {l.label}
              </label>
            ))}
          </fieldset>
        ) : null}
        <button className="textButton" type="button" onClick={() => v?.fit()} title="Zoom to fit the surface" disabled={!state?.datastore}>
          Fit
        </button>
      </header>

      <section className="mapHost" ref={host} aria-label="Map">
        {error ? (
          <div className="notice error" role="alert">
            {error}{" "}
            <button className="textButton inline" type="button" onClick={() => location.reload()}>
              Retry
            </button>
          </div>
        ) : null}
        {!error && !ready ? <div className="notice">Loading the map catalog…</div> : null}
        {ready && catalog && catalog.maps.length === 0 ? <div className="notice">No map data has been recorded yet.</div> : null}
        {ready && state && !state.datastore && catalog && catalog.maps.length > 0 ? <div className="notice">This surface has no terrain for the selected force.</div> : null}
        {state ? (
          <div className="hud" aria-live="off">
            <span>z {state.z}</span>
            {hover ? (
              <span>
                {Math.floor(hover.world[0])}, {Math.floor(hover.world[1])}
              </span>
            ) : null}
            <span>
              tiles {state.tiles.full}/{state.tiles.visible}
              {state.tiles.inFlight > 0 ? ` · loading ${state.tiles.inFlight + state.tiles.queued}` : ""}
            </span>
            <span title="Texture memory">{formatBytes(state.tiles.textureBytes)}</span>
            <span>{state.renderer === "webgl2" ? "WebGL2" : state.renderer === "canvas2d" ? "Canvas2D" : state.renderer}</span>
            {hover?.hit ? <span className="hudHit">{hover.hit.label}</span> : null}
          </div>
        ) : null}
      </section>

      <footer className="timebar" aria-label="Time">
        {/* Live counts as playing: pausing it holds the moment shown, and playing on from a past time is a timelapse
            that turns live again when it catches up. */}
        <button
          className="playButton"
          type="button"
          onClick={() => (state?.live ? v?.setTime(v.displayTime()) : state?.playing ? v?.pause() : v?.play())}
          disabled={!state?.datastore}
          aria-label={state?.live ? "Pause" : state?.playing ? "Pause timelapse" : "Play timelapse"}
        >
          {state?.live || state?.playing ? "❚❚" : "▶"}
        </button>
        <select className="speed" value={state?.speed ?? 3_600_000} onChange={e => v?.setSpeed(Number(e.target.value))} aria-label="Timelapse speed">
          {SPEEDS.map(([ms, label]) => (
            <option key={ms} value={ms}>
              {label}
            </option>
          ))}
        </select>
        <input
          className="timeSlider"
          type="range"
          min={range[0]}
          max={Math.max(range[0] + 1, range[1])}
          step={1000}
          value={state ? Math.min(range[1], Math.max(range[0], state.time)) : 0}
          onChange={e => v?.setTime(Number(e.target.value))}
          disabled={!state?.datastore}
          aria-label="Map time"
        />
        <div className="timeReadout">
          <span className="timeValue">{state ? formatTime(state.time) : "–"}</span>
          <span className="timeAgo">{state ? (state.live ? "live" : formatAgo(now - state.time)) : ""}</span>
        </div>
        <button className={`liveButton${state?.live ? " active" : ""}`} type="button" onClick={() => v?.goLive()} disabled={!state?.datastore}>
          Live
        </button>
      </footer>
    </main>
  );
}
