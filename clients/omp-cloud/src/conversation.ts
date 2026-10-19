import { randomFillSync } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import { basename } from "node:path";

export const MAX_CONVERSATION_BYTES = 64 * 1024 * 1024;
/** `customType` of the cards this extension adds to the transcript; never uploaded. */
export const CARD_TYPE = "omp-cloud";

const NL = 0x0a;
const BLOB_REF_RE = /\bblob:sha256:([a-f0-9]{64})\b/g;
const PARENT_RE = /"parentId":"([^"]+)"/;

/** A UUIDv7: 48-bit millisecond timestamp, version 7, RFC 4122 variant, 74 random bits. */
export function uuidv7(now: number = Date.now()): string {
	const b = new Uint8Array(16);
	randomFillSync(b);
	let ms = now;
	for (let i = 5; i >= 0; i--) {
		b[i] = ms % 256;
		ms = Math.floor(ms / 256);
	}
	b[6] = ((b[6] ?? 0) & 0x0f) | 0x70;
	b[8] = ((b[8] ?? 0) & 0x3f) | 0x80;
	const h = Buffer.from(b).toString("hex");
	return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

export interface RewriteOptions {
	/**
	 * The id of the pending `cloud` tool call of a delegation started by the tool. The assistant
	 * message carrying it, and every entry after it, is dropped so the cloud omp never sees a
	 * tool call without a result.
	 */
	dropFromToolCall?: string;
}

export interface RewrittenConversation {
	/** Original filename with its session id replaced by the new one. */
	filename: string;
	data: Buffer;
	oldId: string;
	newId: string;
	/** `blob:sha256:<hash>` references found in the uploaded file. */
	blobHashes: string[];
	/** Cards (`custom_message` entries of type `omp-cloud`) left out of the upload. */
	droppedCards: number;
	/** Entries cut from the pending tool call on. */
	droppedForToolCall: number;
}

export function blobHashes(data: Buffer): string[] {
	const found = new Set<string>();
	for (const m of data.toString("latin1").matchAll(BLOB_REF_RE)) if (m[1]) found.add(m[1]);
	return [...found];
}

interface Line {
	buf: Buffer;
	/** Whether a newline followed the line in the original file. */
	nl: boolean;
}

function splitLines(data: Buffer): Line[] {
	const lines: Line[] = [];
	let start = 0;
	while (start < data.length) {
		const end = data.indexOf(NL, start);
		if (end < 0) {
			lines.push({ buf: data.subarray(start), nl: false });
			break;
		}
		lines.push({ buf: data.subarray(start, end), nl: true });
		start = end + 1;
	}
	return lines;
}

function joinLines(lines: Line[]): Buffer {
	const parts: Buffer[] = [];
	const nl = Buffer.from([NL]);
	for (const l of lines) {
		parts.push(l.buf);
		if (l.nl) parts.push(nl);
	}
	return Buffer.concat(parts);
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
	return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function parseLine(l: Line): Record<string, unknown> | undefined {
	try {
		return asRecord(JSON.parse(l.buf.toString("utf8")));
	} catch {
		return undefined;
	}
}

function carriesToolCall(entry: Record<string, unknown>, toolCallId: string): boolean {
	if (entry.type !== "message") return false;
	const msg = asRecord(entry.message);
	if (!msg || msg.role !== "assistant" || !Array.isArray(msg.content)) return false;
	return msg.content.some(block => {
		const b = asRecord(block);
		return b?.type === "toolCall" && b.id === toolCallId;
	});
}

/**
 * Cuts the pending tool call: the first assistant message that carries it and every line after
 * it (descendants are always later in the file). Returns the number of lines dropped.
 */
function dropFromToolCall(lines: Line[], toolCallId: string): number {
	const needle = Buffer.from(toolCallId);
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i];
		if (!line || !line.buf.includes(needle)) continue;
		const entry = parseLine(line);
		if (entry && carriesToolCall(entry, toolCallId)) {
			const dropped = lines.length - i;
			lines.length = i;
			// descendants are always later in the file, so nothing left refers to a dropped entry
			return dropped;
		}
	}
	return 0;
}

