import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

// web/dist/.keep is committed so `go build ./...` works on a clean checkout (web_embed.go
// embeds all:web/dist). Vite empties dist before each build, so put the file back afterwards.
function keepDistPlaceholder(): Plugin {
  let outDir = "dist";
  return {
    name: "timeshadedb-keep-dist",
    apply: "build",
    configResolved(config) {
      outDir = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      writeFileSync(resolve(outDir, ".keep"), "");
    }
  };
}

export default defineConfig({
  plugins: [react(), keepDistPlaceholder()],
  server: {
    port: 5173,
    proxy: {
      "/api": "http://127.0.0.1:8080"
    }
  }
});
