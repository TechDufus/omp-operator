import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";
import { run } from "./exec";
import { UserError, type Whoami, type WhoamiAgent } from "./types";

export interface ShipConfig {
	settings: boolean;
	extensions: boolean;
	memory: boolean;
}

/** `<agentDir>/cloud.json`. */
export interface UserConfig {
	url?: string;
	token?: string;
	tokenCommand?: string[];
	namespace?: string;
	agent?: string;
	maxUploadMB: number;
	forceBundle: boolean;
	ship: ShipConfig;
	excludeExtensions: string[];
}

/** `<repo root>/.omp/cloud.json`. */
export interface RepoConfig {
	agent?: string;
	namespace?: string;
	secretFiles: string[];
	secretEnv: string[];
}

export interface CloudConfig extends UserConfig {
	repo: RepoConfig;
	/** Where each setting came from, for `/cloud status`. */
	sources: string[];
}

export interface Connection {
	url: string;
	token: string;
}

export const NOT_CONFIGURED =
	"☁ omp-cloud is not configured: set OMP_CLOUD_URL and OMP_CLOUD_TOKEN or create ~/.omp/agent/cloud.json";

export type Env = Record<string, string | undefined>;

/** `PI_CODING_AGENT_DIR` or else `~/.omp/agent`. */
export function agentDir(env: Env = process.env, home: string = homedir()): string {
	return env.PI_CODING_AGENT_DIR || join(home, ".omp", "agent");
}

async function readJson(path: string): Promise<Record<string, unknown> | undefined> {
	let text: string;
	try {
		text = await readFile(path, "utf8");
	} catch (e) {
		if ((e as NodeJS.ErrnoException).code === "ENOENT") return undefined;
		throw new UserError(`☁ cannot read ${path}: ${(e as Error).message}`);
	}
	try {
		const v: unknown = JSON.parse(text);
		if (v === null || typeof v !== "object" || Array.isArray(v)) throw new Error("not a JSON object");
		return v as Record<string, unknown>;
	} catch (e) {
		throw new UserError(`☁ ${path} is not valid: ${(e as Error).message}`);
	}
}

function str(v: unknown): string | undefined {
	return typeof v === "string" && v.trim() !== "" ? v.trim() : undefined;
}

function strList(v: unknown): string[] {
	return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string" && x !== "") : [];
}

function bool(v: unknown, dflt: boolean): boolean {
	return typeof v === "boolean" ? v : dflt;
}

export interface LoadOptions {
	env?: Env;
	home?: string;
	/** Repo root whose `.omp/cloud.json` is merged in. */
	repoRoot?: string;
}

/**
 * Precedence: environment (`OMP_CLOUD_*`), then the repo file for agent and
 * namespace, then the user file.
 */
export async function loadConfig(opts: LoadOptions = {}): Promise<CloudConfig> {
	const env = opts.env ?? process.env;
	const dir = agentDir(env, opts.home);
	const userPath = join(dir, "cloud.json");
	const user = await readJson(userPath);
	const sources: string[] = [];
	if (user) sources.push(userPath);

	const shipRaw = (user?.ship ?? {}) as Record<string, unknown>;
	const cfg: CloudConfig = {
		url: str(user?.url),
		token: str(user?.token),
		tokenCommand: strList(user?.tokenCommand),
		namespace: str(user?.namespace),
		agent: str(user?.agent),
		maxUploadMB: typeof user?.maxUploadMB === "number" && user.maxUploadMB > 0 ? user.maxUploadMB : 512,
		forceBundle: bool(user?.forceBundle, false),
		ship: {
			settings: bool(shipRaw.settings, true),
			extensions: bool(shipRaw.extensions, true),
			memory: bool(shipRaw.memory, true),
		},
		excludeExtensions: strList(user?.excludeExtensions),
		repo: { secretFiles: [], secretEnv: [] },
		sources,
	};

	if (opts.repoRoot) {
		const repoPath = join(opts.repoRoot, ".omp", "cloud.json");
		const repo = await readJson(repoPath);
		if (repo) {
			sources.push(repoPath);
			cfg.repo = { agent: str(repo.agent), namespace: str(repo.namespace), secretFiles: strList(repo.secretFiles), secretEnv: strList(repo.secretEnv) };
			if (cfg.repo.agent) cfg.agent = cfg.repo.agent;
			if (cfg.repo.namespace) cfg.namespace = cfg.repo.namespace;
		}
	}

	const envUrl = str(env.OMP_CLOUD_URL);
	const envNs = str(env.OMP_CLOUD_NAMESPACE);
	const envAgent = str(env.OMP_CLOUD_AGENT);
	if (envUrl) cfg.url = envUrl;
	if (envNs) cfg.namespace = envNs;
	if (envAgent) cfg.agent = envAgent;
	if (envUrl || str(env.OMP_CLOUD_TOKEN) || envNs || envAgent) sources.push("OMP_CLOUD_* environment");
	if (cfg.url) cfg.url = cfg.url.replace(/\/+$/, "");
	return cfg;
}

