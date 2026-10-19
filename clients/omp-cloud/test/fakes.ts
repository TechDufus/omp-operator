import type { Host, HostUi, NotifyType, PiHost } from "../src/types";

export interface FakeHost {
	host: Host;
	notes: [string, NotifyType | undefined][];
	widgets: (string[] | undefined)[];
	status: (string | undefined)[];
	/** Callbacks scheduled with `setTimeout`. */
	timeouts: (() => void)[];
	/** The callbacks registered with `setInterval`, run by `tick`. */
	intervalCallbacks: (() => void)[];
	idle: boolean;
	/** Runs the interval callbacks and lets the async poll finish. */
	tick(): Promise<void>;
}

export interface FakeHostOptions {
	cwd?: string;
	mode?: string;
	sessionFile?: string;
	entries?: unknown[];
	select?: (title: string, options: string[]) => Promise<string | undefined>;
	input?: (title: string, placeholder?: string) => Promise<string | undefined>;
	custom?: HostUi["custom"];
	confirm?: boolean;
}

export function fakeHost(opts: FakeHostOptions = {}): FakeHost {
	const f: FakeHost = {
		notes: [],
		widgets: [],
		status: [],
		timeouts: [],
		intervalCallbacks: [],
		idle: true,
		tick: async () => {},
		host: undefined as unknown as Host,
	};
	f.host = {
		cwd: opts.cwd ?? "/x",
		mode: opts.mode,
		model: { provider: "anthropic", id: "claude-sonnet-5-5" },
		sessionManager: {
			getSessionFile: () => opts.sessionFile,
			getSessionDir: () => "/s",
			getEntries: () => opts.entries ?? [],
		},
		isIdle: () => f.idle,
		ui: {
			notify: (m, t) => void f.notes.push([m, t]),
			setWidget: (_k, c) => void f.widgets.push(c),
			setStatus: (_k, t) => void f.status.push(t),
			confirm: async () => opts.confirm ?? true,
			select: opts.select,
			input: opts.input,
			custom: opts.custom,
		},
		setInterval: callback => {
			f.intervalCallbacks.push(callback);
			return { id: "interval" } as unknown as Timer;
		},
		setTimeout: callback => {
			f.timeouts.push(callback);
			return { id: "timeout" } as unknown as Timer;
		},
		clearTimer: () => {},
	};
	f.tick = async () => {
		for (const cb of f.intervalCallbacks) cb();
		// The fakes resolve in microtasks; one macrotask turn lets the whole poll finish.
		const { promise, resolve } = Promise.withResolvers<void>();
		setImmediate(resolve);
		await promise;
	};
	return f;
}

export interface SentMessage {
	message: { customType: string; content: string; display: boolean; details?: unknown; attribution?: "user" | "agent" };
	options: { triggerTurn?: boolean; deliverAs?: string } | undefined;
}

export interface FakePi {
	pi: PiHost;
	sent: SentMessage[];
}

export function fakePi(thinking: string | undefined = "high"): FakePi {
	const sent: SentMessage[] = [];
	return {
		sent,
		pi: {
			getThinkingLevel: () => thinking,
			sendMessage: (message, options) => void sent.push({ message, options }),
		},
	};
}

/** Waits until `cond` holds, polling the event loop; fails after `ms`. */
export async function until(cond: () => boolean, ms = 3000): Promise<void> {
	const end = Date.now() + ms;
	while (!cond()) {
		if (Date.now() > end) throw new Error("condition not met in time");
		await Bun.sleep(5);
	}
}
