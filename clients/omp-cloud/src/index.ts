import { fileURLToPath } from "node:url";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { Cloud } from "./verbs";

/** This extension's own directory (the package root), which is never shipped to the cloud. */
const OWN_ROOT = fileURLToPath(new URL("..", import.meta.url));

export default function ompCloud(pi: ExtensionAPI): void {
	const cloud = new Cloud(pi, OWN_ROOT);

	// `/cloud` is the only entry point; there is deliberately no input-prefix trigger.
	pi.registerCommand("cloud", {
		description: "Hand work to a cloud agent and manage cloud threads",
		getArgumentCompletions: prefix => cloud.complete(prefix),
		handler: (args, ctx) => cloud.handle(args, ctx),
	});

	// switchSession replaces the session's context (and clears its timers); re-arm the watcher.
	pi.on("session_switch", (_event, ctx) => {
		cloud.rebind(ctx);
	});
}
