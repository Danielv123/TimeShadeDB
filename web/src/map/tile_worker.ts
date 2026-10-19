// Tile fetch worker (loader.ts WorkerLoader). Fetches a body, lets the browser decompress it
// (Content-Encoding), optionally expands a TSV3 tile to RGBA for the Canvas2D renderer, and
// transfers the buffers back without copying.

import type { WorkerRequest, WorkerResponse } from "./loader.ts";
import { expandBody } from "./loader.ts";

interface WorkerScope {
  postMessage(message: WorkerResponse, transfer?: Transferable[]): void;
  onmessage: ((e: MessageEvent<WorkerRequest>) => void) | null;
}

const scope = self as unknown as WorkerScope;
const inflight = new Map<number, AbortController>();
let lut: Uint32Array | null = null;

scope.onmessage = (e: MessageEvent<WorkerRequest>) => {
  const msg = e.data;
  switch (msg.type) {
    case "lut":
      lut = msg.lut;
      return;
    case "abort":
      inflight.get(msg.id)?.abort();
      inflight.delete(msg.id);
      return;
    case "fetch":
      void run(msg);
      return;
  }
};

async function run(msg: Extract<WorkerRequest, { type: "fetch" }>): Promise<void> {
  const ctrl = new AbortController();
  inflight.set(msg.id, ctrl);
  const t0 = performance.now();
  try {
    const res = await fetch(msg.url, { headers: msg.headers, signal: ctrl.signal, cache: msg.cache });
    if (!res.ok) {
      let code: string | undefined;
      let message = `${res.status} ${res.statusText} for ${msg.url}`;
      try {
        const body = (await res.json()) as { error?: string; code?: string };
        code = body.code;
        message = `${body.error ?? res.statusText} (${res.status})`;
      } catch {
        // not JSON
      }
      scope.postMessage({ id: msg.id, ok: false, status: res.status, code, message });
      return;
    }
    const buffer = await res.arrayBuffer();
    const ms = performance.now() - t0;
    const rgba = msg.expand ? expandBody(buffer, lut) : undefined;
    const transfer: Transferable[] = [buffer];
    if (rgba) {
      transfer.push(rgba.buffer as ArrayBuffer);
    }
    scope.postMessage({ id: msg.id, ok: true, buffer, ms, rgba: rgba?.buffer as ArrayBuffer | undefined }, transfer);
  } catch (err) {
    const e = err as Error;
    scope.postMessage({ id: msg.id, ok: false, name: e?.name, message: e?.message ?? String(err) });
  } finally {
    inflight.delete(msg.id);
  }
}
