/** Wire types of the omp-operator cloud API (/api/v1) and the host surface the extension needs. */

// ---------------------------------------------------------------------------
// Cloud API responses
// ---------------------------------------------------------------------------

export type TurnState = "queued" | "running" | "done" | "failed" | "cancelled";
export type ThreadPhase = "Working" | "Idle" | "";

export interface WhoamiAgent {
	namespace: string;
	name: string;
	ready: boolean;
	defaultModel: string;
	modelPatterns: string[];
	gitHosts: string[];
	maxUploadBytes: number;
}

export interface Whoami {
	user: string;
	agents: WhoamiAgent[];
}

export interface ThreadTurnSummary {
	id: number;
	state: TurnState;
	title?: string;
	reply?: string;
	error?: string;
	completedAt?: string;
}

export interface ThreadResult {
	branch: string;
	head: string;
	base?: string;
	filesChanged: number;
	insertions: number;
	deletions: number;
	pushed: boolean;
	pushError?: string;
	updatedAt?: string;
}

export interface ThreadSummary {
	namespace: string;
	name: string;
	agent: string;
	title: string;
	owner: string;
	phase: ThreadPhase;
	turns: number;
	queued: number;
	model: string;
	latest?: ThreadTurnSummary | null;
	result?: ThreadResult | null;
	createdAt: string;
	lastActivityAt?: string;
	url: string;
	warnings?: string[];
}

export interface ThreadTurn {
	id: number;
	state: TurnState;
	text: string;
	by: string;
	queuedAt: string;
	startedAt?: string;
	completedAt?: string;
	reply?: string;
	title?: string;
	error?: string;
	usage?: Record<string, number>;
	head?: string;
	filesChanged?: number;
	insertions?: number;
	deletions?: number;
	pushed?: boolean;
	pushError?: string;
}

/** In the detail view `turns` is the turn list; the summary's count is its length. */
export interface ThreadDetail extends Omit<ThreadSummary, "turns"> {
	repo: {
		name: string;
		remoteURL?: string;
		baseRef?: string;
		baseCommit?: string;
		handoffCommit?: string;
		localBranch?: string;
	};
	paths?: { home?: string; workspace?: string };
	activeSession?: string;
	turns: ThreadTurn[];
}

export interface ThreadEvent {
	at: string;
	kind: string;
	text: string;
}

export interface ThreadEvents {
	session: string;
	events: ThreadEvent[];
}

export interface ResultHeaders {
	branch: string;
	head: string;
	base: string;
}

// ---------------------------------------------------------------------------
// Handoff request
// ---------------------------------------------------------------------------

export interface HandoffRequest {
	agent: string;
	prompt: string;
	model?: string;
	thinking?: string;
	repo: {
		name: string;
		remoteURL: string;
		baseRef: string;
		baseCommit: string;
		handoffCommit: string;
		localBranch: string;
	};
	paths: { home: string; workspace: string };
	author: { name: string; email: string };
	secretFiles: { path: string; content: string }[];
	secretEnv: Record<string, string>;
	conversationFile: string;
}

// ---------------------------------------------------------------------------
// Host surface (a structural subset of omp's ExtensionContext)
// ---------------------------------------------------------------------------

export type NotifyType = "info" | "warning" | "error";

export interface HostUi {
	notify(message: string, type?: NotifyType): void;
	setWidget(key: string, content: string[] | undefined, options?: { placement?: "aboveEditor" | "belowEditor" }): void;
	setStatus(key: string, text: string | undefined): void;
	confirm(title: string, message: string): Promise<boolean>;
}

export interface Host {
	ui: HostUi;
	cwd: string;
	model: { provider: string; id: string } | undefined;
	sessionManager: {
		getSessionFile(): string | undefined;
		getSessionDir(): string;
	};
	isIdle(): boolean;
	setInterval(callback: () => void, ms?: number): Timer;
	setTimeout(callback: () => void, ms?: number): Timer;
	clearTimer(timer: Timer): void;
}

export interface CommandHost extends Host {
	switchSession(sessionPath: string): Promise<{ cancelled: boolean }>;
}

export interface PiHost {
	getThinkingLevel(): string | undefined;
	sendMessage(
		message: { customType: string; content: string; display: boolean },
		options?: { triggerTurn?: boolean; deliverAs?: "steer" | "followUp" | "nextTurn" | "aside" },
	): void;
}

/** An error whose message is shown to the user exactly as written. */
export class UserError extends Error {
	override name = "UserError";
}
