// Where the timeshadedb executable comes from: a configured path, else an environment variable, else
// the npm package built for this platform (an optional dependency, as esbuild ships its binaries), else bin/ in the
// plugin directory (a development build: npm run build:binary).
import * as fs from "node:fs";
import { createRequire } from "node:module";
import * as path from "node:path";

/** The npm package that carries the binary for a platform, e.g. @timeshadedb/bin-win32-x64. */
export function binaryPackage(platform: string = process.platform, arch: string = process.arch): string {
	return `@timeshadedb/bin-${platform}-${arch}`;
}

export function binaryName(platform: string = process.platform): string {
	return platform === "win32" ? "timeshadedb.exe" : "timeshadedb";
}

export type Found = { path: string; source: "config" | "env" | "package" | "local" };

export type FindOptions = {
	configured?: string | null;
	env?: NodeJS.ProcessEnv;
	/** The plugin's directory (where package.json is). */
	pluginDir: string;
	platform?: string;
	arch?: string;
	exists?: (file: string) => boolean;
	/** Resolves a package's package.json, or throws (tests). */
	resolve?: (request: string) => string;
};

const isFile = (file: string) => {
	try {
		return fs.statSync(file).isFile();
	} catch {
		return false;
	}
};

/**
 * The binary to run, or an Error that says every place looked at. A configured path or environment variable that
 * does not exist is an error, not a reason to fall back: the operator asked for that file.
 */
export function findBinary(opts: FindOptions): Found | Error {
	const exists = opts.exists ?? isFile;
	const env = opts.env ?? process.env;
	const platform = opts.platform ?? process.platform;
	const arch = opts.arch ?? process.arch;
	const name = binaryName(platform);
	if (opts.configured) {
		const file = path.resolve(opts.configured);
		return exists(file) ? { path: file, source: "config" } : new Error(`timeshade.binary_path ${file} does not exist`);
	}
	if (env.TIMESHADEDB_BINARY) {
		const file = path.resolve(env.TIMESHADEDB_BINARY);
		return exists(file) ? { path: file, source: "env" } : new Error(`TIMESHADEDB_BINARY ${file} does not exist`);
	}
	const looked: string[] = [];
	const pkg = binaryPackage(platform, arch);
	const resolve = opts.resolve ?? createRequire(path.join(opts.pluginDir, "package.json")).resolve;
	try {
		const file = path.join(path.dirname(resolve(`${pkg}/package.json`)), "bin", name);
		if (exists(file)) {
			return { path: file, source: "package" };
		}
		looked.push(file);
	} catch {
		looked.push(`package ${pkg} (not installed)`);
	}
	const local = path.join(opts.pluginDir, "bin", name);
	if (exists(local)) {
		return { path: local, source: "local" };
	}
	looked.push(local);
	return new Error(`no timeshadedb binary for ${platform}-${arch}: set timeshade.binary_path; looked at ${looked.join(", ")}`);
}
