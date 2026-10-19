/** `/cloud` argument parsing, thread-id resolution and completion. Pure: no I/O. */

export const VERBS = [
	"new",
	"list",
	"show",
	"watch",
	"unwatch",
	"send",
	"stop",
	"pull",
	"resume",
	"delete",
	"status",
	"help",
] as const;
export type Verb = (typeof VERBS)[number];

/** Verbs whose first argument names a thread. */
export const ID_VERBS: readonly Verb[] = ["show", "watch", "send", "stop", "pull", "resume", "delete"];
export type IdVerb = "show" | "watch" | "send" | "stop" | "pull" | "resume" | "delete";

export const USAGE_LAUNCH = "☁ usage: /cloud <task for the cloud agent>";
export const USAGE_SEND = "☁ usage: /cloud send <id> <message>";

/** What the user typed, before thread ids are resolved. */
export type Parsed =
	| { kind: "launch"; task: string }
	| { kind: "list" | "unwatch" | "status" | "help" }
	| { kind: IdVerb; token: string; text: string }
	| { kind: "error"; message: string };

/** A fully resolved command. */
export type Command =
	| { kind: "launch"; task: string }
	| { kind: "list" | "unwatch" | "status" | "help" }
	| { kind: Exclude<IdVerb, "send">; thread: string }
	| { kind: "send"; thread: string; text: string }
	| { kind: "error"; message: string };

function isVerb(s: string): s is Verb {
	return (VERBS as readonly string[]).includes(s);
}

function isIdVerb(s: Verb): s is IdVerb {
	return (ID_VERBS as readonly string[]).includes(s);
}

function splitFirst(s: string): [string, string] {
	const m = /^(\S+)\s*([\s\S]*)$/.exec(s);
	return m ? [m[1] ?? "", m[2] ?? ""] : ["", ""];
}

/**
 * The first token, lowercased, is a verb or the task starts here. Empty args
 * list; `new` launches the rest even when it begins with a verb word.
 */
export function parseCommand(args: string): Parsed {
	const trimmed = args.trim();
	if (trimmed === "") return { kind: "list" };
	const [first, rest] = splitFirst(trimmed);
	const verb = first.toLowerCase();
	if (!isVerb(verb)) return { kind: "launch", task: trimmed };
	if (verb === "new") return rest === "" ? { kind: "error", message: USAGE_LAUNCH } : { kind: "launch", task: rest };
	if (!isIdVerb(verb)) return { kind: verb };
	const [token, text] = splitFirst(rest);
	if (verb === "send") {
		if (token === "" || text === "") return { kind: "error", message: USAGE_SEND };
	} else if (token === "") {
		return { kind: "error", message: `☁ usage: /cloud ${verb} <id>` };
	}
	return { kind: verb, token, text };
}

export type ThreadMatch = { name: string } | { error: "none" | "ambiguous"; candidates: string[] };

/** A thread by full name, unique 6-character suffix, or unique name prefix. */
export function resolveThread(token: string, names: readonly string[]): ThreadMatch {
	const t = token.toLowerCase();
	const exact = names.find(n => n.toLowerCase() === t);
	if (exact) return { name: exact };
	const tiers = [
		names.filter(n => t.length === 6 && n.toLowerCase().endsWith(`-${t}`)),
		names.filter(n => n.toLowerCase().startsWith(t)),
	];
	for (const hits of tiers) {
		if (hits.length === 1 && hits[0]) return { name: hits[0] };
		if (hits.length > 1) return { error: "ambiguous", candidates: hits };
	}
	return { error: "none", candidates: [] };
}

/** The last 6 characters of a thread name, which `/cloud <verb> <short>` accepts. */
export function shortName(name: string): string {
	return name.slice(-6);
}

export function hintForUnmatched(token: string, verb: string): string {
	return `☁ no thread matches "${token}"; to start a cloud task beginning with "${verb}", use /cloud new <task>`;
}

/** Resolves a parsed command's thread token against the namespace's thread names. */
export function resolveCommand(parsed: Parsed, names: readonly string[]): Command {
	if (parsed.kind === "launch" || parsed.kind === "error") return parsed;
	if (!("token" in parsed)) return { kind: parsed.kind };
	const m = resolveThread(parsed.token, names);
	if ("error" in m) {
		if (m.error === "ambiguous") {
			return { kind: "error", message: `☁ "${parsed.token}" matches several threads: ${m.candidates.join(", ")}` };
		}
		return { kind: "error", message: hintForUnmatched(parsed.token, parsed.kind) };
	}
	if (parsed.kind === "send") return { kind: "send", thread: m.name, text: parsed.text };
	return { kind: parsed.kind, thread: m.name };
}

export interface Completion {
	value: string;
	label: string;
	description?: string;
}

const VERB_HELP: Record<Verb, string> = {
	new: "hand off a task (even one that begins with a verb word)",
	list: "list cloud threads",
	show: "show a thread's latest reply and events",
	watch: "watch a thread live",
	unwatch: "stop the live widget",
	send: "send a follow-up message to a thread",
	stop: "stop the running turn and cancel queued ones",
	pull: "fetch the thread's result branch",
	resume: "continue the thread's conversation locally",
	delete: "delete a thread",
	status: "show the connection and available agents",
	help: "show usage",
};

export function verbHelp(verb: Verb): string {
	return VERB_HELP[verb];
}

/**
 * Synchronous completion: verbs while the first word is being typed, then
 * thread names from the cache after an id verb. `value` replaces the whole
 * argument text, so it carries the verb.
 */
export function completions(prefix: string, cachedNames: readonly string[]): Completion[] | null {
	const p = prefix.replace(/^\s+/, "");
	if (!/\s/.test(p)) {
		const lower = p.toLowerCase();
		const items = VERBS.filter(v => v.startsWith(lower)).map(v => ({ value: v, label: v, description: VERB_HELP[v] }));
		return items.length > 0 ? items : null;
	}
	const [first, rest] = splitFirst(p);
	const verb = first.toLowerCase();
	if (!isVerb(verb) || !isIdVerb(verb)) return null;
	// Past the id the rest is free text.
	if (/\s/.test(rest)) return null;
	const typed = rest.toLowerCase();
	const items = cachedNames
		.filter(n => n.toLowerCase().startsWith(typed) || n.toLowerCase().endsWith(typed))
		.map(n => ({ value: `${verb} ${n}`, label: n }));
	return items.length > 0 ? items : null;
}
