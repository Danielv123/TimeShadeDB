// Builds the per-platform binary packages (decision D5) into npm/<package>/: package.json (os, cpu) and bin/. Publish
// each with npm publish, then the plugin, whose optionalDependencies name them at the same version; npm installs
// only the one matching the machine. Build the viewer first (npm run build:binary -- --web, or web's own build with
// --base ./): every binary embeds web/dist.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { binaryName, packageName, targets } from "./targets.mjs";

const plugin = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repo = path.resolve(plugin, "..");
const pkg = JSON.parse(fs.readFileSync(path.join(plugin, "package.json"), "utf8"));
for (const t of targets) {
	const name = packageName(t);
	if (pkg.optionalDependencies?.[name] !== pkg.version) {
		throw new Error(`package.json optionalDependencies must name ${name} at ${pkg.version}`);
	}
	const dir = path.join(plugin, "npm", name.replace("/", "__"));
	fs.rmSync(dir, { recursive: true, force: true });
	fs.mkdirSync(path.join(dir, "bin"), { recursive: true });
	const env = { ...process.env, GOOS: t.goos, GOARCH: t.goarch, CGO_ENABLED: "0" };
	execFileSync("go", ["build", "-trimpath", "-o", path.join(dir, "bin", binaryName(t)), "./cmd/timeshadedb"], { cwd: repo, env, stdio: "inherit" });
	fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify({
		name,
		version: pkg.version,
		description: `The timeshadedb binary for ${t.platform}-${t.arch}, used by ${pkg.name}`,
		license: pkg.license,
		os: [t.platform],
		cpu: [t.arch],
		files: ["bin"],
	}, null, "\t") + "\n");
	console.log(dir);
}
