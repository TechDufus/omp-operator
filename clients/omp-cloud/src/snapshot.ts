import { readFile, stat } from "node:fs/promises";
import { basename, isAbsolute, join, posix } from "node:path";
import { run, type RunResult } from "./exec";

export const MAX_SECRET_FILE_BYTES = 256 * 1024;
const DEFAULT_AUTHOR = { name: "omp-cloud", email: "omp-cloud@localhost" };

export interface SnapshotOptions {
	cwd: string;
	/** An existing scratch directory; the index and the bundle are written here. */
	tmpDir: string;
	/** The cloud agent's `cloud.git[].host` entries. */
	gitHosts: string[];
	forceBundle: boolean;
	/** min(maxUploadMB, agent.maxUploadBytes), in bytes. */
	maxBytes: number;
	secretFiles: string[];
	secretEnv: string[];
	env: Record<string, string | undefined>;
	home: string;
}

export interface Snapshot {
	root: string;
	name: string;
	mode: "delta" | "full";
	remote: string;
	/** Raw configured remote URL (`git config --get remote.<remote>.url`); empty only when no remote is resolved. */
	remoteURL: string;
	baseRef: string;
	baseCommit: string;
	handoffCommit: string;
	localBranch: string;
	/** Absent when the delta is empty (handoff equals base). */
	bundlePath?: string;
	bundleBytes: number;
	secretFiles: { path: string; content: string }[];
	secretEnv: Record<string, string>;
	paths: { home: string; workspace: string };
	author: { name: string; email: string };
	/** Commits the handoff carries beyond the remote's base (delta mode), or the whole history (full mode). */
	commits: number;
	/** What the work-in-progress commit adds on top of HEAD. */
	wip: { files: number; insertions: number; deletions: number };
	/** The first untracked files that ship (git does not ignore them); `untrackedTotal` counts all of them. */
	untracked: string[];
	untrackedTotal: number;
	/** Credential-named files left out of the snapshot: they stay on this machine. */
	withheld: string[];
}

export interface RemoteHost {
	scheme: "ssh" | "http" | "https";
	host: string;
}

/** Parses scp-like, ssh://, http:// and https:// remotes; anything else is unsupported. */
export function parseRemote(raw: string): RemoteHost | undefined {
	const url = raw.trim();
	if (/^[a-z][a-z0-9+.-]*:\/\//i.test(url)) {
		let u: URL;
		try {
			u = new URL(url);
		} catch {
			return undefined;
		}
		const host = u.hostname.toLowerCase();
		if (!host) return undefined;
		switch (u.protocol) {
			case "http:":
				return { scheme: "http", host };
			case "https:":
				return { scheme: "https", host };
			case "ssh:":
			case "git+ssh:":
			case "ssh+git:":
				return { scheme: "ssh", host };
			default:
				return undefined;
		}
	}
	const scp = /^(?:[^@/\s]+@)?([^:/\s]+):(?!\/\/)(.+)$/.exec(url);
	if (scp?.[1] && !/^[A-Za-z]$/.test(scp[1])) return { scheme: "ssh", host: scp[1].toLowerCase() };
	return undefined;
}

export interface GitOptions {
	env?: Record<string, string | undefined>;
	input?: string | Uint8Array;
	timeoutMs?: number;
}

/** Runs git in `cwd`, returning the exit code and output. */
export function gitRaw(cwd: string, args: string[], opts: GitOptions = {}): Promise<RunResult> {
	return run("git", args, {
		cwd,
		env: { GIT_TERMINAL_PROMPT: "0", ...opts.env },
		input: opts.input,
		timeoutMs: opts.timeoutMs,
	});
}

/** Runs git in `cwd`; any non-zero exit throws with git's own message. Returns trimmed stdout. */
export async function git(cwd: string, args: string[], opts: GitOptions = {}): Promise<string> {
	const r = await gitRaw(cwd, args, opts);
	if (r.code !== 0) throw new Error(`git ${args[0] ?? ""} failed: ${r.stderr.trim() || `exit ${r.code}`}`);
	return r.stdout.trim();
}

async function gitOpt(cwd: string, args: string[]): Promise<string | undefined> {
	const r = await gitRaw(cwd, args);
	return r.code === 0 ? r.stdout.trim() : undefined;
}

export function mb(bytes: number): string {
	return String(Number((bytes / (1024 * 1024)).toFixed(1)));
}

interface Upstream {
	remote: string;
	baseRef: string;
}

async function findUpstream(root: string, branch: string): Promise<Upstream | undefined> {
	if (branch) {
		const line = await gitOpt(root, [
			"for-each-ref",
			"--format=%(upstream:remotename)%09%(upstream:remoteref)",
			`refs/heads/${branch}`,
		]);
		const [remote, ref] = (line ?? "").split("\t");
		if (remote && remote !== "." && ref?.startsWith("refs/heads/")) {
			return { remote, baseRef: ref.slice("refs/heads/".length) };
		}
	}
	const sym = await gitOpt(root, ["symbolic-ref", "--short", "refs/remotes/origin/HEAD"]);
	if (sym?.startsWith("origin/")) return { remote: "origin", baseRef: sym.slice("origin/".length) };
	return undefined;
}

