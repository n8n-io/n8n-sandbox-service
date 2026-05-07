/** Configuration for creating a SandboxClient instance. */
export interface SandboxClientOptions {
  /** API key for authenticating with the sandbox service. */
  apiKey?: string;
  /** Base URL of the sandbox service API. */
  baseUrl?: string;
  /** Optional retry policy for transient request failures. */
  retry?: RetryOptions;
}

/** Retry policy for transient HTTP failures. */
export interface RetryOptions {
  /**
   * Number of retry attempts after the initial request.
   * Example: 3 means at most 4 total attempts.
   */
  attempts?: number;
  /** Initial backoff delay in milliseconds. Defaults to 200. */
  baseDelayMs?: number;
  /** Maximum backoff delay in milliseconds. Defaults to 2000. */
  maxDelayMs?: number;
  /**
   * HTTP statuses that should be retried. Defaults to [429, 502, 503].
   * Network/transport errors (status 0) are always considered retryable.
   */
  retryOnStatuses?: number[];
  /** Enable delay jitter (0.5x..1.5x). Defaults to true. */
  jitter?: boolean;
}

/** Metadata for a sandbox instance. */
export interface SandboxRecord {
  id: string;
  status: string;
  /** Unix timestamp (seconds) when the sandbox was created. */
  createdAt: number;
  /** Unix timestamp (seconds) of last activity. */
  lastActiveAt: number;
}

/** Directory entry returned by the file listing API. */
export interface FileEntry {
  name: string;
  size: number;
  isDir: boolean;
  type: "file" | "directory";
  /** ISO 8601 last-modified timestamp. */
  modTime: string;
}

/** File or directory metadata returned by the stat API. */
export interface FileStat {
  name: string;
  path: string;
  type: "file" | "directory";
  size: number;
  /** ISO 8601 creation timestamp. */
  createdAt: string;
  /** ISO 8601 last-modified timestamp. */
  modifiedAt: string;
}

/** Request payload for executing a command in a sandbox. */
export interface ExecRequest {
  /** Shell command to execute (run via `/bin/sh -c`). */
  command: string;
  /** Environment variables to set for the command. */
  env?: Record<string, string | undefined>;
  /** Working directory for the command. */
  workdir?: string;
  /** Maximum execution time in milliseconds. Defaults to 5 minutes. */
  timeoutMs?: number;
  /** Signal to abort the running command. */
  abortSignal?: AbortSignal;
  /** Called with each chunk of stdout data as it arrives. */
  onStdout?: (data: string) => void;
  /** Called with each chunk of stderr data as it arrives. */
  onStderr?: (data: string) => void;
}

/** Aggregated result of a completed command execution. */
export interface ExecResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  /** Wall-clock execution time in milliseconds. */
  executionTimeMs: number;
  /** Whether the command was killed due to timeout. */
  timedOut: boolean;
  /** Whether the command was terminated by a signal. */
  killed: boolean;
  /** True when exitCode is 0. */
  success: boolean;
}

/** Accepted content types for file write operations. */
export type FileContent = string | Buffer | Uint8Array;

/** Options for listing files in a sandbox directory. */
export interface ListFilesOptions {
  /** Directory path to list. Defaults to root. */
  path?: string;
  /** List files recursively. */
  recursive?: boolean;
  /** Filter by file extension (e.g. `.ts`). */
  extension?: string;
}

/** Request payload for copying a file or directory. */
export interface CopyFileRequest {
  src: string;
  dest: string;
  recursive?: boolean;
  overwrite?: boolean;
}

/** Request payload for moving or renaming a file or directory. */
export interface MoveFileRequest {
  src: string;
  dest: string;
  overwrite?: boolean;
}

/** Options for deleting a file or directory. */
export interface DeleteFileOptions {
  /** Remove non-empty directories. */
  recursive?: boolean;
  /** Ignore "not found" errors. */
  force?: boolean;
}

// Wire-format types (snake_case from the API)

export type SandboxWireResponse = {
  id: string;
  status: string;
  created_at: number;
  last_active_at: number;
};

export type FileEntryWireResponse = {
  name: string;
  size: number;
  is_dir: boolean;
  type: "file" | "directory";
  mod_time: string;
};

export type FileStatWireResponse = {
  name: string;
  path: string;
  type: "file" | "directory";
  size: number;
  created_at: string;
  modified_at: string;
};

/** Streamed stdout chunk from exec. */
export type ExecStdoutEvent = { type: "stdout"; data: string };
/** Streamed stderr chunk from exec. */
export type ExecStderrEvent = { type: "stderr"; data: string };
/** Final exit event from exec with process metadata. */
export type ExecExitEvent = {
  type: "exit";
  exit_code: number;
  success: boolean;
  execution_time_ms: number;
  timed_out: boolean;
  killed: boolean;
};
/** Error event from exec indicating a stream-level failure. */
export type ExecErrorEvent = { type: "error"; error: string };
/** Discriminated union of all NDJSON exec stream events. */
export type ExecEvent = ExecStdoutEvent | ExecStderrEvent | ExecExitEvent | ExecErrorEvent;
