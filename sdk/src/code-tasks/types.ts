import type { Readable } from "node:stream";
import type { FileContent } from "../types";

/** Runtime of a code sandbox. */
export type CodeRuntime = "n8n-js" | "n8n-python";

/** Options for creating a code sandbox. */
export interface CreateCodeSandboxOptions {
  /**
   * Chosen by the caller: 1 to 128 characters from `A-Z`, `a-z`, `0-9`, `.`,
   * `_`, `:` and `-`. Creates of one name return the same sandbox while its
   * settings are equal; other settings fail with `409`.
   */
  name: string;
  runtime: CodeRuntime;
  /**
   * Version of the runtime image. Should match the n8n version. 1 to 64
   * characters from `A-Z`, `a-z`, `0-9`, `.`, `+` and `-`.
   */
  runtimeVersion: string;
  /** Number of lanes: each lane runs one task at a time. */
  maxConcurrency: number;
  /** Total sandbox memory in MB. */
  memoryMb: number;
  idleTimeoutMs?: number;
  maxAgeMs?: number;
}

/** A code sandbox, as returned by `createCodeSandbox`. */
export interface CodeSandbox {
  id: string;
  name: string;
  /** True only for the create that made the sandbox. */
  created: boolean;
  state: "ready";
}

/**
 * Settings of one task run, sent as the first part of the request. The daemon
 * reads them before the inputs, and refuses the task if they are not valid.
 * Encoded as JSON, at most 1 KiB.
 */
export interface CodeTaskSpec {
  /**
   * Identifies the task in its events. 1 to 128 characters from `A-Z`, `a-z`,
   * `0-9`, `_` and `-`. Use a new ID for each task: the sandbox refuses an ID
   * it already started with `409`.
   */
  taskId: string;
  /**
   * Maximum size in bytes of the `result.json` that the events include as
   * `result`. Default and maximum 1 MiB.
   */
  inlineResultMaxBytes?: number;
}

/** A binary input file of a code task. */
export interface CodeTaskBinaryInput {
  /** Relative path of the file in the task, such as `bin/0`. Must not be `task.json`. */
  path: string;
  /** File content. A stream is sent as it is read, without buffering. */
  content: FileContent | Readable;
  /** MIME type of the content, such as `image/png`. Default `application/octet-stream`. */
  contentType?: string;
}

/** Options for running a code task. */
export interface RunCodeTaskOptions {
  spec: CodeTaskSpec;
  /**
   * JSON content of the task, sent as the `task.json` input. A stream is sent
   * as it is read, without buffering.
   */
  taskInput: FileContent | Readable;
  /** Binary input files of the task. */
  inputs?: CodeTaskBinaryInput[];
  /** Aborts the upload and closes the event stream. */
  signal?: AbortSignal;
}

/** The runtime received the task. */
export interface CodeTaskAcceptedEvent {
  type: "accepted";
  taskId: string;
}

/** The task still runs. Sent every 5 seconds. */
export interface CodeTaskHeartbeatEvent {
  type: "heartbeat";
  taskId: string;
}

/** An output file of a task that completed. */
export interface CodeTaskOutput {
  /** Path relative to the task's output directory. */
  path: string;
  /** Size in bytes. */
  size: number;
}

/** Terminal event of a task that completed. */
export interface CodeTaskDoneEvent {
  type: "done";
  taskId: string;
  /** Content of `result.json`, or null when the file is missing or larger than `inlineResultMaxBytes`. */
  result: unknown;
  /** Output files of the task. Empty when there are none. */
  outputs: CodeTaskOutput[];
}

/**
 * Why a task failed:
 * - `user_error`: the task code failed.
 * - `invalid`: the outputs or `result.json` are not valid, or are too large.
 * - `crashed`: the runtime is not available or stopped.
 */
export type CodeTaskErrorCode = "user_error" | "invalid" | "crashed";

/** Terminal event of a task that failed. */
export interface CodeTaskErrorEvent {
  type: "error";
  taskId: string;
  code: CodeTaskErrorCode;
  message: string;
  /** The error from the runtime. Only for `user_error`. */
  userError?: unknown;
  /** Content of `result.json`, when it is valid and small enough. Only for `user_error`. */
  result?: unknown;
}

/** An event of a code task. The last event is `done` or `error`. */
export type CodeTaskEvent =
  | CodeTaskAcceptedEvent
  | CodeTaskHeartbeatEvent
  | CodeTaskDoneEvent
  | CodeTaskErrorEvent;

/** The type of each code task event. */
export type CodeTaskEventType = CodeTaskEvent["type"];
