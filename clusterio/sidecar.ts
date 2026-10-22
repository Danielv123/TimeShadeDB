// The map server as a child process of the controller (PLAN.md §4.10): `timeshadedb serve --listen 127.0.0.1:0`,
// whose first stdout line names the port it got. It is restarted with backoff when it exits, and stops when the
// controller does: stop() closes its stdin (--exit-on-stdin-close), and so does the controller dying.
import { spawn as nodeSpawn, type ChildProcess, type SpawnOptions } from "node:child_process";
import { EventEmitter } from "node:events";
import * as readline from "node:readline";
import type * as lib from "@clusterio/lib";
import { lifecycle } from "./runtime.js";

export type SidecarState = "stopped" | "starting" | "running" | "backoff";

export type SidecarOptions = {
	command: string;
	args: string[];
	logger: lib.Logger;
	env?: NodeJS.ProcessEnv;
	cwd?: string;
	spawn?: (command: string, args: string[], options: SpawnOptions) => ChildProcess;
	/** First restart delay, doubled per crash in a row up to maxBackoffMs. */
	minBackoffMs?: number;
	maxBackoffMs?: number;
	/** A run at least this long resets the backoff. */
	stableMs?: number;
	/** Time to print "listening on" before the process counts as failed and is killed. */
	readyTimeoutMs?: number;
	/** Time to exit after stdin closed before it is killed. */
	stopTimeoutMs?: number;
	/** Output lines logged per 10 s; the rest are counted. */
	logLines?: number;
};

const LISTENING = /^listening on (\S+)\s*$/;

/**
 * Events: "ready" (url) each time a process is listening, "exit" (code, signal) each time one exits, "state" (state).
 */
export class Sidecar extends EventEmitter {
	private opts: Required<Omit<SidecarOptions, "env" | "cwd">> & Pick<SidecarOptions, "env" | "cwd">;
	private child: ChildProcess | undefined;
	private exited: Promise<void> = Promise.resolve();
	private timer: NodeJS.Timeout | undefined;
	private readyTimer: NodeJS.Timeout | undefined;
	private wanted = false;
	private failures = 0;
	private startedAt = 0;
	private logged = { at: 0, n: 0, skipped: 0 };
	private _state: SidecarState = "stopped";
	private _url: string | undefined;
	/** Processes started, the first included. */
	starts = 0;
	/** Exits that were not asked for. */
	crashes = 0;

	constructor(opts: SidecarOptions) {
		super();
		this.opts = {
			spawn: nodeSpawn,
			minBackoffMs: 1000,
			maxBackoffMs: 60_000,
			stableMs: 60_000,
			readyTimeoutMs: 30_000,
			stopTimeoutMs: 10_000,
			logLines: 50,
			...opts,
		};
	}

	get state(): SidecarState {
		return this._state;
	}

	/** http://127.0.0.1:<port> while a process is listening. */
	get url(): string | undefined {
		return this._state === "running" ? this._url : undefined;
	}

	/** Change the command line; takes effect at the next start (restart() applies it now). */
	configure(command: string, args: string[]): void {
		this.opts.command = command;
		this.opts.args = args;
	}

	start(): void {
		this.wanted = true;
		if (this._state === "stopped") {
			this.failures = 0;
			this.spawnOnce();
		}
	}

	/** Resolves with the URL once a process is listening (now, if one is). */
	ready(timeoutMs = 30_000): Promise<string> {
		if (this.url) {
			return Promise.resolve(this.url);
		}
		return new Promise((resolve, reject) => {
			const done = (url: string) => {
				clearTimeout(t);
				resolve(url);
			};
			const t = setTimeout(() => {
				this.off("ready", done);
				reject(new Error("timeshade sidecar not ready in time"));
			}, timeoutMs);
			this.once("ready", done);
		});
	}

	/** Stops the process (stdin closed, then killed after stopTimeoutMs) and does not restart it. */
	async stop(): Promise<void> {
		this.wanted = false;
		clearTimeout(this.timer);
		this.timer = undefined;
		const child = this.child;
		if (child) {
			child.stdin?.end();
			const kill = setTimeout(() => child.kill("SIGKILL"), this.opts.stopTimeoutMs);
			try {
				await this.exited;
			} finally {
				clearTimeout(kill);
			}
		}
		this.setState("stopped");
	}

