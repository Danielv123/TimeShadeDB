// SPDX-License-Identifier: BlueOak-1.0.0
// The part of a line queue that does not fit in memory, on disk (docs/API.md 5: a forwarder that cannot send keeps
// what it has rather than dropping it). A queue keeps its oldest lines in memory and, once those pass its memory
// bound, appends every new line here instead, so the order is: memory, then the segments oldest first. When the memory
// part is sent the queue takes the oldest segment back (take); the segment's file is deleted only once the queue says
// all of its lines were acknowledged (done), so a crash resends them (the map server skips envelopes it applied
// already). A stopping queue writes what it holds in memory to a segment before the others (putBack), and the next
// start continues from there.
import * as fs from "node:fs";
import * as path from "node:path";

/** Segment files are <number>.ndjson; new segments start past this many bytes. */
const SEGMENT_BYTES = 8 << 20;

export class DiskTail {
	/** Segment numbers on disk, oldest first. */
	private segs: number[] = [];
	private writeFd: number | undefined;
	private writeSeg: number | undefined;
	private writeBytes = 0;
	/** Bytes of the segments not taken yet, and their lines (counted as they are appended or loaded). */
	bytes = 0;
	lines = 0;
	/** Lines dropped because the disk part passed maxBytes (the oldest segments go). */
	dropped = 0;

	constructor(private readonly dir: string, private readonly maxBytes = 4 * 1024 ** 3) {
		fs.mkdirSync(dir, { recursive: true });
		for (const name of fs.readdirSync(dir)) {
			const m = /^(-?\d+)\.ndjson$/.exec(name);
			if (m) {
				this.segs.push(Number(m[1]));
			}
		}
		this.segs.sort((a, b) => a - b);
		for (const s of this.segs) {
			const text = fs.readFileSync(this.file(s), "utf8");
			this.bytes += Buffer.byteLength(text);
			this.lines += countLines(text);
		}
	}

	get empty(): boolean {
		return this.lines === 0;
	}

	private file(seg: number): string {
		return path.join(this.dir, `${seg}.ndjson`);
	}

	/** Appends lines (without newlines) after everything on disk. */
	append(lines: string[]): void {
		if (lines.length === 0) {
			return;
		}
		if (this.writeFd === undefined || this.writeBytes >= SEGMENT_BYTES) {
			this.closeWrite();
			const seg = this.segs.length > 0 ? this.segs[this.segs.length - 1] + 1 : 0;
			this.segs.push(seg);
			this.writeSeg = seg;
			this.writeFd = fs.openSync(this.file(seg), "a");
			this.writeBytes = 0;
		}
		const text = lines.join("\n") + "\n";
		fs.writeSync(this.writeFd!, text);
		const n = Buffer.byteLength(text);
		this.writeBytes += n;
		this.bytes += n;
		this.lines += lines.length;
		this.trim();
	}

	/** Drops the oldest segments while the disk part is over maxBytes (keeping the one being written). */
	private trim(): void {
		while (this.bytes > this.maxBytes && this.segs.length > 1) {
			const seg = this.segs.shift()!;
			const text = readOrEmpty(this.file(seg));
			this.bytes -= Buffer.byteLength(text);
			const n = countLines(text);
			this.lines -= n;
			this.dropped += n;
			fs.rmSync(this.file(seg), { force: true });
		}
	}

	private closeWrite(): void {
		if (this.writeFd !== undefined) {
			fs.closeSync(this.writeFd);
			this.writeFd = undefined;
			this.writeSeg = undefined;
		}
	}

	/** The lines of the oldest segment, which stays on disk until done(segment); undefined when there is none. */
	take(): { seg: number; lines: string[] } | undefined {
		if (this.segs.length === 0) {
			return undefined;
		}
		const seg = this.segs.shift()!;
		if (seg === this.writeSeg) {
			this.closeWrite();
		}
		const text = readOrEmpty(this.file(seg));
		const lines = text.split("\n").filter(l => l.length > 0);
		this.bytes -= Buffer.byteLength(text);
		this.lines -= countLines(text);
		return { seg, lines };
	}

	/** Every line of a taken segment was acknowledged: delete it. */
	done(seg: number): void {
		fs.rmSync(this.file(seg), { force: true });
	}

	/** Writes lines (the queue's memory part when it stops) as a segment before every other. */
	putBack(lines: string[]): void {
		if (lines.length === 0) {
			return;
		}
		const seg = (this.segs.length > 0 ? this.segs[0] : 0) - 1;
		const text = lines.join("\n") + "\n";
		fs.writeFileSync(this.file(seg), text);
		this.segs.unshift(seg);
		this.bytes += Buffer.byteLength(text);
		this.lines += lines.length;
	}

	close(): void {
		this.closeWrite();
	}
}

function countLines(text: string): number {
	let n = 0;
	for (let i = text.indexOf("\n"); i >= 0; i = text.indexOf("\n", i + 1)) {
		n += 1;
	}
	return n;
}

function readOrEmpty(file: string): string {
	try {
		return fs.readFileSync(file, "utf8");
	} catch {
		return "";
	}
}
