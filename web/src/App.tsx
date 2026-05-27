import { type CSSProperties, useCallback, useEffect, useMemo, useRef, useState } from "react";
import L from "leaflet";
import logoDarkUrl from "../images/logo_dark_transparent.png";
import logoLightUrl from "../images/logo_light_transparent.png";

type SaveCatalog = {
  saves: SaveSummary[];
};

type SaveSummary = {
  savefile_uuid: string;
  forces: string[];
  surfaces: string[];
  datastores: DatastoreSummary[];
};

type DatastoreSummary = {
  surface: string;
  force: string;
  latest_tick: number;
  latest_row_seq: number;
  chunk_count: number;
  min_chunk_x: number;
  max_chunk_x: number;
  min_chunk_y: number;
  max_chunk_y: number;
  min_tile_x: number;
  max_tile_x: number;
  min_tile_y: number;
  max_tile_y: number;
};

type SliderStyle = CSSProperties & {
  "--progress": string;
};

type MapUrlState = {
  savefileUUID: string | null;
  force: string | null;
  surface: string | null;
  x: number | null;
  y: number | null;
  z: number | null;
  tick: number | null;
};

const tileRequestThrottleMs = 100;
const metadataRefreshMs = 10_000;
const tileSize = 512;
const maxDisplayZoom = 4;
const playbackSpeedOptions = [60, 300, 900, 1800, 3600, 7200, 18000, 36000];

const DecodedTileLayer = L.TileLayer.extend({
  createTile(coords: L.Coords, done: L.DoneCallback) {
    const tile = document.createElement("img");
    L.DomEvent.on(tile, "load", () => {
      if (typeof tile.decode !== "function") {
        done(undefined, tile);
        return;
      }
      tile.decode().then(
        () => done(undefined, tile),
        (error: unknown) => done(error instanceof Error ? error : new Error(String(error)), tile)
      );
    });
    L.DomEvent.on(tile, "error", () => {
      done(new Error(`Failed to load tile ${tile.src}`), tile);
    });
    tile.alt = "";
    tile.decoding = "async";
    tile.role = "presentation";
    tile.src = this.getTileUrl(coords);
    return tile;
  }
}) as unknown as typeof L.TileLayer;

function decodedTileLayer(url: string, options: L.TileLayerOptions) {
  return new DecodedTileLayer(url, options) as L.TileLayer;
}

function selectedSaveFromPath() {
  const match = /^\/saves\/([^/]+)\/?$/.exec(window.location.pathname);
  return match ? decodeURIComponent(match[1]) : null;
}

function stringParam(params: URLSearchParams, key: string) {
  const value = params.get(key);
  return value && value.trim() ? value : null;
}

function numberParam(params: URLSearchParams, key: string) {
  const value = params.get(key);
  if (!value) {
    return null;
  }
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : null;
}

function mapStateFromUrl(): MapUrlState {
  const params = new URLSearchParams(window.location.search);
  return {
    savefileUUID: selectedSaveFromPath(),
    force: stringParam(params, "force"),
    surface: stringParam(params, "surface"),
    x: numberParam(params, "x"),
    y: numberParam(params, "y"),
    z: numberParam(params, "z"),
    tick: numberParam(params, "tick")
  };
}

function savePath(savefileUUID: string) {
  return `/saves/${encodeURIComponent(savefileUUID)}`;
}

function formattedUrlNumber(value: number, fractionDigits: number) {
  return Number(value.toFixed(fractionDigits)).toString();
}

