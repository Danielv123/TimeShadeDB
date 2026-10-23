import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { DiskTail } from "../spool.js";

test("spool: lines appended while the last segment is taken go to a new segment, not into the taken one", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ts-spool-"));
	const tail = new DiskTail(dir);
	tail.append(["a", "b"]);
	const first = tail.take()!;
	assert.deepEqual(first.lines, ["a", "b"]);
	tail.append(["c"]);
	tail.done(first.seg);
	const second = tail.take()!;
	assert.notEqual(second.seg, first.seg);
	assert.deepEqual(second.lines, ["c"]);
	tail.close();
	fs.rmSync(dir, { recursive: true, force: true });
});

test("spool: a put-back segment does not take the number of a taken one", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "ts-spool-"));
	const tail = new DiskTail(dir);
	tail.putBack(["a"]);
	const taken = tail.take()!;
	tail.putBack(["b"]);
	tail.done(taken.seg);
	tail.close();
	const again = new DiskTail(dir);
	assert.deepEqual(again.take()?.lines, ["b"]);
	again.close();
	fs.rmSync(dir, { recursive: true, force: true });
});
