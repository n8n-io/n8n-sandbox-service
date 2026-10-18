// RPC round trip benchmark of the Task API (code sandboxes), without n8n.
//
// Each task calls helpers.httpRequest CALLS times. The script answers each
// "rpc" event with a fixed result through POST …/tasks/{taskId}/rpc/{callId}.
// The task measures each call (rpcMs). The script measures the result POST.
//
//   (cd sdk && pnpm build)
//   node benchmarks/code-rpc-roundtrip.mjs
//   TASKS=50 CALLS=100 node benchmarks/code-rpc-roundtrip.mjs
//
// The code sandbox image must provide the "n8n-js" runtime (@n8n/code-runtime).
import { CodeSandboxClient } from "../sdk/dist/index.mjs";

const BASE_URL = process.env.BASE_URL || "http://127.0.0.1:8080";
const API_KEY = process.env.API_KEY || "test";
const TASKS = Number(process.env.TASKS || 50);
const CALLS = Number(process.env.CALLS || 1);
const REUSE = (process.env.REUSE_WORKERS || "false") === "true";
const RESULT_BYTES = Number(process.env.RESULT_BYTES || 1024);

const client = new CodeSandboxClient({ baseUrl: BASE_URL, apiKey: API_KEY });
const result = { status: 200, body: "x".repeat(RESULT_BYTES) };

function taskInput(taskId) {
  const code = `
    const rpcMs = [];
    for (let i = 0; i < ${CALLS}; i++) {
      const t0 = performance.now();
      await helpers.httpRequest({ url: 'http://bench/' + i });
      rpcMs.push(performance.now() - t0);
    }
    return { rpcMs };
  `;
  return JSON.stringify({
    taskId,
    settings: { code, nodeMode: "runCode", workflowMode: "manual", continueOnFail: false },
    data: null,
    binaries: [],
  });
}

function stats(name, values) {
  if (values.length === 0) return `${name}: no samples`;
  const sorted = [...values].sort((a, b) => a - b);
  const at = (p) => sorted[Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))];
  const avg = sorted.reduce((a, b) => a + b, 0) / sorted.length;
  const f = (v) => v.toFixed(2);
  return `${name}: n=${sorted.length} avg=${f(avg)} p50=${f(at(50))} p95=${f(at(95))} p99=${f(at(99))} max=${f(sorted[sorted.length - 1])} ms`;
}

const sandbox = await client.createSandbox({
  runtime: "n8n-js",
  slots: 1,
  taskMemoryMb: 256,
  spareWorkers: true,
  reuseWorkers: REUSE,
  labels: { bench: "rpc-roundtrip" },
});
const rpcMs = [];
const postMs = [];
const taskMs = [];
let errors = 0;
try {
  for (let i = 0; i < TASKS; i++) {
    const taskId = `rpc_${process.pid}_${i}`;
    const t0 = performance.now();
    const terminal = await client.runTask(
      sandbox.id,
      { taskId, timeoutMs: 60_000, inlineResultMaxBytes: 1024 * 1024 },
      [{ path: "task.json", content: taskInput(taskId) }],
      {
        onEvent: (ev) => {
          if (ev.type !== "rpc") return;
          const p0 = performance.now();
          client
            .postRpcResult(sandbox.id, taskId, ev.callId, { ok: true, data: result })
            .then(() => postMs.push(performance.now() - p0))
            .catch((e) => {
              errors++;
              console.error(`rpc result failed: ${e.message}`);
            });
        },
      },
    );
    taskMs.push(performance.now() - t0);
    if (terminal.type !== "done") {
      errors++;
      console.error(`task ${taskId} failed: ${JSON.stringify(terminal)}`);
    } else {
      rpcMs.push(...(terminal.result?.result?.rpcMs ?? []));
    }
    await client.deleteTask(sandbox.id, taskId).catch(() => {});
  }
} finally {
  await client.deleteSandbox(sandbox.id);
}

console.log(`tasks=${TASKS} calls/task=${CALLS} reuseWorkers=${REUSE} resultBytes=${RESULT_BYTES} errors=${errors}`);
console.log(stats("rpc round trip (in task)", rpcMs));
console.log(stats("result POST (client)", postMs));
console.log(stats("task total (client)", taskMs));
