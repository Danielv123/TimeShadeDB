// SPDX-License-Identifier: BlueOak-1.0.0
// Ported from illisis/minimap runtime.ts (Blue Oak Model License 1.0.0, see LICENSE-BlueOak-1.0.0.md).
import { randomBytes } from "node:crypto";
import type * as lib from "@clusterio/lib";

export type Level = "info" | "verbose" | "warn" | "error";

/** Log one lifecycle phase as "<op> <phase>", with op and phase as metadata (as Universal Edges logs them). */
export function lifecycle(
	logger: lib.Logger,
	op: string,
	phase: string,
	fields: Record<string, unknown> = {},
	level: Level = "info",
): void {
	logger.log(level, `${op} ${phase}`, { ...fields, op, phase });
}

/**
 * A warner for repeated warnings. Each key reaches the log at most once per window (60 s), and each line carries
 * `count`: the occurrences since the key's previous line, this one included.
 */
export function rateLimited(logger: lib.Logger, windowMs = 60_000, now: () => number = Date.now) {
	const last = new Map<string, { at: number; count: number }>();
	return (key: string, message: string, fields: Record<string, unknown> = {}): void => {
		const at = now();
		const entry = last.get(key);
		if (entry && at - entry.at < windowMs) {
			entry.count += 1;
			return;
		}
		logger.warn(message, { ...fields, count: (entry?.count ?? 0) + 1 });
		// The keys are a fixed set of warning names; a clear at 1000 bounds the map should that ever change.
		if (last.size >= 1000) {
			last.clear();
		}
		last.set(key, { at, count: 0 });
	};
}

const processBootId = randomBytes(4).toString("hex");
let seq = 0;

/** `<boot id>.<seq>` for this process: unique across restarts (resync traces). */
export function newId(): string {
	seq += 1;
	return `${processBootId}.${seq}`;
}
