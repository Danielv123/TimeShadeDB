// Copies the capture's Clusterio save-patch module into module/, where Clusterio looks for a plugin's module
// (packages/host/src/patch.ts): node ../capture/build.mjs assembles it from capture/clusterio_module and capture/lua.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const plugin = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const capture = path.resolve(plugin, "..", "capture");
const out = path.join(plugin, "build", "capture");
fs.rmSync(out, { recursive: true, force: true });
execFileSync(process.execPath, [path.join(capture, "build.mjs"), "--out", out, "--no-zip"], { stdio: ["ignore", "ignore", "inherit"] });
const src = path.join(out, "clusterio_module");
const info = JSON.parse(fs.readFileSync(path.join(src, "module.json"), "utf8"));
if (info.name !== "timeshade") {
	throw new Error(`the module is named ${info.name}; Clusterio needs it named after the plugin (timeshade)`);
}
const dst = path.join(plugin, "module");
fs.rmSync(dst, { recursive: true, force: true });
fs.cpSync(src, dst, { recursive: true });
console.log(`${dst} (module ${info.name} ${info.version}, ${fs.readdirSync(dst).length} files)`);
