import React from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";
import { App } from "./App";
import { perf } from "./perf";
import { boot, takeEarly } from "./map/boot";

// Start the perf hooks and the catalog request before React renders, so the first tiles
// are requested as early as possible.
perf();
// The map fills the window apart from two bars; the speculative first view request only needs
// a superset of the visible tiles, so the window size is enough.
const started = boot(location.search, { width: window.innerWidth, height: window.innerHeight, dpr: window.devicePixelRatio || 1 }, { early: takeEarly() });

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App boot={started} />
  </React.StrictMode>
);
