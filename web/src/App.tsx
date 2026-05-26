import { type CSSProperties, useEffect, useMemo, useRef, useState } from "react";
import L from "leaflet";

type Metadata = {
  canvasWidth: number;
  canvasHeight: number;
  tileSize: number;
  tileCols: number;
  tileRows: number;
  fromSec: number;
  toSec: number;
};

type SliderStyle = CSSProperties & {
  "--progress": string;
};

const tileRequestThrottleMs = 100;

function tileLayerUrl(timestamp: number) {
  return `/api/tiles/0/{x}/{y}.png?ts=${timestamp}`;
}

function concreteTileUrl(timestamp: number, x: number, y: number) {
  return `/api/tiles/0/${x}/${y}.png?ts=${timestamp}`;
}

function loadTileImage(src: string) {
  return new Promise<void>((resolve, reject) => {
    const img = new Image();
    img.onload = () => {
      if (typeof img.decode !== "function") {
        resolve();
        return;
      }
      img.decode().then(resolve, reject);
    };
    img.onerror = () => reject(new Error(`Failed to load ${src}`));
    img.src = src;
  });
}

function preloadTimestampTiles(meta: Metadata, timestamp: number) {
  const loads: Promise<void>[] = [];
  for (let y = 0; y < meta.tileRows; y++) {
    for (let x = 0; x < meta.tileCols; x++) {
      loads.push(loadTileImage(concreteTileUrl(timestamp, x, y)));
    }
  }
  return Promise.all(loads);
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
  const [meta, setMeta] = useState<Metadata | null>(null);
  const [timestamp, setTimestamp] = useState(0);
  const [requestTimestamp, setRequestTimestamp] = useState(0);
  const [error, setError] = useState<string | null>(null);

  const scheduleTileRequest = (nextTimestamp: number) => {
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
  };

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
    return () => {
      if (requestTimerRef.current !== null) {
        window.clearTimeout(requestTimerRef.current);
      }
    };
  }, []);

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
      minZoom: -2,
      maxZoom: 0,
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
    let cancelled = false;

    preloadTimestampTiles(meta, requestTimestamp)
      .then(() => {
        if (cancelled || renderSeqRef.current !== seq) {
          return;
        }
        nextLayer = L.tileLayer(tileLayerUrl(requestTimestamp), {
          tileSize: meta.tileSize,
          minNativeZoom: 0,
          maxNativeZoom: 0,
          bounds,
          noWrap: true,
          updateWhenIdle: false,
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
          setError(`Failed to load tile ${coords.x},${coords.y} for ${requestTimestamp}`);
        });
      })
      .catch((err: unknown) => {
        if (cancelled || renderSeqRef.current !== seq) {
          return;
        }
        setError(err instanceof Error ? err.message : String(err));
      });

    return () => {
      cancelled = true;
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
              setTimestamp(nextTimestamp);
              scheduleTileRequest(nextTimestamp);
            }}
            onChange={(event) => {
              const nextTimestamp = Number(event.currentTarget.value);
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