function urlForMapState(
  savefileUUID: string | null,
  force: string,
  surface: string,
  tick: number | null,
  map: L.Map | null
) {
  if (!savefileUUID) {
    return "/";
  }
  const params = new URLSearchParams();
  if (force) {
    params.set("force", force);
  }
  if (surface) {
    params.set("surface", surface);
  }
  if (tick !== null && Number.isFinite(tick)) {
    params.set("tick", Math.max(0, Math.trunc(tick)).toString());
  }
  if (map) {
    const center = map.getCenter();
    params.set("x", formattedUrlNumber(center.lng, 2));
    params.set("y", formattedUrlNumber(center.lat, 2));
    params.set("z", formattedUrlNumber(map.getZoom(), 2));
  }
  const query = params.toString();
  return `${savePath(savefileUUID)}${query ? `?${query}` : ""}`;
}

function tileLayerUrl(savefileUUID: string, force: string, surface: string, tick: number) {
  return `/api/chunk/tiles/${encodeURIComponent(savefileUUID)}/${encodeURIComponent(force)}/${encodeURIComponent(surface)}/{z}/{x}/{y}.png?tick=${tick}`;
}

function formatTick(tick: number) {
  if (!Number.isFinite(tick) || tick <= 0) {
    return "Tick 0";
  }
  return `Tick ${Math.trunc(tick).toLocaleString()}`;
}

function uniqueSorted(values: string[]) {
  return Array.from(new Set(values)).sort((a, b) => a.localeCompare(b));
}

function preferredValue(values: string[], preferred: string) {
  if (values.includes(preferred)) {
    return preferred;
  }
  return values[0] ?? "";
}

function datastoreBounds(datastore: DatastoreSummary) {
  if (datastore.chunk_count <= 0) {
    return L.latLngBounds([[-tileSize, 0], [0, tileSize]]);
  }
  const west = datastore.min_tile_x * tileSize;
  const east = (datastore.max_tile_x + 1) * tileSize;
  const north = -datastore.min_tile_y * tileSize;
  const south = -(datastore.max_tile_y + 1) * tileSize;
  return L.latLngBounds([[south, west], [north, east]]);
}

function minZoomForDatastore(datastore: DatastoreSummary | null) {
  if (!datastore || datastore.chunk_count <= 0) {
    return 0;
  }
  const cols = Math.max(1, datastore.max_tile_x - datastore.min_tile_x + 1);
  const rows = Math.max(1, datastore.max_tile_y - datastore.min_tile_y + 1);
  const maxTiles = Math.max(cols, rows);
  let zoom = 0;
  for (let tiles = 1; tiles < maxTiles; tiles <<= 1) {
    zoom--;
  }
  return zoom;
}

function restoreMapViewFromUrl(map: L.Map, urlState: MapUrlState, minZoom: number) {
  if (urlState.x === null || urlState.y === null || urlState.z === null) {
    return false;
  }
  const zoom = Math.max(minZoom, Math.min(maxDisplayZoom, urlState.z));
  map.setView([urlState.y, urlState.x], zoom, { animate: false });
  return true;
}

function tickForDatastore(datastore: DatastoreSummary, urlState: MapUrlState) {
  const latestTick = datastore.latest_tick || 0;
  if (urlState.tick === null) {
    return latestTick;
  }
  return Math.min(latestTick, Math.max(0, Math.trunc(urlState.tick)));
}

