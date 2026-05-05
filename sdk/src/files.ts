import type { HttpClient } from "./http";
import type {
  CopyFileRequest,
  DeleteFileOptions,
  FileContent,
  FileEntry,
  FileEntryWireResponse,
  FileStat,
  FileStatWireResponse,
  ListFilesOptions,
  MoveFileRequest,
} from "./types";

export async function readFile(http: HttpClient, id: string, path: string): Promise<Buffer> {
  return http.requestBuffer("GET", `/sandboxes/${id}/files/content`, {
    params: { path },
  });
}

export async function writeFile(
  http: HttpClient,
  id: string,
  path: string,
  content: FileContent,
  overwrite = true,
): Promise<void> {
  await http.requestVoid("PUT", `/sandboxes/${id}/files`, {
    params: { path, overwrite: String(overwrite) },
    headers: { "Content-Type": "application/octet-stream" },
    data: asBuffer(content),
  });
}

export async function appendFile(
  http: HttpClient,
  id: string,
  path: string,
  content: FileContent,
): Promise<void> {
  await http.requestVoid("POST", `/sandboxes/${id}/files`, {
    params: { path },
    headers: { "Content-Type": "application/octet-stream" },
    data: asBuffer(content),
  });
}

export async function deleteFile(
  http: HttpClient,
  id: string,
  path: string,
  options?: DeleteFileOptions,
): Promise<void> {
  await http.requestVoid("DELETE", `/sandboxes/${id}/files`, {
    params: {
      path,
      recursive: String(options?.recursive ?? false),
      force: String(options?.force ?? false),
    },
  });
}

export async function copyFile(
  http: HttpClient,
  id: string,
  request: CopyFileRequest,
): Promise<void> {
  await http.requestVoid("POST", `/sandboxes/${id}/files/copy`, {
    data: {
      src: request.src,
      dest: request.dest,
      recursive: request.recursive ?? false,
      overwrite: request.overwrite ?? false,
    },
  });
}

export async function moveFile(
  http: HttpClient,
  id: string,
  request: MoveFileRequest,
): Promise<void> {
  await http.requestVoid("POST", `/sandboxes/${id}/files/move`, {
    data: {
      src: request.src,
      dest: request.dest,
      overwrite: request.overwrite ?? false,
    },
  });
}

export async function mkdir(
  http: HttpClient,
  id: string,
  path: string,
  recursive = false,
): Promise<void> {
  await http.requestVoid("POST", `/sandboxes/${id}/mkdir`, {
    params: { path, recursive: String(recursive) },
  });
}

export async function listFiles(
  http: HttpClient,
  id: string,
  options: ListFilesOptions = {},
): Promise<FileEntry[]> {
  const params: Record<string, string> = {};
  if (options.path) params.path = options.path;
  if (options.recursive !== undefined) params.recursive = String(options.recursive);
  if (options.extension) params.extension = options.extension;

  const payload = await http.requestJson<FileEntryWireResponse[]>("GET", `/sandboxes/${id}/files`, {
    params,
  });

  return payload.map((entry) => ({
    name: entry.name,
    size: entry.size,
    isDir: entry.is_dir,
    type: entry.type,
    modTime: entry.mod_time,
  }));
}

export async function stat(http: HttpClient, id: string, path: string): Promise<FileStat> {
  const payload = await http.requestJson<FileStatWireResponse>("GET", `/sandboxes/${id}/stat`, {
    params: { path },
  });

  return {
    name: payload.name,
    path: payload.path,
    type: payload.type,
    size: payload.size,
    createdAt: payload.created_at,
    modifiedAt: payload.modified_at,
  };
}

function asBuffer(content: FileContent): Buffer {
  return typeof content === "string" ? Buffer.from(content, "utf-8") : Buffer.from(content);
}
