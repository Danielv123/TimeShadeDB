import { defineConfig } from "vite";
import { clusterioPlugin } from "@clusterio/web_ui/vite.mjs";

// The plugin bundle the Clusterio web UI loads (docs/writing-plugins.md, webEntrypoint): index.ts, package.json and
// web/index.tsx, with the modules the web UI shares (@clusterio/lib, react, antd, typebox, ...) left to its import map.
export default defineConfig({
	plugins: [clusterioPlugin()],
});
