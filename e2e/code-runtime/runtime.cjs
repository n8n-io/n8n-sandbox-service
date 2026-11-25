// A minimal code runtime for e2e tests. It is not the n8n runtime.
//
// It speaks the code daemon's control protocol on fd 3 (see
// internal/codedaemon/worker.go). For each task it reads in/task.json, which
// is {"code": "..."}, and runs the code as the body of an async function with
// `require` and `console`. It writes {"result", "console"} to out/result.json.
"use strict";

const fs = require("node:fs");
const net = require("node:net");
const path = require("node:path");
const readline = require("node:readline");

const AsyncFunction = (async () => {}).constructor;
const maxErrorMessage = 4096;

const control = new net.Socket({ fd: 3, readable: true, writable: true });
const send = (message) => control.write(`${JSON.stringify(message)}\n`);

async function runTask(taskId, dir) {
	const lines = [];
	const log = (...args) => lines.push(args.map(String).join(" "));
	const taskConsole = { log, info: log, warn: log, error: log, debug: log };
	const writeResult = (value) =>
		fs.writeFileSync(path.join(dir, "out", "result.json"), JSON.stringify({ result: value, console: lines }));
	try {
		const { code } = JSON.parse(fs.readFileSync(path.join(dir, "in", "task.json"), "utf8"));
		const result = await new AsyncFunction("require", "console", code)(require, taskConsole);
		writeResult(result);
		send({ type: "done", taskId });
	} catch (error) {
		try {
			writeResult(null);
		} catch {
			// The error message below still ends the task.
		}
		const message = String(error && error.message ? error.message : error).slice(0, maxErrorMessage);
		send({ type: "error", taskId, error: { message } });
	}
}

readline.createInterface({ input: control }).on("line", (line) => {
	const message = JSON.parse(line);
	if (message.type === "start") {
		void runTask(message.taskId, message.dir);
	}
});
control.on("close", () => process.exit(0));

send({ type: "ready" });
