import { randomFillSync } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import { basename } from "node:path";

export const MAX_CONVERSATION_BYTES = 64 * 1024 * 1024;

const NL = 0x0a;
const BLOB_REF_RE = /\bblob:sha256:([a-f0-9]{64})\b/g;

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

export interface RewrittenConversation {
	/** Original filename with its session id replaced by the new one. */
	filename: string;
	data: Buffer;
	oldId: string;
	newId: string;
	/** `blob:sha256:<hash>` references found in the file. */
	blobHashes: string[];
}

export function blobHashes(data: Buffer): string[] {
	const found = new Set<string>();
	for (const m of data.toString("latin1").matchAll(BLOB_REF_RE)) if (m[1]) found.add(m[1]);
	return [...found];
}

/**
 * Re-identifies a session file for the cloud: only the `"type":"session"`
 * header line changes (new `id`, `parentSession` set to the original path);
 * every other line, including the title slot, stays byte-identical.
 */
export function rewriteConversation(
	original: Buffer,
	originalPath: string,
	newId: string = uuidv7(),
): RewrittenConversation {
	let start = 0;
	let header: { start: number; end: number; value: Record<string, unknown> } | undefined;
	while (start < original.length) {
		let end = original.indexOf(NL, start);
		if (end < 0) end = original.length;
		const line = original.subarray(start, end);
		if (line.includes('"session"')) {
			try {
				const v: unknown = JSON.parse(line.toString("utf8"));
				if (v && typeof v === "object" && (v as Record<string, unknown>).type === "session") {
					header = { start, end, value: v as Record<string, unknown> };
					break;
				}
			} catch {
				// not JSON: keep scanning
			}
		}
		start = end + 1;
	}
	if (!header) throw new Error("conversation has no session header line");

	const oldId = typeof header.value.id === "string" ? header.value.id : "";
	const rewritten = { ...header.value, id: newId, parentSession: originalPath };
	const data = Buffer.concat([
		original.subarray(0, header.start),
		Buffer.from(JSON.stringify(rewritten), "utf8"),
		original.subarray(header.end),
	]);

	const base = basename(originalPath);
	const stem = base.replace(/\.jsonl$/, "");
	const filename = oldId && stem.endsWith(`_${oldId}`) ? `${stem.slice(0, -oldId.length)}${newId}.jsonl` : `${stem}_${newId}.jsonl`;
	return { filename, data, oldId, newId, blobHashes: blobHashes(original) };
}

export type ConversationResult =
	| { kind: "none" }
	| { kind: "too-large"; bytes: number }
	| ({ kind: "ok" } & RewrittenConversation);

/** Reads and rewrites the current session file, if it exists on disk and fits the cap. */
export async function loadConversation(sessionFile: string | undefined): Promise<ConversationResult> {
	if (!sessionFile) return { kind: "none" };
	let size: number;
	try {
		size = (await stat(sessionFile)).size;
	} catch {
		return { kind: "none" };
	}
	if (size > MAX_CONVERSATION_BYTES) return { kind: "too-large", bytes: size };
	return { kind: "ok", ...rewriteConversation(await readFile(sessionFile), sessionFile) };
}
