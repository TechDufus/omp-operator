import { cp, mkdir, readdir, stat } from "node:fs/promises";
import { basename, join } from "node:path";
import { run } from "./exec";
import { git, gitRaw } from "./snapshot";
import { UserError } from "./types";

/** The branch a thread's result is pushed to and pulled into (`cloudspec.Branch`). */
export function threadBranch(thread: string): string {
	return `omp-cloud/${thread}`;
}

export interface PullOptions {
	root: string;
	thread: string;
	bundlePath: string;
	/** The thread's base commit, named in the "lacks the base commit" error. */
	baseCommit: string;
	remote: string;
}

export interface PullOutcome {
	branch: string;
	head: string;
}

/**
 * Fetches the result bundle's branch and points `omp-cloud/<thread>` at it:
 * fast-forward when that branch is checked out, otherwise `branch -f`.
 */
export async function pullBundle(opts: PullOptions): Promise<PullOutcome> {
	const branch = threadBranch(opts.thread);
	const fetched = await gitRaw(opts.root, ["fetch", "--no-tags", opts.bundlePath, `refs/heads/${branch}`]);
	if (fetched.code !== 0) {
		if (/prerequisite|missing necessary objects|bad object|unable to read tree/i.test(fetched.stderr)) {
			const base = opts.baseCommit ? ` ${opts.baseCommit}` : "";
			throw new UserError(`your repository lacks the thread's base commit${base}; git fetch ${opts.remote || "origin"}, then retry`);
		}
		throw new Error(`git fetch failed: ${fetched.stderr.trim() || `exit ${fetched.code}`}`);
	}
	const head = await git(opts.root, ["rev-parse", "FETCH_HEAD^{commit}"]);
	const current = await git(opts.root, ["branch", "--show-current"]);
	if (current === branch) await git(opts.root, ["merge", "--ff-only", head]);
	else await git(opts.root, ["branch", "-f", branch, head]);
	return { branch, head };
}

export async function isClean(root: string): Promise<boolean> {
	return (await git(root, ["status", "--porcelain"])) === "";
}

async function jsonlFiles(dir: string, topOnly: boolean): Promise<{ path: string; mtime: number }[]> {
	const out: { path: string; mtime: number }[] = [];
	for (const entry of await readdir(dir, { withFileTypes: true })) {
		const p = join(dir, entry.name);
		if (entry.isDirectory()) {
			if (!topOnly) out.push(...(await jsonlFiles(p, false)));
		} else if (entry.name.endsWith(".jsonl")) {
			out.push({ path: p, mtime: (await stat(p)).mtimeMs });
		}
	}
	return out;
}

/** Extracts the conversation tarball into `into` and returns the newest session `.jsonl` in it. */
export async function extractConversation(tarball: string, into: string): Promise<string> {
	await mkdir(into, { recursive: true });
	// -xf (not -xzf) so a plain or gzip-compressed tar both work.
	const r = await run("tar", ["-xf", tarball, "-C", into]);
	if (r.code !== 0) throw new Error(`cannot unpack the conversation: ${r.stderr.trim().slice(0, 300) || `tar exit ${r.code}`}`);
	let files = await jsonlFiles(into, true);
	if (files.length === 0) files = await jsonlFiles(into, false);
	const newest = files.sort((a, b) => b.mtime - a.mtime)[0];
	if (!newest) throw new Error("the cloud conversation holds no session file");
	return newest.path;
}

/** Copies the session file (and its artifacts directory, if any) into the local session directory. */
export async function installConversation(jsonl: string, sessionDir: string): Promise<string> {
	await mkdir(sessionDir, { recursive: true });
	const dest = join(sessionDir, basename(jsonl));
	await cp(jsonl, dest);
	const artifacts = jsonl.replace(/\.jsonl$/, "");
	const isDir = await stat(artifacts).then(s => s.isDirectory(), () => false);
	if (isDir) await cp(artifacts, dest.replace(/\.jsonl$/, ""), { recursive: true });
	return dest;
}