/** Drops the cards and re-links their children to the nearest surviving ancestor. Returns the cards dropped. */
function dropCards(lines: Line[]): { lines: Line[]; dropped: number } {
	const parentOf = new Map<string, string | null>();
	for (const l of lines) {
		if (!l.buf.includes("custom_message") || !l.buf.includes(CARD_TYPE)) continue;
		const e = parseLine(l);
		if (e?.type !== "custom_message" || e.customType !== CARD_TYPE || typeof e.id !== "string") continue;
		parentOf.set(e.id, typeof e.parentId === "string" ? e.parentId : null);
	}
	if (parentOf.size === 0) return { lines, dropped: 0 };

	const resolve = (id: string | null): string | null => {
		let cur = id;
		const seen = new Set<string>();
		while (cur !== null && parentOf.has(cur) && !seen.has(cur)) {
			seen.add(cur);
			cur = parentOf.get(cur) ?? null;
		}
		return cur;
	};

	const kept: Line[] = [];
	let dropped = 0;
	for (const l of lines) {
		const isCardLine = l.buf.includes("custom_message") && l.buf.includes(CARD_TYPE);
		const parent = PARENT_RE.exec(l.buf.toString("utf8"))?.[1];
		if (!isCardLine && !(parent && parentOf.has(parent))) {
			kept.push(l);
			continue;
		}
		const e = parseLine(l);
		if (!e) {
			kept.push(l);
		} else if (e.type === "custom_message" && e.customType === CARD_TYPE) {
			dropped++;
		} else if (typeof e.parentId === "string" && parentOf.has(e.parentId)) {
			kept.push({ buf: Buffer.from(JSON.stringify({ ...e, parentId: resolve(e.parentId) }), "utf8"), nl: l.nl });
		} else {
			kept.push(l);
		}
	}
	return { lines: kept, dropped };
}

/**
 * Re-identifies a session file for the cloud: the `"type":"session"` header line gets a new
 * `id` and `parentSession` set to the original path; cards of this extension are removed and
 * their children re-linked; a pending tool call is cut. Every other line, including the title
 * slot, stays byte-identical.
 */
export function rewriteConversation(
	original: Buffer,
	originalPath: string,
	newId: string = uuidv7(),
	opts: RewriteOptions = {},
): RewrittenConversation {
	let lines = splitLines(original);
	let headerAt = -1;
	let header: Record<string, unknown> | undefined;
	for (let i = 0; i < lines.length; i++) {
		const l = lines[i];
		if (!l || !l.buf.includes('"session"')) continue;
		const v = parseLine(l);
		if (v?.type === "session") {
			headerAt = i;
			header = v;
			break;
		}
	}
	if (!header || headerAt < 0) throw new Error("conversation has no session header line");

	const oldId = typeof header.id === "string" ? header.id : "";
	const headerLine = lines[headerAt];
	if (headerLine) {
		lines[headerAt] = { buf: Buffer.from(JSON.stringify({ ...header, id: newId, parentSession: originalPath }), "utf8"), nl: headerLine.nl };
	}

	let droppedForToolCall = 0;
	if (opts.dropFromToolCall) droppedForToolCall = dropFromToolCall(lines, opts.dropFromToolCall);
	const cards = dropCards(lines);
	lines = cards.lines;

	const data = joinLines(lines);
	const base = basename(originalPath);
	const stem = base.replace(/\.jsonl$/, "");
	const filename = oldId && stem.endsWith(`_${oldId}`) ? `${stem.slice(0, -oldId.length)}${newId}.jsonl` : `${stem}_${newId}.jsonl`;
	return { filename, data, oldId, newId, blobHashes: blobHashes(data), droppedCards: cards.dropped, droppedForToolCall };
}

export type ConversationResult =
	| { kind: "none" }
	| { kind: "too-large"; bytes: number }
	| ({ kind: "ok" } & RewrittenConversation);

/** Reads and rewrites the current session file, if it exists on disk and fits the cap. */
export async function loadConversation(sessionFile: string | undefined, opts: RewriteOptions = {}): Promise<ConversationResult> {
	if (!sessionFile) return { kind: "none" };
	let size: number;
	try {
		size = (await stat(sessionFile)).size;
	} catch {
		return { kind: "none" };
	}
	if (size > MAX_CONVERSATION_BYTES) return { kind: "too-large", bytes: size };
	return { kind: "ok", ...rewriteConversation(await readFile(sessionFile), sessionFile, undefined, opts) };
}
