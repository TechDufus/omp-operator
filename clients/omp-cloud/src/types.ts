/** Wire types of the omp-operator cloud API (/api/v1) and the host surface the extension needs. */

import type { ExtensionUIContext } from "@oh-my-pi/pi-coding-agent";

// ---------------------------------------------------------------------------
// Cloud API responses
// ---------------------------------------------------------------------------

export type TurnState = "queued" | "running" | "done" | "failed" | "cancelled";
export type ThreadPhase = "Working" | "Waiting" | "Idle" | "";

export interface QuestionOption {
	label: string;
	description?: string;
}

/** The oldest unanswered question of a thread (`ThreadSummary.question`). */
export interface PendingQuestion {
	turn: number;
	id: number;
	ask: number;
	text: string;
	header?: string;
	options?: QuestionOption[] | null;
	multi?: boolean;
	recommended?: number | null;
	askedAt: string;
	/** Unanswered questions of that ask, this one included. */
	remaining: number;
}

/** One question of a turn (`ThreadTurn.questions`) with its answer, once given. */
export interface Question {
	id: number;
	ask: number;
	key: string;
	text: string;
	header?: string;
	options?: QuestionOption[] | null;
	multi?: boolean;
	recommended?: number | null;
	askedAt: string;
	selected?: string[] | null;
	answer?: string;
	answeredBy?: string;
	answeredAt?: string;
	expired?: boolean;
}

/** Who started a thread: the user with `/cloud`, or the local model through the `cloud` tool. */
export type ThreadOrigin = "user" | "tool";

export interface WhoamiAgent {
	namespace: string;
	name: string;
	ready: boolean;
	defaultModel: string;
	modelPatterns: string[];
	gitHosts: string[];
	maxUploadBytes: number;
	/** Absent from operators older than v0.2.1. */
	capacity?: { maxConcurrentSessions: number; running: number };
	threads?: { working: number; waiting: number; idle: number };
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
	/** The oldest unanswered question; absent from operators older than v0.2.1. */
	question?: PendingQuestion | null;
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
	usage?: Record<string, number | string>;
	head?: string;
	filesChanged?: number;
	insertions?: number;
	deletions?: number;
	pushed?: boolean;
	pushError?: string;
	questions?: Question[] | null;
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

/** The answer to one question: option labels, free text, or both. */
export interface AnswerBody {
	selected?: string[];
	answer?: string;
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
	/** Present in every real host; absent only in test doubles that never prompt. */
	select?(title: string, options: string[]): Promise<string | undefined>;
	input?(title: string, placeholder?: string): Promise<string | undefined>;
	/** Interactive components and overlays; a no-op returning undefined in RPC hosts. */
	custom?: ExtensionUIContext["custom"];
}

export interface Host {
	ui: HostUi;
	cwd: string;
	/** `"tui"` is the only mode that can show overlays. */
	mode?: string;
	hasUI?: boolean;
	model: { provider: string; id: string } | undefined;
	sessionManager: {
		getSessionFile(): string | undefined;
		getSessionDir(): string;
		/** All entries of the session file, for recovering what earlier cards recorded. */
		getEntries?(): readonly unknown[];
	};
	isIdle(): boolean;
	setInterval(callback: () => void, ms?: number): Timer;
	setTimeout(callback: () => void, ms?: number): Timer;
	clearTimer(timer: Timer): void;
}

export interface CommandHost extends Host {
	switchSession(sessionPath: string): Promise<{ cancelled: boolean }>;
}

export type DeliverAs = "steer" | "followUp" | "nextTurn" | "aside";

export interface PiHost {
	getThinkingLevel(): string | undefined;
	sendMessage(
		message: { customType: string; content: string; display: boolean; details?: unknown },
		options?: { triggerTurn?: boolean; deliverAs?: DeliverAs },
	): void;
}

/** An error whose message is shown to the user exactly as written. */
export class UserError extends Error {
	override name = "UserError";
}
