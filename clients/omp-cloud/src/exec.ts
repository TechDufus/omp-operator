import { spawn } from "node:child_process";

export interface RunOptions {
	cwd?: string;
	/** Merged over process.env; an undefined value removes the variable. */
	env?: Record<string, string | undefined>;
	timeoutMs?: number;
	input?: string | Uint8Array;
}

export interface RunResult {
	code: number;
	stdout: string;
	stderr: string;
}

/**
 * Runs a child process and collects its output. Never rejects: a spawn failure
 * is code 127 and a timeout is code 124, both with the reason in stderr.
 * Used instead of `pi.exec`, which cannot set the child's environment.
 */
export function run(cmd: string, args: string[], opts: RunOptions = {}): Promise<RunResult> {
	const { promise, resolve } = Promise.withResolvers<RunResult>();
	const env: Record<string, string | undefined> = { ...process.env, ...opts.env };
	for (const k of Object.keys(env)) if (env[k] === undefined) delete env[k];
	const child = spawn(cmd, args, {
		cwd: opts.cwd,
		env: env as NodeJS.ProcessEnv,
		stdio: ["pipe", "pipe", "pipe"],
	});
	const out: Buffer[] = [];
	const err: Buffer[] = [];
	let settled = false;
	let timedOut = false;
	const timer = opts.timeoutMs
		? setTimeout(() => {
				timedOut = true;
				child.kill("SIGKILL");
			}, opts.timeoutMs)
		: undefined;
	const finish = (code: number, extra = ""): void => {
		if (settled) return;
		settled = true;
		clearTimeout(timer);
		resolve({
			code,
			stdout: Buffer.concat(out).toString("utf8"),
			stderr: Buffer.concat(err).toString("utf8") + extra,
		});
	};
	child.stdout.on("data", (d: Buffer) => out.push(d));
	child.stderr.on("data", (d: Buffer) => err.push(d));
	child.on("error", e => finish(127, `${cmd}: ${e.message}`));
	child.on("close", code => {
		if (timedOut) finish(124, `${cmd}: timed out after ${opts.timeoutMs}ms`);
		else finish(code ?? 1);
	});
	child.stdin.on("error", () => {});
	child.stdin.end(opts.input);
	return promise;
}
