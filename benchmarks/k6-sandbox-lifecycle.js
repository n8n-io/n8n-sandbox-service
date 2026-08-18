import http from "k6/http";
import { check, fail, sleep } from "k6";
import { Trend } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://127.0.0.1:8080";
const API_KEY = __ENV.API_KEY || "test";
const SCENARIO = __ENV.SCENARIO || "load";
const ITERATIONS = Number(__ENV.ITERATIONS || 30);
// Seconds to idle after the first exec so the API's idle sweeper stops the
// sandbox, which makes the second exec a wake. 0 skips the wake step.
const WAKE_AFTER = Number(__ENV.WAKE_AFTER || 0);

const createDuration = new Trend("sandbox_create_duration", true);
const execDuration = new Trend("sandbox_exec_duration", true);
const wakeExecDuration = new Trend("sandbox_wake_exec_duration", true);
const deleteDuration = new Trend("sandbox_delete_duration", true);

// The baseline scenario measures one operation at a time, which is what
// docs/performance.md records. The load scenario is the concurrent run.
const baselineOptions = {
  vus: 1,
  iterations: ITERATIONS,
  thresholds: {
    http_req_failed: ["rate<0.01"],
  },
};

const loadOptions = {
  stages: [
    { duration: "30s", target: 50 },
    { duration: "1m", target: 50 },
    { duration: "10s", target: 0 },
  ],
  thresholds: {
    http_req_failed: ["rate<0.05"],
    iteration_duration: ["p(95)<30000"],
  },
};

export const options = SCENARIO === "baseline" ? baselineOptions : loadOptions;

const headers = {
  "Content-Type": "application/json",
  "X-Api-Key": API_KEY,
};

function exec(sandboxId) {
  const payload = JSON.stringify({ command: "echo 'hello'" });
  const res = http.post(
    `${BASE_URL}/sandboxes/${sandboxId}/executions`,
    payload,
    { headers },
  );

  if (
    !check(res, {
      "exec status is 200": (r) => r.status === 200,
      "exec completed successfully": (r) => {
        const lines = r.body.trim().split("\n");
        const last = JSON.parse(lines[lines.length - 1]);
        return last.type === "exit" && last.exit_code === 0;
      },
    })
  ) {
    fail(`exec failed: ${res.status} ${res.body}`);
  }
  return res;
}

export default function () {
  // 1. Create sandbox
  const createRes = http.post(`${BASE_URL}/sandboxes`, null, { headers });
  createDuration.add(createRes.timings.duration);

  if (
    !check(createRes, {
      "create status is 201": (r) => r.status === 201,
    })
  ) {
    fail(`create failed: ${createRes.status} ${createRes.body}`);
  }

  const sandboxId = createRes.json().id;

  // 2. Execute echo 'hello' on the running sandbox
  execDuration.add(exec(sandboxId).timings.duration);

  // 3. Optionally let the sandbox go idle, then exec again to measure a wake
  if (WAKE_AFTER > 0) {
    sleep(WAKE_AFTER);
    wakeExecDuration.add(exec(sandboxId).timings.duration);
  }

  // 4. Delete sandbox
  const deleteRes = http.del(`${BASE_URL}/sandboxes/${sandboxId}`, null, {
    headers,
  });
  deleteDuration.add(deleteRes.timings.duration);

  check(deleteRes, {
    "delete status is 204": (r) => r.status === 204,
  });
}
