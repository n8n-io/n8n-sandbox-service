// Direct benchmark of the Task API (code sandboxes), without n8n.
// SPIKE-SPEC-opus.md, section 10.6.
//
//   k6 run -e SCENARIO=create benchmarks/k6-code-task-api.js           # one at a time
//   k6 run -e SCENARIO=create -e BURST=10 benchmarks/k6-code-task-api.js
//   k6 run -e SCENARIO=task benchmarks/k6-code-task-api.js             # 1 KB in, 1 KB out
//
// k6 cannot answer an "rpc" event while the task stream is open. For the RPC
// round trip, use benchmarks/code-rpc-roundtrip.mjs.
//
// The code sandbox image must provide the "n8n-js" runtime (@n8n/code-runtime).
import http from "k6/http";
import { check, fail, sleep } from "k6";
import { Trend, Counter } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://127.0.0.1:8080";
const API_KEY = __ENV.API_KEY || "test";
const SCENARIO = __ENV.SCENARIO || "task";
const ITERATIONS = Number(__ENV.ITERATIONS || 50);
const BURST = Number(__ENV.BURST || 1);
const SPARE = (__ENV.SPARE_WORKERS || "true") === "true";
const REUSE = (__ENV.REUSE_WORKERS || "false") === "true";
// Seconds between tasks. With 0, each task waits for the worker of the last one to load.
const THINK = Number(__ENV.THINK || 0);

const createDuration = new Trend("code_create_duration", true);
const createRunner = new Trend("code_create_runner", true);
const createSpareReady = new Trend("code_create_spare_ready", true);
// A create that the API served from a warm pool (SANDBOX_CODE_POOLS).
const createPooledDuration = new Trend("code_create_pooled_duration", true);
const createUnpooledDuration = new Trend("code_create_unpooled_duration", true);
const createClaim = new Trend("code_create_claim", true);
const createPooled = new Counter("code_create_pooled");
const createUnpooled = new Counter("code_create_unpooled");
const deleteDuration = new Trend("code_delete_duration", true);
const taskDuration = new Trend("code_task_duration", true);
const taskAccepted = new Trend("code_task_time_to_accepted", true);
const taskRun = new Trend("code_task_worker_run", true);
const taskHandoff = new Trend("code_task_handoff", true);
const taskErrors = new Counter("code_task_errors");

const headers = { "X-Api-Key": API_KEY };
const payload = "x".repeat(1024);

export const options =
  SCENARIO === "create"
    ? {
        scenarios: {
          default: {
            executor: "per-vu-iterations",
            vus: BURST,
            iterations: BURST === 1 ? ITERATIONS : Math.max(1, Math.floor(ITERATIONS / BURST)),
            maxDuration: "30m",
          },
        },
      }
    : {
        scenarios: {
          default: { executor: "shared-iterations", vus: 1, iterations: ITERATIONS, maxDuration: "30m" },
        },
      };

function createSandbox(slots) {
  const res = http.post(
    `${BASE_URL}/v1/code/sandboxes`,
    JSON.stringify({ runtime: "n8n-js", slots, taskMemoryMb: 256, spareWorkers: SPARE, reuseWorkers: REUSE, labels: { bench: SCENARIO } }),
    { headers: { ...headers, "Content-Type": "application/json" }, timeout: "180s" },
  );
  if (!check(res, { "create 201": (r) => r.status === 201 })) fail(`create failed: ${res.status} ${res.body}`);
  const body = res.json();
  createDuration.add(res.timings.duration);
  if (body.pooled) {
    createPooled.add(1);
    createPooledDuration.add(res.timings.duration);
    // The API omits a claim time of 0 ms.
    createClaim.add(body.timings.claim ?? 0);
  } else {
    createUnpooled.add(1);
    createUnpooledDuration.add(res.timings.duration);
    createRunner.add(body.timings.runnerCreate);
    createSpareReady.add(body.timings.spareReady);
  }
  return body.id;
}

function deleteSandbox(id) {
  const res = http.del(`${BASE_URL}/v1/code/sandboxes/${id}`, null, { headers });
  deleteDuration.add(res.timings.duration);
}

let taskSeq = 0;

function taskBody(code, additionalProperties) {
  const taskId = `k6_${__VU}_${__ITER}_${taskSeq++}`;
  const boundary = "----k6code" + taskId;
  const spec = JSON.stringify({ taskId, timeoutMs: 30000, inlineResultMaxBytes: 1048576 });
  const task = JSON.stringify({
    taskId,
    settings: { code, nodeMode: "runCode", workflowMode: "manual", continueOnFail: false, additionalProperties },
    data: null,
    binaries: [],
  });
  const body =
    `--${boundary}\r\nContent-Disposition: form-data; name="spec"\r\nContent-Type: application/json\r\n\r\n${spec}\r\n` +
    `--${boundary}\r\nContent-Disposition: form-data; name="input"; filename="task.json"\r\nContent-Type: application/octet-stream\r\n\r\n${task}\r\n` +
    `--${boundary}--\r\n`;
  return { taskId, body, contentType: `multipart/form-data; boundary=${boundary}` };
}

function recordTask(res) {
  taskDuration.add(res.timings.duration);
  const events = String(res.body)
    .trim()
    .split("\n")
    .filter((l) => l)
    .map((l) => JSON.parse(l));
  const terminal = events[events.length - 1];
  if (!terminal || terminal.type !== "done") {
    taskErrors.add(1);
    console.error(`task failed: ${res.status} ${res.body}`);
    return null;
  }
  if (terminal.timings) {
    taskHandoff.add(terminal.timings.handoff);
    if (terminal.timings.worker) taskRun.add(terminal.timings.worker.run);
  }
  return terminal;
}

export function setup() {
  if (SCENARIO === "create") return {};
  return { id: createSandbox(1) };
}

export function teardown(data) {
  if (data.id) deleteSandbox(data.id);
}

export default async function (data) {
  if (SCENARIO === "create") {
    deleteSandbox(createSandbox(2));
    if (THINK > 0) sleep(THINK);
    return;
  }

  if (SCENARIO === "task") {
    const t = taskBody("return payload", { payload });
    const res = http.post(`${BASE_URL}/v1/code/sandboxes/${data.id}/tasks`, t.body, {
      headers: { ...headers, "Content-Type": t.contentType },
    });
    const done = recordTask(res);
    check(done, { "1 KB result": (d) => d && d.result && d.result.result.length === 1024 });
    http.del(`${BASE_URL}/v1/code/sandboxes/${data.id}/tasks/${t.taskId}`, null, { headers });
    if (THINK > 0) sleep(THINK);
    return;
  }
}
