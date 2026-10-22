// Forwards envelopes to the sidecar's POST /api/v3/ingest (docs/API.md 5) in order, one request at a time:
// gzip NDJSON bodies of about 1 MiB, retried on network errors, 5xx and 429 with backoff (honouring Retry-After),
// split on 400 and 413, and a single refused line dropped. Ingest is idempotent, so a retried body is harmless. While
// the sidecar is down the queue holds up to maxQueueBytes in memory; past it the queue continues on disk in spoolDir
// (spool.ts), up to maxDiskBytes, and close() keeps what it holds there for the next start. Without a spoolDir the
// oldest lines are dropped instead.
import { promisify } from "node:util";
import * as zlib from "node:zlib";
import type * as lib from "@clusterio/lib";
import { lifecycle, rateLimited } from "./runtime.js";
import { DiskTail } from "./spool.js";

const gzip = promisify(zlib.gzip);

export type Target = { url: string; token: string };

export type IngestQueueOptions = {
	/** Where to post now, or undefined while the sidecar is not running. */
	target: () => Target | undefined;
	logger: lib.Logger;
	maxQueueBytes?: number;
	maxBatchBytes?: number;
	minBackoffMs?: number;
	maxBackoffMs?: number;
	/** Wait between checks while there is no target. */
	idleMs?: number;
	requestTimeoutMs?: number;
	fetch?: typeof globalThis.fetch;
	/** Where the queue continues past maxQueueBytes, and what it holds is kept across restarts (none: drop the oldest). */
	spoolDir?: string;
	/** The disk part's bound; past it its oldest lines are dropped. */
	maxDiskBytes?: number;
};

/** The counts of core.IngestResponse the queue reads. */
type IngestResponse = { bad_lines?: number; bad_messages?: number; errors?: string[] };

export type QueueStats = {
	queuedLines: number;
	queuedBytes: number;
	/** Of the queued lines, those on disk (spoolDir), and their bytes. */
	diskLines: number;
	diskBytes: number;
	sentLines: number;
	droppedLines: number;
	refusedLines: number;
	retries: number;
	badLines: number;
	badMessages: number;
};

export class IngestQueue {
	private lines: string[] = [];
	private head = 0;
	private bytes = 0;
	private running = false;
	private closed = false;
	private wakeUp: (() => void) | undefined;
	private sleepTimer: NodeJS.Timeout | undefined;
	private drained: (() => void)[] = [];
	private readonly opts: Required<Omit<IngestQueueOptions, "spoolDir" | "maxDiskBytes">> & Pick<IngestQueueOptions, "spoolDir" | "maxDiskBytes">;
	private readonly warn: ReturnType<typeof rateLimited>;
	private failures = 0;
	/** Bytes of the lines being posted (taken off the queue while their request runs). */
	private inflightBytes = 0;
	/** Lines the next request may carry at most (halved by a 400 or 413). */
	private splitTo = Infinity;
	/** The lines being posted (kept when the queue closes mid-request). */
	private inflight: string[] = [];
	/**
	 * The disk part (spoolDir). The memory lines are either all pushed directly, or all from the segment fromSeg taken
	 * from the disk, of which fromSegLeft are not acknowledged yet; new lines go to the disk while it holds any or while
	 * such a segment is out, so the order stays memory, then disk.
	 */
	private readonly disk: DiskTail | undefined;
	private fromSeg: number | undefined;
	private fromSegLeft = 0;
	private diskBuf: string[] = [];
	private diskBufBytes = 0;
	private diskTimer: NodeJS.Immediate | undefined;
	readonly stats: QueueStats = {
		queuedLines: 0, queuedBytes: 0, diskLines: 0, diskBytes: 0, sentLines: 0, droppedLines: 0, refusedLines: 0,
		retries: 0, badLines: 0, badMessages: 0,
	};

