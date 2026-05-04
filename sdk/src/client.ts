import { exec } from "./exec";
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
  CreateSandboxOptions,
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

export class SandboxClient {
  private readonly http: HttpClient;

  constructor(options: SandboxClientOptions) {
    this.http = new HttpClient(options.baseUrl ?? "", options.apiKey);
  }

  // Sandbox lifecycle

  async createSandbox(options?: CreateSandboxOptions): Promise<SandboxRecord> {
    return createSandbox(this.http, options);
  }

  async getSandbox(id: string): Promise<SandboxRecord> {
    return getSandbox(this.http, id);
  }

  async deleteSandbox(id: string): Promise<void> {
    return deleteSandbox(this.http, id);
  }

  // Command execution

  async exec(id: string, request: ExecRequest): Promise<ExecResult> {
    return exec(this.http, id, request);
  }

  // File operations

  async readFile(id: string, path: string): Promise<Buffer> {
    return readFile(this.http, id, path);
  }

  async writeFile(
    id: string,
    path: string,
    content: FileContent,
    overwrite?: boolean,
  ): Promise<void> {
    return writeFile(this.http, id, path, content, overwrite);
  }

  async appendFile(id: string, path: string, content: FileContent): Promise<void> {
    return appendFile(this.http, id, path, content);
  }

  async deleteFile(id: string, path: string, options?: DeleteFileOptions): Promise<void> {
    return deleteFile(this.http, id, path, options);
  }

  async copyFile(id: string, request: CopyFileRequest): Promise<void> {
    return copyFile(this.http, id, request);
  }

  async moveFile(id: string, request: MoveFileRequest): Promise<void> {
    return moveFile(this.http, id, request);
  }

  async mkdir(id: string, path: string, recursive?: boolean): Promise<void> {
    return mkdir(this.http, id, path, recursive);
  }

  async listFiles(id: string, options?: ListFilesOptions): Promise<FileEntry[]> {
    return listFiles(this.http, id, options);
  }

  async stat(id: string, path: string): Promise<FileStat> {
    return stat(this.http, id, path);
  }
}