/** Token precedence: `OMP_CLOUD_TOKEN`, then `token`, then `tokenCommand` (stdout trimmed, 10s timeout). */
export async function resolveToken(cfg: CloudConfig, env: Env = process.env): Promise<string | undefined> {
	const fromEnv = str(env.OMP_CLOUD_TOKEN);
	if (fromEnv) return fromEnv;
	if (cfg.token) return cfg.token;
	const [cmd, ...args] = cfg.tokenCommand ?? [];
	if (!cmd) return undefined;
	const r = await run(cmd, args, { timeoutMs: 10_000, env });
	if (r.code !== 0) throw new UserError(`☁ tokenCommand failed: ${r.stderr.trim().slice(0, 200) || `exit ${r.code}`}`);
	return str(r.stdout);
}

export async function resolveConnection(cfg: CloudConfig, env: Env = process.env): Promise<Connection> {
	const token = await resolveToken(cfg, env);
	if (!cfg.url || !token) throw new UserError(NOT_CONFIGURED);
	return { url: cfg.url, token };
}

/** Picks the cloud agent: the configured one, else the only one `whoami` lists. */
export function selectAgent(who: Whoami, cfg: Pick<CloudConfig, "namespace" | "agent">): WhoamiAgent {
	let candidates = who.agents;
	if (cfg.namespace) candidates = candidates.filter(a => a.namespace === cfg.namespace);
	if (cfg.agent) candidates = candidates.filter(a => a.name === cfg.agent);
	const [only, ...rest] = candidates;
	if (!only) {
		if (cfg.agent || cfg.namespace) {
			const want = cfg.namespace && cfg.agent ? `${cfg.namespace}/${cfg.agent}` : (cfg.agent ?? cfg.namespace);
			throw new UserError(`☁ cloud agent ${want} is not available to ${who.user}`);
		}
		throw new UserError(`☁ no cloud agent is available to ${who.user}`);
	}
	if (rest.length > 0) {
		const list = candidates.map(a => `${a.namespace}/${a.name}`).join(", ");
		throw new UserError(`☁ several cloud agents are available (${list}); set "agent" and "namespace" in ~/.omp/agent/cloud.json or .omp/cloud.json`);
	}
	return only;
}

/** The namespace for thread commands: the configured one, else the only namespace `whoami` lists. */
export function selectNamespace(who: Whoami, cfg: Pick<CloudConfig, "namespace">): string {
	if (cfg.namespace) return cfg.namespace;
	const namespaces = [...new Set(who.agents.map(a => a.namespace))];
	const [only, ...rest] = namespaces;
	if (!only) throw new UserError(`☁ no cloud agent is available to ${who.user}`);
	if (rest.length > 0) {
		throw new UserError(`☁ several namespaces are available (${namespaces.join(", ")}); set "namespace" in ~/.omp/agent/cloud.json or .omp/cloud.json`);
	}
	return only;
}