	constructor(opts: IngestQueueOptions) {
		this.opts = {
			maxQueueBytes: 64 << 20,
			maxBatchBytes: 1 << 20,
			minBackoffMs: 500,
			maxBackoffMs: 30_000,
			idleMs: 1000,
			requestTimeoutMs: 60_000,
			fetch: globalThis.fetch,
			...opts,
		};
		this.warn = rateLimited(opts.logger);
		if (opts.spoolDir) {
			this.disk = new DiskTail(opts.spoolDir, opts.maxDiskBytes);
			if (!this.disk.empty) {
				lifecycle(opts.logger, "timeshade.ingest", "spool_loaded", { lines: this.disk.lines, bytes: this.disk.bytes });
				this.compact();
				this.pump();
			}
		}
	}

	/** Lines waiting: in memory, buffered for the disk and on it. */
	get length(): number {
		return this.memLength + this.diskBuf.length + (this.disk?.lines ?? 0);
	}

	private get memLength(): number {
		return this.lines.length - this.head;
	}

	setMaxQueueBytes(n: number): void {
		this.opts.maxQueueBytes = n;
		this.trim();
	}

	/** Queues one NDJSON line (no newline). */
	push(line: string): void {
		if (this.closed) {
			return;
		}
		const disk = this.disk;
		if (disk && (!disk.empty || this.diskBuf.length > 0 || this.fromSeg !== undefined ||
			this.bytes + this.inflightBytes + line.length + 1 > this.opts.maxQueueBytes)) {
			this.diskBuf.push(line);
			this.diskBufBytes += line.length + 1;
			if (this.diskBufBytes >= 1 << 20) {
				this.flushDisk();
			} else {
				this.diskTimer ??= setImmediate(() => this.flushDisk());
			}
			this.compact();
			this.pump();
			return;
		}
		this.lines.push(line);
		this.bytes += line.length + 1;
		this.trim();
		this.pump();
	}

	/** Writes the lines buffered for the disk. */
	private flushDisk(): void {
		if (this.diskTimer) {
			clearImmediate(this.diskTimer);
			this.diskTimer = undefined;
		}
		if (!this.disk || this.diskBuf.length === 0) {
			return;
		}
		const before = this.disk.dropped;
		this.disk.append(this.diskBuf);
		this.diskBuf = [];
		this.diskBufBytes = 0;
		if (this.disk.dropped > before) {
			this.stats.droppedLines += this.disk.dropped - before;
			this.warn("dropped", "timeshade.ingest disk queue full, oldest dropped", {
				op: "timeshade.ingest", phase: "dropped", lines: this.disk.dropped - before, diskBytes: this.disk.bytes,
			});
		}
		this.compact();
	}

	/** With the memory part sent, the oldest segment of the disk part becomes the memory part. */
	private refill(): void {
		if (!this.disk || this.memLength > 0 || this.fromSeg !== undefined) {
			return;
		}
		this.flushDisk();
		const t = this.disk.take();
		if (!t) {
			return;
		}
		this.lines = t.lines;
		this.head = 0;
		this.bytes = t.lines.reduce((n, l) => n + l.length + 1, 0);
		this.fromSeg = t.seg;
		this.fromSegLeft = t.lines.length;
		if (t.lines.length === 0) {
			this.ack(0);
		}
	}

	/** n lines of the memory part were acknowledged (or refused for good). */
	private ack(n: number): void {
		if (this.fromSeg === undefined) {
			return;
		}
		this.fromSegLeft -= n;
		if (this.fromSegLeft <= 0) {
			this.disk!.done(this.fromSeg);
			this.fromSeg = undefined;
			this.fromSegLeft = 0;
		}
	}

	/** Try now (the sidecar became ready). */
	wake(): void {
		this.failures = 0;
		const w = this.wakeUp;
		this.wakeUp = undefined;
		clearTimeout(this.sleepTimer);
		w?.();
	}

