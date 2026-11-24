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
  state: string;
}

/** The `spec` part of a code task. The daemon defines the fields. */
export interface CodeTaskSpec {
  taskId: string;
  [field: string]: unknown;
}

/** A binary input file of a code task. */
export interface CodeTaskInput {
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
  inputs?: CodeTaskInput[];
  /** Aborts the upload and closes the event stream. */
  signal?: AbortSignal;
}

/** An event of a code task. The daemon defines the types and fields. */
export interface CodeTaskEvent {
  type: string;
  [field: string]: unknown;
}
