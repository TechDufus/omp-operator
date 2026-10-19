import type { AnswerBody, Question } from "./types";
import { createWriteStream } from "node:fs";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";
import type { ReadableStream as WebReadableStream } from "node:stream/web";
import type {
	HandoffRequest,
	ResultHeaders,
	ThreadDetail,
	ThreadEvents,
	ThreadSummary,
	Whoami,
} from "./types";

export class ApiError extends Error {
	override name = "ApiError";
	constructor(
		readonly status: number,
		message: string,
	) {
		super(message);
	}
}

export interface HandoffParts {
	request: HandoffRequest;
	/** Path of the git bundle; omitted when the delta is empty. */
	source?: string;
	/** Rewritten conversation JSONL; omitted without a conversation. */
	conversation?: { filename: string; data: Uint8Array };
	/** Path of setup.tar.gz. */
	setup?: string;
}

export interface HandoffResponse extends ThreadSummary {
	warnings?: string[];
}

export type FetchFn = (input: string, init?: RequestInit) => Promise<Response>;

const UPLOAD_TIMEOUT_MS = 10 * 60 * 1000;
const API_TIMEOUT_MS = 30 * 1000;

export class CloudClient {
	readonly #base: string;
	readonly #token: string;
	readonly #fetch: FetchFn;

	constructor(url: string, token: string, fetchFn: FetchFn = (i, init) => fetch(i, init)) {
		this.#base = url.replace(/\/+$/, "");
		this.#token = token;
		this.#fetch = fetchFn;
	}

	#ns(ns: string, name?: string, sub?: string): string {
		let p = `/api/v1/namespaces/${encodeURIComponent(ns)}/threads`;
		if (name) p += `/${encodeURIComponent(name)}`;
		if (sub) p += `/${sub}`;
		return p;
	}

	async #req(method: string, path: string, init: { body?: FormData; json?: unknown; timeoutMs?: number } = {}): Promise<Response> {
		const headers: Record<string, string> = { Authorization: `Bearer ${this.#token}` };
		let body: FormData | string | undefined = init.body;
		if (init.json !== undefined) {
			headers["Content-Type"] = "application/json";
			body = JSON.stringify(init.json);
		}
		let res: Response;
		try {
			res = await this.#fetch(this.#base + path, {
				method,
				headers,
				body,
				signal: AbortSignal.timeout(init.timeoutMs ?? API_TIMEOUT_MS),
			});
		} catch (e) {
			throw new Error(`cannot reach ${this.#base}: ${(e as Error).message}`);
		}
		if (!res.ok) {
			let message = `${res.status} ${res.statusText}`.trim();
			try {
				const j = (await res.json()) as { error?: string };
				if (j.error) message = j.error;
			} catch {
				// non-JSON error body: keep the status line
			}
			throw new ApiError(res.status, message);
		}
		return res;
	}

	async #json<T>(method: string, path: string, init: { json?: unknown } = {}): Promise<T> {
		const res = await this.#req(method, path, init);
		return (await res.json()) as T;
	}

	whoami(): Promise<Whoami> {
		return this.#json<Whoami>("GET", "/api/v1/whoami");
	}

	async listThreads(ns: string): Promise<ThreadSummary[]> {
		const r = await this.#json<{ threads: ThreadSummary[] }>("GET", this.#ns(ns));
		return r.threads ?? [];
	}

	thread(ns: string, name: string): Promise<ThreadDetail> {
		return this.#json<ThreadDetail>("GET", this.#ns(ns, name));
	}

	async events(ns: string, name: string, since?: string): Promise<ThreadEvents> {
		const q = since ? `?since=${encodeURIComponent(since)}` : "";
		const r = await this.#json<ThreadEvents>("GET", this.#ns(ns, name, "events") + q);
		return { session: r.session ?? "", events: r.events ?? [] };
	}

	async message(ns: string, name: string, text: string): Promise<{ id: number }> {
		const r = await this.#json<{ turn: { id: number } }>("POST", this.#ns(ns, name, "messages"), { json: { text } });
		return r.turn;
	}

	stop(ns: string, name: string): Promise<{ stoppedRunning: boolean; cancelled: number }> {
		return this.#json("POST", this.#ns(ns, name, "stop"));
	}

	/** Answers one question of a turn; `selected` holds option labels, `answer` free text. */
	answer(ns: string, name: string, turn: number, question: number, body: AnswerBody): Promise<Question> {
		return this.#json("POST", this.#ns(ns, name, `turns/${turn}/questions/${question}/answer`), { json: body });
	}

	async deleteThread(ns: string, name: string): Promise<void> {
		await this.#req("DELETE", this.#ns(ns, name));
	}

	/** Multipart handoff. Part order is `request`, `source`, `conversation`, `setup`. */
	async createThread(ns: string, parts: HandoffParts): Promise<HandoffResponse> {
		const form = new FormData();
		form.append("request", new Blob([JSON.stringify(parts.request)], { type: "application/json" }), "request.json");
		if (parts.source) form.append("source", Bun.file(parts.source), "source.bundle");
		if (parts.conversation) {
			form.append("conversation", new Blob([parts.conversation.data], { type: "application/x-ndjson" }), parts.conversation.filename);
		}
		if (parts.setup) form.append("setup", Bun.file(parts.setup), "setup.tar.gz");
		const res = await this.#req("POST", this.#ns(ns), { body: form, timeoutMs: UPLOAD_TIMEOUT_MS });
		return (await res.json()) as HandoffResponse;
	}

	async #download(path: string, dest: string): Promise<Headers> {
		const res = await this.#req("GET", path, { timeoutMs: UPLOAD_TIMEOUT_MS });
		if (!res.body) throw new Error("empty response body");
		await pipeline(Readable.fromWeb(res.body as WebReadableStream), createWriteStream(dest));
		return res.headers;
	}

	/** Streams the result bundle to `dest`; throws ApiError(404) when there is no result yet. */
	async downloadResult(ns: string, name: string, dest: string): Promise<ResultHeaders> {
		const h = await this.#download(this.#ns(ns, name, "result"), dest);
		return { branch: h.get("X-Omp-Branch") ?? "", head: h.get("X-Omp-Head") ?? "", base: h.get("X-Omp-Base") ?? "" };
	}

	/** Streams the conversation continuity tarball (tar.gz) to `dest`. */
	async downloadConversation(ns: string, name: string, dest: string): Promise<void> {
		await this.#download(this.#ns(ns, name, "conversation"), dest);
	}
}