async function readSecretFiles(root: string, paths: string[]): Promise<{ path: string; content: string }[]> {
	const out: { path: string; content: string }[] = [];
	for (const p of paths) {
		const unsafe =
			isAbsolute(p) || posix.normalize(p) !== p || p === ".." || p.startsWith("../") || p === ".git" || p.startsWith(".git/");
		if (unsafe) throw new Error(`${p} is not a safe workspace-relative path`);
		const abs = join(root, p);
		let st: Awaited<ReturnType<typeof stat>>;
		try {
			st = await stat(abs);
		} catch {
			throw new Error(`${p} does not exist`);
		}
		if (!st.isFile()) throw new Error(`${p} is not a regular file`);
		if (st.size > MAX_SECRET_FILE_BYTES) throw new Error(`${p} is over ${MAX_SECRET_FILE_BYTES / 1024} KiB`);
		const ignored = await gitRaw(root, ["check-ignore", "-q", "--", p]);
		if (ignored.code !== 0) throw new Error(`${p} is not gitignored; refusing to ship it`);
		out.push({ path: p, content: (await readFile(abs)).toString("base64") });
	}
	return out;
}

// Basename patterns of files that hold credentials; see `isCredentialPath`.
const CREDENTIAL_GLOBS = [
	/^\.env$/,
	/^\.env\..+$/,
	/\.tfvars$/,
	/\.tfvars\.json$/,
	/\.tfstate$/,
	/\.tfstate\..+$/,
	/\.(pem|key|p12|pfx|jks|keystore)$/,
	/^id_(rsa|dsa|ecdsa|ed25519)$/,
	/^\.(netrc|npmrc|pypirc)$/,
	/^kubeconfig$/,
	/\.kubeconfig$/,
	/^talosconfig$/,
];
const TEMPLATE_SUFFIX = /\.(example|sample|template)$/;
/** How many untracked files a handoff card names. */
export const UNTRACKED_LISTED = 10;

/** Whether a path's basename looks like a credential file (`.env`, `*.pem`, `id_rsa`, `kubeconfig`, ...). */
export function isCredentialPath(path: string): boolean {
	const base = (path.split("/").pop() ?? path).toLowerCase();
	if (/^\.env\./.test(base) && TEMPLATE_SUFFIX.test(base)) return false;
	return CREDENTIAL_GLOBS.some(re => re.test(base));
}

/**
 * Removes credential-named changes from the private index: tracked files keep their HEAD
 * content, untracked ones are dropped. Returns the paths left out, which stay on this machine.
 */
async function withholdCredentials(root: string, indexEnv: Record<string, string>, allowed: ReadonlySet<string>): Promise<string[]> {
	const changed = await git(root, ["diff", "--cached", "--name-status", "--no-renames", "-z", "HEAD"], { env: indexEnv });
	const fields = changed.split("\0");
	const restore: string[] = [];
	const drop: string[] = [];
	for (let i = 0; i + 1 < fields.length; i += 2) {
		const status = fields[i] ?? "";
		const path = fields[i + 1] ?? "";
		if (!isCredentialPath(path) || allowed.has(path)) continue;
		(status.startsWith("A") ? drop : restore).push(path);
	}
	if (restore.length > 0) {
		const entries: string[] = [];
		for (const p of restore) {
			const r = await gitRaw(root, ["ls-tree", "-z", "HEAD", "--", p]);
			const entry = r.stdout.replace(/\0$/, "");
			if (entry) entries.push(entry.replace(/^(\d+) \w+ (\w+)\t/, "$1 $2\t"));
		}
		await git(root, ["update-index", "-z", "--index-info"], {
			env: indexEnv,
			input: `${entries.join("\0")}\0`,
		});
	}
	if (drop.length > 0) {
		await git(root, ["update-index", "--force-remove", "-z", "--stdin"], { env: indexEnv, input: `${drop.join("\0")}\0` });
	}
	return [...restore, ...drop].sort();
}

/** Untracked files git does not ignore, sorted. */
async function listUntracked(root: string): Promise<string[]> {
	const out = await git(root, ["ls-files", "--others", "--exclude-standard", "-z"]);
	return out === "" ? [] : out.split("\0").filter(p => p !== "").sort();
}

/** Files and lines the WIP commit changes relative to HEAD. */
async function wipStat(root: string, indexEnv: Record<string, string>): Promise<{ files: number; insertions: number; deletions: number }> {
	const names = await git(root, ["diff", "--cached", "--name-only", "-z", "HEAD"], { env: indexEnv });
	const short = await git(root, ["diff", "--cached", "--shortstat", "HEAD"], { env: indexEnv });
	return {
		files: names === "" ? 0 : names.split("\0").filter(p => p !== "").length,
		insertions: Number(/(\d+) insertion/.exec(short)?.[1] ?? 0),
		deletions: Number(/(\d+) deletion/.exec(short)?.[1] ?? 0),
	};
}

