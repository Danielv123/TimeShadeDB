// Builds timeshadedb for this machine into bin/, where the plugin finds it when no binary package is installed (a
// development setup). --web builds the viewer first (with relative asset URLs, so it works behind the proxy), which
// the binary embeds; without it the binary embeds whatever web/dist holds.
import { execFileSync, execSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

const plugin = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repo = path.resolve(plugin, "..");
if (process.argv.includes("--web")) {
	const web = path.join(repo, "web");
	execSync("npm ci", { cwd: web, stdio: "inherit" });
	execSync("npm run build -- --base ./", { cwd: web, stdio: "inherit" });
}
const out = path.join(plugin, "bin", process.platform === "win32" ? "timeshadedb.exe" : "timeshadedb");
execFileSync("go", ["build", "-o", out, "./cmd/timeshadedb"], { cwd: repo, stdio: "inherit" });
console.log(out);