	/** Resolves once the queue is empty, or after timeoutMs; true when it emptied. */
	drain(timeoutMs: number): Promise<boolean> {
		if (this.length === 0 && !this.running) {
			return Promise.resolve(true);
		}
		return new Promise(resolve => {
			const t = setTimeout(() => {
				this.drained = this.drained.filter(d => d !== done);
				resolve(false);
			}, timeoutMs);
			const done = () => {
				clearTimeout(t);
				resolve(true);
			};
			this.drained.push(done);
		});
	}

	/**
	 * Stops sending. With a spoolDir what is queued (also a request in flight) stays on disk for the next start;
	 * without one it is lost.
	 */
	close(): void {
		this.closed = true;
		if (this.disk) {
			this.flushDisk();
			if (this.fromSeg === undefined) {
				// Memory lines pushed directly: before everything on disk. (Lines from a taken segment are still in its
				// file, which the next start reads again; the acknowledged ones are skipped by the map server.)
				this.disk.putBack(this.inflight.concat(this.lines.slice(this.head)));
			}
			this.disk.close();
			this.lines = [];
			this.head = 0;
			this.bytes = 0;
		}
		this.wake();
	}

	private trim() {
		if (this.disk) {
			return; // past maxQueueBytes lines go to the disk (push)
		}
		let dropped = 0;
		while (this.bytes + this.inflightBytes > this.opts.maxQueueBytes && this.length > 1) {
			this.bytes -= this.lines[this.head].length + 1;
			this.head += 1;
			dropped += 1;
		}
		if (dropped > 0) {
			this.stats.droppedLines += dropped;
			this.warn("dropped", "timeshade.ingest queue full, oldest dropped", {
				op: "timeshade.ingest", phase: "dropped", lines: dropped, queuedBytes: this.bytes,
			});
		}
		this.compact();
	}

	private compact() {
		if (this.head > 1024 && this.head * 2 > this.lines.length) {
			this.lines = this.lines.slice(this.head);
			this.head = 0;
		}
		this.stats.queuedLines = this.length;
		this.stats.diskLines = (this.disk?.lines ?? 0) + this.diskBuf.length;
		this.stats.diskBytes = (this.disk?.bytes ?? 0) + this.diskBufBytes;
		this.stats.queuedBytes = this.bytes + this.stats.diskBytes;
	}

	private sleep(ms: number): Promise<void> {
		return new Promise(resolve => {
			this.wakeUp = resolve;
			this.sleepTimer = setTimeout(() => {
				this.wakeUp = undefined;
				resolve();
			}, ms);
		});
	}

	private backoff(retryAfter?: string | null): number {
		const seconds = retryAfter ? Number(retryAfter) : NaN;
		const ms = Number.isFinite(seconds) && seconds >= 0
			? seconds * 1000
			: this.opts.minBackoffMs * (2 ** Math.min(this.failures, 16));
		this.failures += 1;
		return Math.min(this.opts.maxBackoffMs, ms);
	}

	private pump() {
		if (this.running || this.closed) {
			return;
		}
		this.running = true;
		this.loop().catch(err => {
			this.opts.logger.error(`timeshade ingest loop failed:\n${err?.stack ?? err}`);
		}).finally(() => {
			this.running = false;
			if (this.length > 0 && !this.closed) {
				this.pump();
			}
		});
	}

