import { exec, resumeExecution, deleteExecution } from "./exec";
import {
  readFile,
  writeFile,
  appendFile,
  deleteFile,
  copyFile,
  moveFile,
  mkdir,
  listFiles,
  stat,
} from "./files";
import { HttpClient } from "./http";
import { createSandbox, getSandbox, deleteSandbox } from "./sandboxes";
import type {
  CopyFileRequest,
  DeleteFileOptions,
  ExecRequest,
  ExecResult,
  FileContent,
  FileEntry,
  FileStat,
  ListFilesOptions,
  MoveFileRequest,
  SandboxClientOptions,
  SandboxRecord,
} from "./types";

/**
 * High-level client for interacting with the sandbox service HTTP API.
 */
export class SandboxClient {
  private readonly http: HttpClient;

  /**
   * Creates a sandbox service client.
   */
  constructor(options: SandboxClientOptions) {
    this.http = new HttpClient(options.baseUrl ?? "", options.apiKey, options.retry);
  }

  // #region Sandbox lifecycle

  /**
   * Creates a new sandbox.
   */
  async createSandbox(): Promise<SandboxRecord> {
    return createSandbox(this.http);
  }

  /**
   * Fetches a sandbox by ID.
   */
  async getSandbox(id: string): Promise<SandboxRecord> {
    return getSandbox(this.http, id);
  }

  /**
   * Deletes a sandbox by ID.
   */
  async deleteSandbox(id: string): Promise<void> {
    return deleteSandbox(this.http, id);
  }

  // #endregion
  // #region Command execution

  /**
   * Executes a command inside a sandbox.
   */
  async exec(id: string, request: ExecRequest): Promise<ExecResult> {
    return exec(this.http, id, request);
  }

  /**
   * Resumes or replays an execution, returning the aggregated result.
   */
  async resumeExecution(sandboxId: string, execId: string, afterSeq?: number): Promise<ExecResult> {
    return resumeExecution(this.http, sandboxId, execId, afterSeq);
  }

  /**
   * Cancels and deletes an execution.
   */
  async deleteExecution(sandboxId: string, execId: string): Promise<void> {
    return deleteExecution(this.http, sandboxId, execId);
  }

  // #endregion
  // #region File operations

  /**
   * Reads a file from a sandbox.
   */
  async readFile(id: string, path: string): Promise<Buffer> {
    return readFile(this.http, id, path);
  }

  /**
   * Writes a file into a sandbox.
   */
  async writeFile(
    id: string,
    path: string,
    content: FileContent,
    overwrite?: boolean,
  ): Promise<void> {
    return writeFile(this.http, id, path, content, overwrite);
  }

  /**
   * Appends content to a file in a sandbox.
   */
  async appendFile(id: string, path: string, content: FileContent): Promise<void> {
    return appendFile(this.http, id, path, content);
  }

  /**
   * Deletes a file or directory from a sandbox.
   */
  async deleteFile(id: string, path: string, options?: DeleteFileOptions): Promise<void> {
    return deleteFile(this.http, id, path, options);
  }

  /**
   * Copies a file or directory inside a sandbox.
   */
  async copyFile(id: string, request: CopyFileRequest): Promise<void> {
    return copyFile(this.http, id, request);
  }

  /**
   * Moves or renames a file or directory inside a sandbox.
   */
  async moveFile(id: string, request: MoveFileRequest): Promise<void> {
    return moveFile(this.http, id, request);
  }

  /**
   * Creates a directory inside a sandbox.
   */
  async mkdir(id: string, path: string, recursive?: boolean): Promise<void> {
    return mkdir(this.http, id, path, recursive);
  }

  /**
   * Lists files in a sandbox directory.
   */
  async listFiles(id: string, options?: ListFilesOptions): Promise<FileEntry[]> {
    return listFiles(this.http, id, options);
  }

  /**
   * Returns metadata for a sandbox file or directory.
   */
  async stat(id: string, path: string): Promise<FileStat> {
    return stat(this.http, id, path);
  }

  // #endregion
}