/**
 * Captures the working tree for a handoff: the base commit on the remote, a
 * WIP commit holding every uncommitted change (tracked or not, never ignored
 * files), and a git bundle of what the remote does not have.
 */
export async function takeSnapshot(opts: SnapshotOptions): Promise<Snapshot> {
	const top = await gitRaw(opts.cwd, ["rev-parse", "--show-toplevel"]);
	if (top.code !== 0) throw new Error("not a git repository");
	const root = top.stdout.trim();
	const headRes = await gitRaw(root, ["rev-parse", "--verify", "HEAD^{commit}"]);
	if (headRes.code !== 0) throw new Error("the repository has no commits");
	const head = headRes.stdout.trim();

	const localBranch = (await gitOpt(root, ["branch", "--show-current"])) ?? "";
	const upstream = await findUpstream(root, localBranch);
	const remoteURL = upstream ? ((await gitOpt(root, ["config", "--get", `remote.${upstream.remote}.url`])) ?? "") : "";

	// Fail on unshippable secrets before spending time on the bundle.
	const secretFiles = await readSecretFiles(root, opts.secretFiles);
	const secretEnv: Record<string, string> = {};
	for (const name of opts.secretEnv) {
		const v = opts.env[name];
		if (v === undefined) throw new Error(`${name} is not set in this omp's environment`);
		secretEnv[name] = v;
	}

	let baseCommit = "";
	if (upstream && remoteURL && !opts.forceBundle) {
		const parsed = parseRemote(remoteURL);
		const hosts = opts.gitHosts.map(h => h.toLowerCase());
		if (parsed && (parsed.scheme !== "ssh" || hosts.includes(parsed.host))) {
			baseCommit = (await gitOpt(root, ["merge-base", "HEAD", `refs/remotes/${upstream.remote}/${upstream.baseRef}`])) ?? "";
		}
	}
	const delta = baseCommit !== "";
	const commits = Number(
		await git(root, ["rev-list", "--count", delta ? `${baseCommit}..${head}` : head]),
	);

	const name = (await gitOpt(root, ["config", "user.name"])) || DEFAULT_AUTHOR.name;
	const email = (await gitOpt(root, ["config", "user.email"])) || DEFAULT_AUTHOR.email;

	// WIP commit via a private index so the user's index is never touched.
	const indexEnv = { GIT_INDEX_FILE: join(opts.tmpDir, "index") };
	await git(root, ["read-tree", "HEAD"], { env: indexEnv });
	await git(root, ["add", "-A", "--", "."], { env: indexEnv });
	const withheld = await withholdCredentials(root, indexEnv, new Set(opts.secretFiles));
	const untracked = (await listUntracked(root)).filter(p => !withheld.includes(p));
	const tree = await git(root, ["write-tree"], { env: indexEnv });
	const wip = await wipStat(root, indexEnv);
	let handoff = head;
	if (tree !== (await git(root, ["rev-parse", "HEAD^{tree}"]))) {
		handoff = await git(
			root,
			["-c", "commit.gpgsign=false", "commit-tree", tree, "-p", "HEAD", "-m", "omp-cloud: local changes at handoff"],
			{
				env: {
					GIT_AUTHOR_NAME: name,
					GIT_AUTHOR_EMAIL: email,
					GIT_COMMITTER_NAME: name,
					GIT_COMMITTER_EMAIL: email,
				},
			},
		);
	}

	let bundlePath: string | undefined;
	let bundleBytes = 0;
	if (!delta || handoff !== baseCommit) {
		bundlePath = join(opts.tmpDir, "source.bundle");
		await git(root, ["update-ref", "refs/omp-cloud/handoff", handoff]);
		try {
			const args = ["bundle", "create", bundlePath, "refs/omp-cloud/handoff"];
			if (delta) args.push(`^${baseCommit}`);
			await git(root, args);
		} finally {
			await gitRaw(root, ["update-ref", "-d", "refs/omp-cloud/handoff"]);
		}
		bundleBytes = (await stat(bundlePath)).size;
		if (bundleBytes > opts.maxBytes) {
			throw new Error(`repository bundle is ${mb(bundleBytes)} MB, over the ${mb(opts.maxBytes)} MB upload limit`);
		}
	}

	return {
		root,
		name: basename(root),
		mode: delta ? "delta" : "full",
		remote: upstream?.remote ?? "",
		remoteURL,
		baseRef: upstream?.baseRef ?? "",
		baseCommit,
		handoffCommit: handoff,
		localBranch,
		bundlePath,
		bundleBytes,
		secretFiles,
		secretEnv,
		paths: { home: opts.home, workspace: root },
		author: { name, email },
		commits,
		wip,
		untracked: untracked.slice(0, UNTRACKED_LISTED),
		untrackedTotal: untracked.length,
		withheld,
	};
}