	private async loop() {
		while (!this.closed && this.length > 0) {
			const target = this.opts.target();
			if (!target) {
				await this.sleep(this.opts.idleMs);
				continue;
			}
			this.refill();
			if (this.memLength === 0) {
				// The lines are still buffered for the disk: write them, then take them.
				this.flushDisk();
				continue;
			}
			// The batch leaves the queue while it is posted, so trim() never drops a line that is in flight.
			const n = this.batchSize();
			const lines = this.lines.slice(this.head, this.head + n);
			let size = 0;
			for (const line of lines) {
				size += line.length + 1;
			}
			this.head += n;
			this.bytes -= size;
			this.inflightBytes = size;
			this.inflight = lines;
			this.compact();
			let status: "ok" | "drop" | "retry";
			try {
				status = await this.post(target, lines);
			} finally {
				this.inflightBytes = 0;
				this.inflight = [];
			}
			if (status === "retry") {
				if (this.closed) {
					return;
				}
				this.lines = lines.concat(this.lines.slice(this.head));
				this.head = 0;
				this.bytes += size;
				this.trim();
			} else if (status === "ok") {
				this.failures = 0;
				this.splitTo = Infinity;
				this.stats.sentLines += n;
				this.ack(n);
			} else {
				this.stats.refusedLines += n;
				this.ack(n);
			}
		}
		this.compact();
		if (this.length === 0) {
			const waiting = this.drained;
			this.drained = [];
			for (const done of waiting) {
				done();
			}
		}
	}

	private batchSize(): number {
		let n = 0;
		let bytes = 0;
		const max = Math.min(this.memLength, this.splitTo);
		while (n < max && (n === 0 || bytes + this.lines[this.head + n].length + 1 <= this.opts.maxBatchBytes)) {
			bytes += this.lines[this.head + n].length + 1;
			n += 1;
		}
		return n;
	}

	/** One request: "ok" (acknowledged), "drop" (refused for good) or "retry" (after a wait, done here). */
	private async post(target: Target, lines: string[]): Promise<"ok" | "drop" | "retry"> {
		let res: Response;
		try {
			const body = await gzip(Buffer.from(lines.join("\n") + "\n"));
			res = await this.opts.fetch(`${target.url}/api/v3/ingest`, {
				method: "POST",
				headers: {
					"Authorization": `Bearer ${target.token}`,
					"Content-Type": "application/x-ndjson",
					"Content-Encoding": "gzip",
				},
				body,
				signal: AbortSignal.timeout(this.opts.requestTimeoutMs),
			});
		} catch (err: any) {
			this.stats.retries += 1;
			this.warn("network", "timeshade.ingest request failed", { op: "timeshade.ingest", phase: "retry", error: err?.message });
			await this.sleep(this.backoff());
			return "retry";
		}
		if (res.status === 200) {
			let answer: IngestResponse = {};
			try {
				answer = await res.json() as IngestResponse;
			} catch {
				// An acknowledgement all the same.
			}
			const bad = (answer.bad_lines ?? 0) + (answer.bad_messages ?? 0);
			this.stats.badLines += answer.bad_lines ?? 0;
			this.stats.badMessages += answer.bad_messages ?? 0;
			if (bad > 0) {
				this.warn("bad", "timeshade.ingest refused part of a batch", {
					op: "timeshade.ingest", phase: "bad", badLines: answer.bad_lines ?? 0,
					badMessages: answer.bad_messages ?? 0, errors: (answer.errors ?? []).slice(0, 3),
				});
			}
			return "ok";
		}
		const text = await res.text().catch(() => "");
		if ((res.status === 400 || res.status === 413) && lines.length > 1) {
			this.splitTo = Math.max(1, Math.ceil(lines.length / 2));
			return "retry";
		}
		if (res.status === 400 || res.status === 413) {
			this.warn(`refused:${res.status}`, "timeshade.ingest refused a line", {
				op: "timeshade.ingest", phase: "refused", status: res.status, error: text.slice(0, 200),
			});
			return "drop";
		}
		this.stats.retries += 1;
		const level = res.status === 401 || res.status === 403 ? "error" : "warn";
		const key = `status:${res.status}`;
		if (level === "error") {
			lifecycle(this.opts.logger, "timeshade.ingest", "unauthorized", { status: res.status }, "error");
		} else {
			this.warn(key, "timeshade.ingest retry", { op: "timeshade.ingest", phase: "retry", status: res.status });
		}
		await this.sleep(this.backoff(res.headers.get("retry-after")));
		return "retry";
	}
}
