import http from "k6/http";
import { check, fail } from "k6";
import { Trend } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://127.0.0.1:8080";
const API_KEY = __ENV.API_KEY || "test";

const createDuration = new Trend("sandbox_create_duration", true);
const execDuration = new Trend("sandbox_exec_duration", true);
const deleteDuration = new Trend("sandbox_delete_duration", true);

export const options = {
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

const headers = {
  "Content-Type": "application/json",
  "X-Api-Key": API_KEY,
};

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

  // 2. Execute echo 'hello'
  const execPayload = JSON.stringify({ command: "echo 'hello'" });
  const execRes = http.post(
    `${BASE_URL}/sandboxes/${sandboxId}/executions`,
    execPayload,
    { headers },
  );
  execDuration.add(execRes.timings.duration);

  if (
    !check(execRes, {
      "exec status is 200": (r) => r.status === 200,
      "exec completed successfully": (r) => {
        const lines = r.body.trim().split("\n");
        const last = JSON.parse(lines[lines.length - 1]);
        return last.type === "exit" && last.exit_code === 0;
      },
    })
  ) {
    fail(`exec failed: ${execRes.status} ${execRes.body}`);
  }

  // 3. Delete sandbox
  const deleteRes = http.del(`${BASE_URL}/sandboxes/${sandboxId}`, null, {
    headers,
  });
  deleteDuration.add(deleteRes.timings.duration);

  check(deleteRes, {
    "delete status is 204": (r) => r.status === 204,
  });
}
