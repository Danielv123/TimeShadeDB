/**
 * The viewer's URL behind the plugin's proxy: its page at <web root>api/timeshade/, with its API base (an absolute
 * path, because the viewer's tile worker resolves URLs against its own script) and the view token. `root` is the
 * web UI's root as an absolute URL.
 */
export function viewerUrl(root: string, token: string): string {
	const page = new URL("api/timeshade/", root);
	page.searchParams.set("data", `${page.pathname}api/v3/`);
	page.searchParams.set("token", token);
	return page.pathname + page.search;
}

/** When to fetch a new token: 10 minutes before it expires, at least a minute from now. */
export function refreshDelay(expiresMs: number, now: number): number {
	return Math.max(60_000, expiresMs - now - 10 * 60_000);
}
