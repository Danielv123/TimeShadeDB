// Publishes the plugin and its binary packages (scripts/targets.mjs) to npm at the version in package.json: each one
// that npm does not have at that version yet. It builds the viewer, the binaries and the plugin, runs the plugin's
// tests, then publishes the binary packages before the plugin, whose published package.json names them in
// optionalDependencies. The committed package.json does not, so npm ci works while they are not on npm yet.
//
// The release workflow (.github/workflows/release.yml) runs it with npm trusted publishing. npm only takes a trusted
// publisher for a package it already has, so a new package is published once by hand: npm login, then this script.
//
//   node scripts/publish.mjs [--check] [--dry-run]
//
// --check only says which packages are not on npm at the version, and writes release=true|false and version to
// $GITHUB_OUTPUT. In GitHub Actions publishing fails at once for a package that is not on npm at all.
import { execSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { packageName, targets } from "./targets.mjs";

const plugin = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const args = process.argv.slice(2);
const pkg = JSON.parse(fs.readFileSync(path.join(plugin, "package.json"), "utf8"));
const version = pkg.version;

const run = (cmd, cwd = plugin) => execSync(cmd, { cwd, stdio: "inherit" });

/** The versions npm has of package name, or null when it has no such package. */
function versions(name) {
	try {
		const out = execSync(`npm view ${name} versions --json`, { cwd: plugin, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
		const v = JSON.parse(out || "[]");
		return Array.isArray(v) ? v : [v];
	} catch (err) {
		if (/E404/.test(`${err.stdout}${err.stderr}`)) {
			return null;
		}
		throw err;
	}
}

const binaries = targets.map(packageName);
const pending = [];
const missing = [];
for (const name of [...binaries, pkg.name]) {
	const v = versions(name);
	if (v === null) {
		missing.push(name);
	}
	if (!v?.includes(version)) {
		pending.push(name);
	}
}
console.log(pending.length > 0 ? `not on npm at ${version}: ${pending.join(", ")}` : `every package is on npm at ${version}`);

if (args.includes("--check")) {
	if (process.env.GITHUB_OUTPUT) {
		fs.appendFileSync(process.env.GITHUB_OUTPUT, `release=${pending.length > 0}\nversion=${version}\n`);
	}
	process.exit(0);
}
if (pending.length === 0) {
	process.exit(0);
}
if (missing.length > 0 && process.env.GITHUB_ACTIONS) {
	console.log(`::error::${missing.join(", ")} not on npm: trusted publishing cannot create a package. Publish it once ` +
		"by hand (npm login, then node clusterio/scripts/publish.mjs) and add the trusted publisher on npmjs.com.");
	process.exit(1);
}

run("npm ci");
// the viewer with relative asset URLs (every binary embeds it), and bin/ for the plugin's end to end test
run("node scripts/build-binary.mjs --web");
run("node scripts/build-npm-binaries.mjs");
run("npm run build");
run("npm test");

const publish = dir => run(`npm publish${args.includes("--dry-run") ? " --dry-run" : ""}`, dir);
for (const name of binaries) {
	if (pending.includes(name)) {
		publish(path.join(plugin, "npm", name.replace("/", "__")));
	}
}
if (pending.includes(pkg.name)) {
	const stage = path.join(plugin, "build", "publish");
	fs.rmSync(stage, { recursive: true, force: true });
	fs.mkdirSync(stage, { recursive: true });
	for (const f of ["dist", "module", "LICENSE-BlueOak-1.0.0.md", "README.md"]) {
		fs.cpSync(path.join(plugin, f), path.join(stage, f), { recursive: true });
	}
	const optionalDependencies = Object.fromEntries(binaries.map(name => [name, version]));
	fs.writeFileSync(path.join(stage, "package.json"), JSON.stringify({ ...pkg, optionalDependencies }, null, "\t") + "\n");
	publish(stage);
}
