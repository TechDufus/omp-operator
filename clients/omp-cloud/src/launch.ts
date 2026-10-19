import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CloudClient, HandoffResponse } from "./api";
import type { CloudConfig, Env } from "./config";
import { loadConversation, MAX_CONVERSATION_BYTES } from "./conversation";
import { buildSetup } from "./setup";
import { takeSnapshot } from "./snapshot";
import type { HandoffRequest, Host, PiHost, WhoamiAgent } from "./types";

export interface LaunchInput {
	host: Pick<Host, "cwd" | "model" | "sessionManager">;
	pi: Pick<PiHost, "getThinkingLevel">;
	cfg: CloudConfig;
	client: Pick<CloudClient, "createThread">;
	agent: WhoamiAgent;
	task: string;
	env: Env;
	home: string;
	agentDir: string;
	/** This extension's own directory; excluded from the shipped setup. */
	ownRoot: string;
}

export interface LaunchResult {
	response: HandoffResponse;
	/** Local warnings (conversation too large, memory copied without a snapshot). */
	warnings: string[];
}

/**
 * Snapshots the repo, the conversation and the omp setup, and uploads them
 * as one handoff. Throws on any failure; nothing is left in the temp dir.
 */
export async function launchHandoff(input: LaunchInput): Promise<LaunchResult> {
	const { host, cfg, agent } = input;
	const tmp = await mkdtemp(join(tmpdir(), "omp-cloud-"));
	try {
		const limits = [cfg.maxUploadMB * 1024 * 1024];
		if (agent.maxUploadBytes > 0) limits.push(agent.maxUploadBytes);
		const snap = await takeSnapshot({
			cwd: host.cwd,
			tmpDir: tmp,
			gitHosts: agent.gitHosts ?? [],
			forceBundle: cfg.forceBundle,
			maxBytes: Math.min(...limits),
			secretFiles: cfg.repo.secretFiles,
			secretEnv: cfg.repo.secretEnv,
			env: input.env,
			home: input.home,
		});

		const warnings: string[] = [];
		const conv = await loadConversation(host.sessionManager.getSessionFile());
		if (conv.kind === "too-large") {
			warnings.push(`conversation is over ${MAX_CONVERSATION_BYTES / (1024 * 1024)} MiB; the cloud agent starts without it`);
		}

		const setup = await buildSetup({
			agentDir: input.agentDir,
			ship: cfg.ship,
			excludeExtensions: cfg.excludeExtensions,
			ownRoot: input.ownRoot,
			blobHashes: conv.kind === "ok" ? conv.blobHashes : [],
			tmpDir: tmp,
		});
		warnings.push(...setup.warnings);

		const thinking = input.pi.getThinkingLevel();
		const request: HandoffRequest = {
			agent: agent.name,
			prompt: input.task,
			...(host.model ? { model: `${host.model.provider}/${host.model.id}` } : {}),
			...(thinking ? { thinking } : {}),
			repo: {
				name: snap.name,
				remoteURL: snap.remoteURL,
				baseRef: snap.baseRef,
				baseCommit: snap.baseCommit,
				handoffCommit: snap.handoffCommit,
				localBranch: snap.localBranch,
			},
			paths: snap.paths,
			author: snap.author,
			secretFiles: snap.secretFiles,
			secretEnv: snap.secretEnv,
			conversationFile: conv.kind === "ok" ? conv.filename : "",
		};

		const response = await input.client.createThread(agent.namespace, {
			request,
			source: snap.bundlePath,
			conversation: conv.kind === "ok" ? { filename: conv.filename, data: conv.data } : undefined,
			setup: setup.path,
		});
		return { response, warnings };
	} finally {
		await rm(tmp, { recursive: true, force: true });
	}
}