	async restart(): Promise<void> {
		await this.stop();
		this.start();
	}

	private setState(state: SidecarState) {
		if (state !== this._state) {
			this._state = state;
			this.emit("state", state);
		}
	}

	/** Logs a line of the process's output, at most logLines per 10 s. */
	private output(line: string, stream: "stdout" | "stderr") {
		const now = Date.now();
		if (now - this.logged.at >= 10_000) {
			if (this.logged.skipped > 0) {
				this.opts.logger.warn(`timeshade sidecar: ${this.logged.skipped} output lines not logged`);
			}
			this.logged = { at: now, n: 0, skipped: 0 };
		}
		if (this.logged.n >= this.opts.logLines) {
			this.logged.skipped += 1;
			return;
		}
		this.logged.n += 1;
		this.opts.logger.log(stream === "stderr" ? "info" : "verbose", `timeshade sidecar: ${line}`);
	}

	private spawnOnce() {
		this.setState("starting");
		this._url = undefined;
		this.starts += 1;
		this.startedAt = Date.now();
		let child: ChildProcess;
		try {
			child = this.opts.spawn(this.opts.command, this.opts.args, {
				stdio: ["pipe", "pipe", "pipe"],
				windowsHide: true,
				env: this.opts.env,
				cwd: this.opts.cwd,
			});
		} catch (err: any) {
			this.failed(`spawn failed: ${err.message}`);
			return;
		}
		this.child = child;
		let resolveExit!: () => void;
		this.exited = new Promise(resolve => {
			resolveExit = resolve;
		});
		let first = true;
		let errored: Error | undefined;
		child.stdin?.on("error", () => { /* the process went away; exit follows */ });
		if (child.stdout) {
			readline.createInterface({ input: child.stdout, crlfDelay: Infinity }).on("line", line => {
				if (first) {
					first = false;
					const m = LISTENING.exec(line);
					if (m && this.child === child) {
						clearTimeout(this.readyTimer);
						this._url = `http://${m[1]}`;
						this.setState("running");
						lifecycle(this.opts.logger, "timeshade.sidecar", "ready", { url: this._url, pid: child.pid }, "verbose");
						this.emit("ready", this._url);
						return;
					}
				}
				this.output(line, "stdout");
			});
		}
		if (child.stderr) {
			readline.createInterface({ input: child.stderr, crlfDelay: Infinity }).on("line", line => this.output(line, "stderr"));
		}
		this.readyTimer = setTimeout(() => {
			if (this.child === child && this._state === "starting") {
				this.opts.logger.warn("timeshade sidecar did not print its address in time; killing it");
				child.kill("SIGKILL");
			}
		}, this.opts.readyTimeoutMs);
		child.on("error", err => {
			// ENOENT and the like: no exit event follows when the process never started.
			errored = err;
			if (child.pid === undefined) {
				this.exitedChild(child, null, null, err);
				resolveExit();
			}
		});
		child.on("exit", (code, signal) => {
			this.exitedChild(child, code, signal, errored);
			resolveExit();
		});
	}

	private exitedChild(child: ChildProcess, code: number | null, signal: NodeJS.Signals | null, err?: Error) {
		if (this.child !== child) {
			return;
		}
		this.child = undefined;
		clearTimeout(this.readyTimer);
		this._url = undefined;
		this.emit("exit", code, signal);
		if (!this.wanted) {
			this.setState("stopped");
			return;
		}
		this.failed(err ? err.message : `exited with code ${code} signal ${signal}`);
	}

	/** An exit nobody asked for: restart after the backoff. */
	private failed(reason: string) {
		this.crashes += 1;
		if (Date.now() - this.startedAt >= this.opts.stableMs) {
			this.failures = 0;
		}
		const delay = Math.min(this.opts.maxBackoffMs, this.opts.minBackoffMs * (2 ** this.failures));
		this.failures += 1;
		lifecycle(this.opts.logger, "timeshade.sidecar", "exited", { reason, restartInMs: delay, crashes: this.crashes }, "warn");
		this.setState("backoff");
		clearTimeout(this.timer);
		this.timer = setTimeout(() => {
			this.timer = undefined;
			if (this.wanted) {
				this.spawnOnce();
			}
		}, delay);
	}
}
