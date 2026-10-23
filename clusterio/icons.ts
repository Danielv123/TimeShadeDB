// The map's icons from Clusterio's data export: `clusterioctl instance export-data` uploads the mod pack's icon
// spritesheet and metadata.json (lib.ExportMetadata: entries with sprite coordinates per prototype type, the shape the
// viewer's iconIndex reads) to the controller, which keeps them in its static directory under content-hashed names and
// lists them in the mod pack's export manifest. The map server serves the icons named by <db>/icons/icons.json
// (docs/FORMAT.md 5) and reads that file again when it changes.
import { copyFile, mkdir, readFile, rename, rm, stat, writeFile } from "node:fs/promises";
import * as path from "node:path";

/** The file in the icons directory that names the two icon files (the map server's api.IconsFile). */
export const ICONS_FILE = "icons.json";

/** A name the map server serves: [A-Za-z0-9._-], not starting with a dot (api.validIconName). */
const VALID_NAME = /^[A-Za-z0-9_-][A-Za-z0-9._-]{0,199}$/;

export type IconsResult = "copied" | "unchanged" | "none";

type IconsJson = { png: string; json: string };

/**
 * Copy the export's spritesheet and metadata (`assets` of a mod pack's export manifest, file names in `staticDir`)
 * into `iconsDir` under the same content-hashed names, each through a temporary file and a rename. icons.json is
 * written last, the same way, so the map server never sees it name a missing file. Then the files the previous
 * icons.json named are removed. "none" when the export has no spritesheet or metadata: nothing is touched.
 */
export async function syncIcons(
	staticDir: string,
	iconsDir: string,
	assets: Record<string, string> | undefined,
): Promise<IconsResult> {
	const png = assets?.spritesheet;
	const meta = assets?.metadata;
	if (!png || !meta) {
		return "none";
	}
	for (const name of [png, meta]) {
		if (!VALID_NAME.test(name) || name === ICONS_FILE) {
			throw new Error(`bad export file name ${JSON.stringify(name)}`);
		}
	}
	const next: IconsJson = { png: `icons/${png}`, json: `icons/${meta}` };
	const prev = await readIconsJson(iconsDir);
	if (prev?.png === next.png && prev.json === next.json && await exists(iconsDir, png) && await exists(iconsDir, meta)) {
		return "unchanged";
	}

	await mkdir(iconsDir, { recursive: true });
	for (const name of [png, meta]) {
		const tmp = path.join(iconsDir, `${name}.tmp`);
		await copyFile(path.join(staticDir, name), tmp);
		await replace(tmp, path.join(iconsDir, name));
	}
	const tmp = path.join(iconsDir, `${ICONS_FILE}.tmp`);
	await writeFile(tmp, JSON.stringify(next));
	await replace(tmp, path.join(iconsDir, ICONS_FILE));

	// The previous files, unless named again. A file the map server is reading at that moment may refuse the removal
	// on Windows; it is left behind.
	for (const old of [prev?.png, prev?.json]) {
		const name = old?.startsWith("icons/") ? old.slice("icons/".length) : undefined;
		if (name && VALID_NAME.test(name) && name !== ICONS_FILE && name !== png && name !== meta) {
			await rm(path.join(iconsDir, name), { force: true }).catch(() => undefined);
		}
	}
	return "copied";
}

async function readIconsJson(dir: string): Promise<Partial<IconsJson> | undefined> {
	try {
		const parsed = JSON.parse(await readFile(path.join(dir, ICONS_FILE), "utf8"));
		return parsed && typeof parsed === "object" ? parsed : undefined;
	} catch {
		return undefined;
	}
}

async function exists(dir: string, name: string): Promise<boolean> {
	try {
		return (await stat(path.join(dir, name))).isFile();
	} catch {
		return false;
	}
}

/** rename(), tried again for up to a second while Windows refuses it because the map server has the target open. */
async function replace(from: string, to: string): Promise<void> {
	for (let attempt = 0; ; attempt++) {
		try {
			await rename(from, to);
			return;
		} catch (err: any) {
			if (attempt >= 10 || !["EPERM", "EACCES", "EBUSY"].includes(err?.code)) {
				throw err;
			}
			await new Promise(resolve => setTimeout(resolve, 100));
		}
	}
}
