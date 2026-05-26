import { type CSSProperties, useCallback, useEffect, useMemo, useRef, useState } from "react";
import L from "leaflet";

type Metadata = {
  canvasWidth: number;
  canvasHeight: number;
  tileSize: number;
  tileCols: number;
  tileRows: number;
  minZoom: number;
  fromSec: number;
  toSec: number;
};

type SliderStyle = CSSProperties & {
  "--progress": string;
};

const tileRequestThrottleMs = 100;
const maxDisplayZoom = 4;
const playbackSpeedOptions = [1, 5, 10, 30, 60, 300, 900, 3600];

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

function tileLayerUrl(timestamp: number) {
  return `/api/tiles/{z}/{x}/{y}.png?ts=${timestamp}`;
}

function formatTimestamp(sec: number) {
  if (!Number.isFinite(sec) || sec <= 0) {
    return "No timestamp";
  }
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "medium",
    timeZone: "UTC"
  }).format(new Date(sec * 1000));
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
  const pendingRequestTimestampRef = useRef(0);
  const timestampRef = useRef(0);
  const [meta, setMeta] = useState<Metadata | null>(null);
  const [timestamp, setTimestamp] = useState(0);
  const [requestTimestamp, setRequestTimestamp] = useState(0);
  const [isPlaying, setIsPlaying] = useState(false);
  const [playbackSpeed, setPlaybackSpeed] = useState(60);
  const [error, setError] = useState<string | null>(null);

  const scheduleTileRequest = useCallback((nextTimestamp: number) => {
    pendingRequestTimestampRef.current = nextTimestamp;
    const now = performance.now();
    const elapsed = now - lastRequestAtRef.current;
    if (elapsed >= tileRequestThrottleMs) {
      if (requestTimerRef.current !== null) {
        window.clearTimeout(requestTimerRef.current);
        requestTimerRef.current = null;
      }
      lastRequestAtRef.current = now;
      setRequestTimestamp(nextTimestamp);
      return;
    }
    if (requestTimerRef.current !== null) {
      return;
    }
    requestTimerRef.current = window.setTimeout(() => {
      requestTimerRef.current = null;
      lastRequestAtRef.current = performance.now();
      setRequestTimestamp(pendingRequestTimestampRef.current);
    }, tileRequestThrottleMs - elapsed);
  }, []);

  useEffect(() => {
    let cancelled = false;
    fetch("/api/meta")
      .then((response) => {
        if (!response.ok) {
          throw new Error(`Metadata request failed: ${response.status}`);
        }
        return response.json() as Promise<Metadata>;
      })
      .then((nextMeta) => {
        if (cancelled) {
          return;
        }
        setMeta(nextMeta);
        const initialTimestamp = nextMeta.toSec || nextMeta.fromSec || 0;
        timestampRef.current = initialTimestamp;
        setTimestamp(initialTimestamp);
        setRequestTimestamp(initialTimestamp);
        pendingRequestTimestampRef.current = initialTimestamp;
        lastRequestAtRef.current = performance.now();
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err));
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    timestampRef.current = timestamp;
  }, [timestamp]);

  useEffect(() => {
    return () => {
      if (requestTimerRef.current !== null) {
        window.clearTimeout(requestTimerRef.current);
      }
    };
  }, []);

  useEffect(() => {
    if (!isPlaying || !meta || meta.fromSec === meta.toSec) {
      return;
    }
    const interval = window.setInterval(() => {
      const baseTimestamp = Number.isFinite(timestampRef.current)
        ? timestampRef.current
        : meta.fromSec;
      const nextTimestamp = Math.min(
        meta.toSec,
        Math.max(meta.fromSec, baseTimestamp + playbackSpeed)
      );
      timestampRef.current = nextTimestamp;
      setTimestamp(nextTimestamp);
      scheduleTileRequest(nextTimestamp);
      if (nextTimestamp >= meta.toSec) {
        setIsPlaying(false);
      }
    }, 1000);
    return () => {
      window.clearInterval(interval);
    };
  }, [isPlaying, meta, playbackSpeed, scheduleTileRequest]);

  useEffect(() => {
    if (!meta || !mapNode.current || mapRef.current) {
      return;
    }
    const bounds = L.latLngBounds([
      [-meta.canvasHeight, 0],
      [0, meta.canvasWidth]
    ]);
    const map = L.map(mapNode.current, {
      crs: L.CRS.Simple,
      minZoom: meta.minZoom,
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
    map.fitBounds(bounds);
    mapRef.current = map;
    return () => {
      map.remove();
      mapRef.current = null;
      activeLayerRef.current = null;
      pendingLayerRef.current = null;
      borderRef.current = null;
    };
  }, [meta]);

  useEffect(() => {
    const map = mapRef.current;
    if (!meta || !map || !requestTimestamp) {
      return;
    }
    const seq = ++renderSeqRef.current;
    pendingLayerRef.current?.remove();
    const bounds = L.latLngBounds([
      [-meta.canvasHeight, 0],
      [0, meta.canvasWidth]
    ]);
    let nextLayer: L.TileLayer | null = null;

    nextLayer = decodedTileLayer(tileLayerUrl(requestTimestamp), {
      tileSize: meta.tileSize,
      minZoom: meta.minZoom,
      maxZoom: maxDisplayZoom,
      maxNativeZoom: 0,
      bounds,
      noWrap: true,
      updateWhenIdle: false,
      updateWhenZooming: true,
      keepBuffer: 1,
      opacity: 1,
      className: "canvasTile"
    }).addTo(map);
    pendingLayerRef.current = nextLayer;

    nextLayer.once("load", () => {
      if (!nextLayer || renderSeqRef.current !== seq) {
        nextLayer?.remove();
        return;
      }
      nextLayer.bringToFront();
      const previousLayer = activeLayerRef.current;
      activeLayerRef.current = nextLayer;
      pendingLayerRef.current = null;
      setError(null);
      if (previousLayer && previousLayer !== nextLayer) {
        requestAnimationFrame(() => {
          requestAnimationFrame(() => {
            previousLayer.remove();
          });
        });
      }
    });
    nextLayer.once("tileerror", (event) => {
      if (!nextLayer || renderSeqRef.current !== seq) {
        return;
      }
      nextLayer.remove();
      if (pendingLayerRef.current === nextLayer) {
        pendingLayerRef.current = null;
      }
      const coords = (event as L.TileEvent).coords;
      setError(`Failed to load tile z${coords.z} ${coords.x},${coords.y} for ${requestTimestamp}`);
    });

    return () => {
      if (renderSeqRef.current !== seq) {
        return;
      }
      if (pendingLayerRef.current === nextLayer) {
        pendingLayerRef.current = null;
        nextLayer?.remove();
      } else if (nextLayer && activeLayerRef.current !== nextLayer) {
        nextLayer.remove();
      }
    };
  }, [meta, requestTimestamp]);

  const sliderDisabled = !meta || meta.fromSec === meta.toSec;
  const progress = useMemo(() => {
    if (!meta || meta.toSec <= meta.fromSec) {
      return 100;
    }
    return ((timestamp - meta.fromSec) / (meta.toSec - meta.fromSec)) * 100;
  }, [meta, timestamp]);

  return (
    <main className="shell">
      <aside className="sidebar">
        <div>
          <p className="eyebrow">timeShadeDB</p>
          <h1>Canvas query</h1>
        </div>

        <section className="controlGroup" aria-label="Timestamp">
          <div className="timestampReadout">{formatTimestamp(timestamp)}</div>
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
                if (meta && timestampRef.current >= meta.toSec) {
                  timestampRef.current = meta.fromSec;
                  setTimestamp(meta.fromSec);
                  scheduleTileRequest(meta.fromSec);
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
                onChange={(event) => {
                  setPlaybackSpeed(Number(event.currentTarget.value));
                }}
              >
                {playbackSpeedOptions.map((speed) => (
                  <option key={speed} value={speed}>
                    {speed}x
                  </option>
                ))}
              </select>
            </label>
          </div>
          <input
            className="timeSlider"
            type="range"
            min={meta?.fromSec ?? 0}
            max={meta?.toSec ?? 0}
            value={timestamp}
            step={1}
            disabled={sliderDisabled}
            style={{ "--progress": `${progress}%` } as SliderStyle}
            onInput={(event) => {
              const nextTimestamp = Number(event.currentTarget.value);
              timestampRef.current = nextTimestamp;
              setTimestamp(nextTimestamp);
              scheduleTileRequest(nextTimestamp);
            }}
            onChange={(event) => {
              const nextTimestamp = Number(event.currentTarget.value);
              timestampRef.current = nextTimestamp;
              setTimestamp(nextTimestamp);
              scheduleTileRequest(nextTimestamp);
            }}
          />
          <div className="rangeLabels">
            <span>{formatTimestamp(meta?.fromSec ?? 0)}</span>
            <span>{formatTimestamp(meta?.toSec ?? 0)}</span>
          </div>
        </section>

        <dl className="statsGrid">
          <div>
            <dt>Canvas</dt>
            <dd>{meta ? `${meta.canvasWidth} x ${meta.canvasHeight}` : "-"}</dd>
          </div>
          <div>
            <dt>Tiles</dt>
            <dd>{meta ? `${meta.tileCols} x ${meta.tileRows}` : "-"}</dd>
          </div>
          <div>
            <dt>Tile size</dt>
            <dd>{meta ? `${meta.tileSize}px` : "-"}</dd>
          </div>
          <div>
            <dt>Unix sec</dt>
            <dd>{timestamp || "-"}</dd>
          </div>
        </dl>

        {error ? <div className="errorBox">{error}</div> : null}
      </aside>

      <section className="mapPane" aria-label="Canvas map">
        <div ref={mapNode} className="map" />
      </section>
    </main>
  );
}
