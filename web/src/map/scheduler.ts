// A global request scheduler: at most `maxInFlight` requests at once, lowest
// priority value first, and requests nobody wants any more are dropped (queued) or aborted
// (in flight). The engine calls want() for everything a frame needs, retain() with the full
// wanted set, then pump().

export type JobRun = (signal: AbortSignal) => Promise<void>;

interface Job {
  key: string;
  prio: number;
  run: JobRun;
  seq: number;
  ctrl?: AbortController;
  started?: number;
}

export interface SchedulerOptions {
  maxInFlight: number;
  /** A job started (left the queue). */
  onStart?(key: string, prio: number): void;
  /** An in-flight job was aborted because it is no longer wanted. */
  onAbort?(key: string): void;
  now?(): number;
}

/** Default concurrency: 16 on HTTP/2 or newer, 6 on HTTP/1.1 (browser per-host limit). */
export function defaultMaxInFlight(protocol?: string): number {
  return protocol === "http/1.1" || protocol === "http/1.0" ? 6 : 16;
}

export class Scheduler {
  private readonly queued = new Map<string, Job>();
  private readonly running = new Map<string, Job>();
  private seq = 0;
  maxInFlight: number;
  private readonly opts: SchedulerOptions;

  constructor(opts: SchedulerOptions) {
    this.opts = opts;
    this.maxInFlight = Math.max(1, opts.maxInFlight);
  }

  get inFlight(): number {
    return this.running.size;
  }

  get pending(): number {
    return this.queued.size;
  }

  isRunning(key: string): boolean {
    return this.running.has(key);
  }

  has(key: string): boolean {
    return this.queued.has(key) || this.running.has(key);
  }

  /** Milliseconds a running job has been in flight, or -1. */
  age(key: string): number {
    const j = this.running.get(key);
    return j?.started === undefined ? -1 : this.now() - j.started;
  }

  /**
   * Adds a job, or updates the priority of a queued one. A running job keeps running.
   * Returns false when the key was already known.
   */
  want(key: string, prio: number, run: JobRun): boolean {
    const r = this.running.get(key);
    if (r) {
      r.prio = prio;
      return false;
    }
    const q = this.queued.get(key);
    if (q) {
      q.prio = prio;
      return false;
    }
    this.queued.set(key, { key, prio, run, seq: this.seq++ });
    return true;
  }

  /** Drops queued jobs and aborts running jobs whose key is not in `keys`. */
  retain(keys: ReadonlySet<string>): void {
    for (const k of this.queued.keys()) {
      if (!keys.has(k)) {
        this.queued.delete(k);
      }
    }
    for (const [k, j] of this.running) {
      if (!keys.has(k)) {
        this.abortJob(j);
      }
    }
  }

  /** Cancels one job (queued or running). */
  cancel(key: string): void {
    this.queued.delete(key);
    const j = this.running.get(key);
    if (j) {
      this.abortJob(j);
    }
  }

  /** Cancels everything. */
  clear(): void {
    this.queued.clear();
    for (const j of [...this.running.values()]) {
      this.abortJob(j);
    }
  }

  /** Starts queued jobs, best priority first, until the in-flight limit is reached. */
  pump(): void {
    while (this.running.size < this.maxInFlight && this.queued.size > 0) {
      let best: Job | undefined;
      for (const j of this.queued.values()) {
        if (!best || j.prio < best.prio || (j.prio === best.prio && j.seq < best.seq)) {
          best = j;
        }
      }
      if (!best) {
        break;
      }
      this.start(best);
    }
  }

  private start(j: Job): void {
    this.queued.delete(j.key);
    j.ctrl = new AbortController();
    j.started = this.now();
    this.running.set(j.key, j);
    this.opts.onStart?.(j.key, j.prio);
    const signal = j.ctrl.signal;
    let p: Promise<void>;
    try {
      p = j.run(signal);
    } catch (err) {
      p = Promise.reject(err);
    }
    p.catch(() => {
      // The job reports its own errors; the scheduler only frees the slot.
    }).finally(() => {
      if (this.running.get(j.key) === j) {
        this.running.delete(j.key);
      }
      this.pump();
    });
  }

  private abortJob(j: Job): void {
    this.running.delete(j.key);
    j.ctrl?.abort();
    this.opts.onAbort?.(j.key);
  }

  private now(): number {
    if (this.opts.now) {
      return this.opts.now();
    }
    return typeof performance !== "undefined" ? performance.now() : Date.now();
  }
}