export function App() {
  const mapNode = useRef<HTMLDivElement | null>(null);
  const mapRef = useRef<L.Map | null>(null);
  const activeLayerRef = useRef<L.TileLayer | null>(null);
  const pendingLayerRef = useRef<L.TileLayer | null>(null);
  const borderRef = useRef<L.Rectangle | null>(null);
  const renderSeqRef = useRef(0);
  const requestTimerRef = useRef<number | null>(null);
  const lastRequestAtRef = useRef(0);
  const pendingRequestTickRef = useRef(0);
  const tickRef = useRef(0);
  const selectedDatastoreKeyRef = useRef("");
  const urlStateRef = useRef<MapUrlState>(mapStateFromUrl());
  const [catalog, setCatalog] = useState<SaveCatalog | null>(null);
  const [selectedSaveID, setSelectedSaveID] = useState<string | null>(() => urlStateRef.current.savefileUUID);
  const [selectedForce, setSelectedForce] = useState(() => urlStateRef.current.force ?? "");
  const [selectedSurface, setSelectedSurface] = useState(() => urlStateRef.current.surface ?? "");
  const [urlViewRevision, setUrlViewRevision] = useState(0);
  const [tick, setTick] = useState(0);
  const [requestTick, setRequestTick] = useState(0);
  const [isPlaying, setIsPlaying] = useState(false);
  const [playbackSpeed, setPlaybackSpeed] = useState(3600);
  const [error, setError] = useState<string | null>(null);

  const saves = catalog?.saves ?? [];
  const selectedSave = useMemo(
    () => saves.find((save) => save.savefile_uuid === selectedSaveID) ?? null,
    [saves, selectedSaveID]
  );
  const forces = selectedSave?.forces ?? [];
  const surfacesForForce = useMemo(() => {
    if (!selectedSave || !selectedForce) {
      return [];
    }
    return uniqueSorted(
      selectedSave.datastores
        .filter((datastore) => datastore.force === selectedForce)
        .map((datastore) => datastore.surface)
    );
  }, [selectedSave, selectedForce]);
  const selectedDatastore = useMemo(() => {
    if (!selectedSave) {
      return null;
    }
    return (
      selectedSave.datastores.find(
        (datastore) => datastore.force === selectedForce && datastore.surface === selectedSurface
      ) ?? null
    );
  }, [selectedForce, selectedSave, selectedSurface]);
  const selectedDatastoreKey = selectedDatastore
    ? `${selectedSave?.savefile_uuid ?? ""}\0${selectedDatastore.force}\0${selectedDatastore.surface}`
    : "";
  const selectedDatastoreBoundsKey = selectedDatastore
    ? [
        selectedDatastore.min_tile_x,
        selectedDatastore.max_tile_x,
        selectedDatastore.min_tile_y,
        selectedDatastore.max_tile_y
      ].join("\0")
    : "";
  const minZoom = useMemo(() => minZoomForDatastore(selectedDatastore), [selectedDatastore]);

  const navigateToSave = useCallback((savefileUUID: string | null) => {
    const nextPath = savefileUUID ? savePath(savefileUUID) : "/";
    window.history.pushState({}, "", nextPath);
    urlStateRef.current = mapStateFromUrl();
    setSelectedSaveID(savefileUUID);
    setError(null);
  }, []);

  const replaceMapUrl = useCallback((savefileUUID: string | null, force: string, surface: string, tick: number | null) => {
    const nextUrl = urlForMapState(savefileUUID, force, surface, tick, mapRef.current);
    if (`${window.location.pathname}${window.location.search}` !== nextUrl) {
      window.history.replaceState({}, "", nextUrl);
    }
    urlStateRef.current = mapStateFromUrl();
  }, []);

  const scheduleTileRequest = useCallback((nextTick: number) => {
    pendingRequestTickRef.current = nextTick;
    const now = performance.now();
    const elapsed = now - lastRequestAtRef.current;
    if (elapsed >= tileRequestThrottleMs) {
      if (requestTimerRef.current !== null) {
        window.clearTimeout(requestTimerRef.current);
        requestTimerRef.current = null;
      }
      lastRequestAtRef.current = now;
      setRequestTick(nextTick);
      return;
    }
    if (requestTimerRef.current !== null) {
      return;
    }
    requestTimerRef.current = window.setTimeout(() => {
      requestTimerRef.current = null;
      lastRequestAtRef.current = performance.now();
      setRequestTick(pendingRequestTickRef.current);
    }, tileRequestThrottleMs - elapsed);
  }, []);

  useEffect(() => {
    const onPopState = () => {
      const nextUrlState = mapStateFromUrl();
      urlStateRef.current = nextUrlState;
      setSelectedSaveID(nextUrlState.savefileUUID);
      setSelectedForce(nextUrlState.force ?? "");
      setSelectedSurface(nextUrlState.surface ?? "");
      setUrlViewRevision((revision) => revision + 1);
      setError(null);
    };
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  useEffect(() => {
    let cancelled = false;
    let inFlight: AbortController | null = null;
    const loadCatalog = () => {
      if (inFlight) {
        return;
      }
      inFlight = new AbortController();
      fetch("/api/chunk/saves", { signal: inFlight.signal })
        .then((response) => {
          if (!response.ok) {
            throw new Error(`Save list request failed: ${response.status}`);
          }
          return response.json() as Promise<SaveCatalog>;
        })
        .then((nextCatalog) => {
          if (!cancelled) {
            setCatalog(nextCatalog);
          }
        })
        .catch((err: unknown) => {
          if (!cancelled && !(err instanceof DOMException && err.name === "AbortError")) {
            setError(err instanceof Error ? err.message : String(err));
          }
        })
        .finally(() => {
          inFlight = null;
        });
    };
    loadCatalog();
    const refreshTimer = window.setInterval(loadCatalog, metadataRefreshMs);
    return () => {
      cancelled = true;
      window.clearInterval(refreshTimer);
      inFlight?.abort();
    };
  }, []);

  useEffect(() => {
    if (!catalog || !selectedSaveID) {
      return;
    }
    if (!catalog.saves.some((save) => save.savefile_uuid === selectedSaveID)) {
      setError(`Save not found: ${selectedSaveID}`);
    }
  }, [catalog, selectedSaveID]);

  useEffect(() => {
    if (!selectedSave) {
      setSelectedForce("");
      setSelectedSurface("");
      setIsPlaying(false);
      return;
    }
    setSelectedForce((current) => {
      if (current && selectedSave.forces.includes(current)) {
        return current;
      }
      return preferredValue(selectedSave.forces, "player");
    });
  }, [selectedForce, selectedSave]);

  useEffect(() => {
    if (!selectedSave || !selectedForce) {
      setSelectedSurface("");
      return;
    }
    setSelectedSurface((current) => {
      if (current && surfacesForForce.includes(current)) {
        return current;
      }
      return preferredValue(surfacesForForce, "nauvis");
    });
  }, [selectedForce, selectedSave, selectedSurface, surfacesForForce]);

  useEffect(() => {
    if (!selectedDatastore) {
      setTick(0);
      setRequestTick(0);
      tickRef.current = 0;
      selectedDatastoreKeyRef.current = "";
      return;
    }
    const latestTick = selectedDatastore.latest_tick || 0;
    const nextTick = tickForDatastore(selectedDatastore, urlStateRef.current);
    if (selectedDatastoreKeyRef.current !== selectedDatastoreKey) {
      selectedDatastoreKeyRef.current = selectedDatastoreKey;
      tickRef.current = nextTick;
      setTick(nextTick);
      setRequestTick(nextTick);
      pendingRequestTickRef.current = nextTick;
      lastRequestAtRef.current = performance.now();
      return;
    }
    if (urlStateRef.current.tick !== null && tickRef.current !== nextTick) {
      tickRef.current = nextTick;
      setTick(nextTick);
      setRequestTick(nextTick);
      pendingRequestTickRef.current = nextTick;
      lastRequestAtRef.current = performance.now();
      return;
    }
    if (tickRef.current > latestTick) {
      tickRef.current = latestTick;
      setTick(latestTick);
      setRequestTick(latestTick);
      pendingRequestTickRef.current = latestTick;
      lastRequestAtRef.current = performance.now();
    }
  }, [selectedDatastore, selectedDatastoreKey, urlViewRevision]);

  useEffect(() => {
    tickRef.current = tick;
  }, [tick]);

  useEffect(() => {
    return () => {
      if (requestTimerRef.current !== null) {
        window.clearTimeout(requestTimerRef.current);
      }
    };
  }, []);

  useEffect(() => {
    if (!isPlaying || !selectedDatastore || selectedDatastore.latest_tick <= 0) {
      return;
    }
    const interval = window.setInterval(() => {
      const baseTick = Number.isFinite(tickRef.current) ? tickRef.current : 0;
      const nextTick = Math.min(selectedDatastore.latest_tick, Math.max(0, baseTick + playbackSpeed));
      tickRef.current = nextTick;
      setTick(nextTick);
      scheduleTileRequest(nextTick);
      if (nextTick >= selectedDatastore.latest_tick) {
        setIsPlaying(false);
      }
    }, 1000);
    return () => window.clearInterval(interval);
  }, [isPlaying, playbackSpeed, scheduleTileRequest, selectedDatastore]);

  useEffect(() => {
    if (!selectedDatastore || !mapNode.current) {
      return;
    }
    const bounds = datastoreBounds(selectedDatastore);
    const map = L.map(mapNode.current, {
      crs: L.CRS.Simple,
      minZoom,
      maxZoom: maxDisplayZoom,
      zoomSnap: 0.25,
      zoomControl: true,
      attributionControl: false,
      fadeAnimation: false,
      maxBounds: bounds.pad(0.35),
      maxBoundsViscosity: 0.7
    });
    borderRef.current = L.rectangle(bounds, {
      color: "#e85d42",
      weight: 2,
      opacity: 0.9,
      fill: false,
      interactive: false
    }).addTo(map);
    if (!restoreMapViewFromUrl(map, urlStateRef.current, minZoom)) {
      map.fitBounds(bounds);
    }
    mapRef.current = map;
    return () => {
      map.remove();
      mapRef.current = null;
      activeLayerRef.current = null;
      pendingLayerRef.current = null;
      borderRef.current = null;
    };
  }, [selectedDatastoreKey]);

  useEffect(() => {
    const map = mapRef.current;
    if (!selectedDatastore || !map) {
      return;
    }
    restoreMapViewFromUrl(map, urlStateRef.current, minZoom);
  }, [minZoom, selectedDatastore, selectedDatastoreKey, urlViewRevision]);

  useEffect(() => {
    const map = mapRef.current;
    if (!selectedDatastore || !map) {
      return;
    }
    const bounds = datastoreBounds(selectedDatastore);
    map.setMinZoom(minZoom);
    map.setMaxBounds(bounds.pad(0.35));
    borderRef.current?.setBounds(bounds);
  }, [minZoom, selectedDatastore, selectedDatastoreBoundsKey]);

  useEffect(() => {
    if (!selectedSaveID || !selectedDatastore) {
      return;
    }
    replaceMapUrl(selectedSaveID, selectedForce, selectedSurface, tick);
  }, [replaceMapUrl, selectedDatastore, selectedForce, selectedSaveID, selectedSurface, tick]);

  useEffect(() => {
    const map = mapRef.current;
    if (!selectedSaveID || !map) {
      return;
    }
    const onMoveEnd = () => replaceMapUrl(selectedSaveID, selectedForce, selectedSurface, tickRef.current);
    map.on("moveend zoomend", onMoveEnd);
    return () => {
      map.off("moveend zoomend", onMoveEnd);
    };
  }, [replaceMapUrl, selectedDatastoreKey, selectedForce, selectedSaveID, selectedSurface]);

  useEffect(() => {
    const map = mapRef.current;
    if (!selectedSaveID || !selectedDatastore || !selectedForce || !selectedSurface || !map) {
      return;
    }
    const seq = ++renderSeqRef.current;
    pendingLayerRef.current?.remove();
    const bounds = datastoreBounds(selectedDatastore);
    const nextLayer = decodedTileLayer(
      tileLayerUrl(selectedSaveID, selectedForce, selectedSurface, requestTick),
      {
        tileSize,
        minZoom,
        maxZoom: maxDisplayZoom,
        maxNativeZoom: 0,
        bounds,
        noWrap: true,
        updateWhenIdle: false,
        updateWhenZooming: true,
        keepBuffer: 1,
        opacity: 1,
        className: "canvasTile"
      }
    ).addTo(map);
    pendingLayerRef.current = nextLayer;

    nextLayer.once("load", () => {
      if (renderSeqRef.current !== seq) {
        nextLayer.remove();
        return;
      }
      nextLayer.bringToFront();
      const previousLayer = activeLayerRef.current;
      activeLayerRef.current = nextLayer;
      pendingLayerRef.current = null;
      setError(null);
      if (previousLayer && previousLayer !== nextLayer) {
        requestAnimationFrame(() => {
          requestAnimationFrame(() => previousLayer.remove());
        });
      }
    });
    nextLayer.once("tileerror", (event) => {
      if (renderSeqRef.current !== seq) {
        return;
      }
      nextLayer.remove();
      if (pendingLayerRef.current === nextLayer) {
        pendingLayerRef.current = null;
      }
      const coords = (event as L.TileEvent).coords;
      setError(`Failed to load tile z${coords.z} ${coords.x},${coords.y} for ${formatTick(requestTick)}`);
    });

    return () => {
      if (renderSeqRef.current !== seq) {
        return;
      }
      if (pendingLayerRef.current === nextLayer) {
        pendingLayerRef.current = null;
        nextLayer.remove();
      } else if (activeLayerRef.current !== nextLayer) {
        nextLayer.remove();
      }
    };
  }, [
    minZoom,
    requestTick,
    selectedDatastoreBoundsKey,
    selectedDatastoreKey,
    selectedForce,
    selectedSaveID,
    selectedSurface
  ]);

  const sliderDisabled = !selectedDatastore || selectedDatastore.latest_tick <= 0;
  const progress = useMemo(() => {
    if (!selectedDatastore || selectedDatastore.latest_tick <= 0) {
      return 100;
    }
    return (tick / selectedDatastore.latest_tick) * 100;
  }, [selectedDatastore, tick]);

  const showSaveList = !selectedSave;

  return (
    <main className="shell">
      <aside className="sidebar">
        <div className="brandHeader">
          <button className="brandButton" type="button" onClick={() => navigateToSave(null)}>
            <picture>
              <source media="(prefers-color-scheme: dark)" srcSet={logoDarkUrl} />
              <img className="brandLogo" src={logoLightUrl} alt="TimeShadeDB" />
            </picture>
          </button>
        </div>

        {selectedSave ? (
          <>
            <section className="controlGroup" aria-label="Game tick">
              <div className="timestampReadout">{formatTick(tick)}</div>
              <div className="playbackControls">
                <button
                  className="playbackButton"
                  type="button"
                  disabled={sliderDisabled}
                  aria-pressed={isPlaying}
                  onClick={() => {
                    if (isPlaying) {
                      setIsPlaying(false);
                      return;
                    }
                    if (selectedDatastore && tickRef.current >= selectedDatastore.latest_tick) {
                      tickRef.current = 0;
                      setTick(0);
                      scheduleTileRequest(0);
                    }
                    setIsPlaying(true);
                  }}
                >
                  <span className="playbackIcon" aria-hidden="true">
                    {isPlaying ? "||" : ">"}
                  </span>
                  <span>{isPlaying ? "Pause" : "Play"}</span>
                </button>
                <label className="speedControl">
                  <span>Speed</span>
                  <select
                    value={playbackSpeed}
                    disabled={sliderDisabled}
                    aria-label="Playback speed"
                    onChange={(event) => setPlaybackSpeed(Number(event.currentTarget.value))}
                  >
                    {playbackSpeedOptions.map((speed) => (
                      <option key={speed} value={speed}>
                        {speed.toLocaleString()} ticks/s
                      </option>
                    ))}
                  </select>
                </label>
              </div>
              <input
                className="timeSlider"
                type="range"
                min={0}
                max={selectedDatastore?.latest_tick ?? 0}
                value={tick}
                step={1}
                disabled={sliderDisabled}
                style={{ "--progress": `${progress}%` } as SliderStyle}
                onInput={(event) => {
                  const nextTick = Number(event.currentTarget.value);
                  tickRef.current = nextTick;
                  setTick(nextTick);
                  scheduleTileRequest(nextTick);
                }}
                onChange={(event) => {
                  const nextTick = Number(event.currentTarget.value);
                  tickRef.current = nextTick;
                  setTick(nextTick);
                  scheduleTileRequest(nextTick);
                }}
              />
              <div className="rangeLabels">
                <span>{formatTick(0)}</span>
                <span>{formatTick(selectedDatastore?.latest_tick ?? 0)}</span>
              </div>
            </section>

            <dl className="statsGrid">
              <div>
                <dt>Save</dt>
                <dd>{selectedSave.savefile_uuid}</dd>
              </div>
              <div>
                <dt>Chunks</dt>
                <dd>{selectedDatastore ? selectedDatastore.chunk_count.toLocaleString() : "-"}</dd>
              </div>
              <div>
                <dt>Tiles</dt>
                <dd>
                  {selectedDatastore
                    ? `${selectedDatastore.max_tile_x - selectedDatastore.min_tile_x + 1} x ${
                        selectedDatastore.max_tile_y - selectedDatastore.min_tile_y + 1
                      }`
                    : "-"}
                </dd>
              </div>
              <div>
                <dt>Row seq</dt>
                <dd>{selectedDatastore ? selectedDatastore.latest_row_seq.toLocaleString() : "-"}</dd>
              </div>
            </dl>

            <section className="controlGroup" aria-label="Force">
              <label className="selectControl">
                <span>Force</span>
                <select
                  value={selectedForce}
                  onChange={(event) => {
                    setSelectedForce(event.currentTarget.value);
                    setIsPlaying(false);
                  }}
                >
                  {forces.map((force) => (
                    <option key={force} value={force}>
                      {force}
                    </option>
                  ))}
                </select>
              </label>
            </section>

            <section className="surfacePanel" aria-label="Surfaces">
              <div className="panelLabel">Surfaces</div>
              <div className="surfaceList">
                {surfacesForForce.map((surface) => (
                  <button
                    key={surface}
                    className="surfaceButton"
                    type="button"
                    aria-pressed={surface === selectedSurface}
                    onClick={() => {
                      setSelectedSurface(surface);
                      setIsPlaying(false);
                    }}
                  >
                    {surface}
                  </button>
                ))}
              </div>
            </section>
          </>
        ) : (
          <section className="controlGroup">
            <div className="timestampReadout">Saves</div>
            <dl className="statsGrid">
              <div>
                <dt>Count</dt>
                <dd>{saves.length.toLocaleString()}</dd>
              </div>
              <div>
                <dt>Database</dt>
                <dd>Chunks</dd>
              </div>
            </dl>
          </section>
        )}

        {error ? <div className="errorBox">{error}</div> : null}
      </aside>

      {showSaveList ? (
        <section className="savePane" aria-label="Saves">
          <div className="saveList">
            {saves.map((save) => (
              <button
                className="saveItem"
                key={save.savefile_uuid}
                type="button"
                onClick={() => navigateToSave(save.savefile_uuid)}
              >
                <span className="saveName">{save.savefile_uuid}</span>
                <span className="saveMeta">
                  {save.datastores.length.toLocaleString()} datastores - {save.forces.join(", ") || "no forces"}
                </span>
              </button>
            ))}
            {catalog && saves.length === 0 ? <div className="emptyState">No saves have been ingested.</div> : null}
          </div>
        </section>
      ) : (
        <section className="mapPane" aria-label="Chunk map">
          <div ref={mapNode} className="map" />
        </section>
      )}
    </main>
  );
}
