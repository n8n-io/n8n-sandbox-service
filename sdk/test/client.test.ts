import { describe, expect, it, vi, beforeEach } from "vitest";
import { SandboxClient } from "../src/client.js";
import {
  CreateOptionMismatchError,
  EgressMismatchError,
  SandboxServiceError,
} from "../src/errors.js";
import { HttpClient } from "../src/http.js";

vi.mock("../src/http.js", () => {
  const MockHttpClient = vi.fn(function (this: Record<string, unknown>) {
    this.requestJson = vi.fn();
    this.requestVoid = vi.fn();
    this.requestBuffer = vi.fn();
    this.requestStream = vi.fn();
  });
  return { HttpClient: MockHttpClient };
});

function getMockHttp(client: SandboxClient): {
  requestJson: ReturnType<typeof vi.fn>;
  requestVoid: ReturnType<typeof vi.fn>;
  requestBuffer: ReturnType<typeof vi.fn>;
} {
  return (client as unknown as { http: Record<string, ReturnType<typeof vi.fn>> }).http;
}

describe("SandboxClient", () => {
  let client: SandboxClient;

  beforeEach(() => {
    vi.clearAllMocks();
    client = new SandboxClient({ baseUrl: "http://localhost:8080", apiKey: "test-key" });
  });

  it("constructs HttpClient with options", () => {
    expect(HttpClient).toHaveBeenCalledWith("http://localhost:8080", "test-key", undefined);
  });

  it("passes retry options to HttpClient", () => {
    new SandboxClient({
      baseUrl: "http://localhost:8080",
      apiKey: "test-key",
      retry: { attempts: 3, baseDelayMs: 50, jitter: false },
    });
    expect(HttpClient).toHaveBeenLastCalledWith(
      "http://localhost:8080",
      "test-key",
      expect.objectContaining({ attempts: 3, baseDelayMs: 50, jitter: false }),
    );
  });

  it("createSandbox sends POST /sandboxes", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
      ephemeral: false,
      egress: "public",
    });

    const result = await client.createSandbox({ egress: "public" });

    // Only the egress mode goes on the wire; an anonymous create is never retry-safe.
    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes", {
      data: { egress: "public" },
      isSafeToRetry: false,
    });
    expect(result).toEqual({
      id: "abc",
      status: "running",
      createdAt: 1000,
      lastActiveAt: 1000,
      ephemeral: false,
      egress: "public",
    });
  });

  it("createSandbox with egress none sends the mode and maps it back", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
      ephemeral: false,
      egress: "none",
    });

    const result = await client.createSandbox({ egress: "none" });

    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes", {
      data: { egress: "none" },
      isSafeToRetry: false,
    });
    expect(result.egress).toBe("none");
  });

  it("createSandbox with ephemeral sends the flag without marking the POST retry-safe", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
      ephemeral: true,
      egress: "public",
    });

    const result = await client.createSandbox({ ephemeral: true, egress: "public" });

    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes", {
      data: { ephemeral: true, egress: "public" },
      isSafeToRetry: false,
    });
    expect(result.ephemeral).toBe(true);
  });

  it("createSandbox with id and ephemeral stays retry-safe", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "11111111-1111-4111-8111-111111111111",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
      ephemeral: true,
      egress: "public",
    });

    await client.createSandbox({
      id: "11111111-1111-4111-8111-111111111111",
      ephemeral: true,
      egress: "public",
    });

    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes", {
      data: { id: "11111111-1111-4111-8111-111111111111", ephemeral: true, egress: "public" },
      isSafeToRetry: true,
    });
  });

  it("createSandbox rejects a sandbox whose egress is not the one requested", async () => {
    const mock = getMockHttp(client);
    const record = { id: "abc", status: "running", created_at: 1000, last_active_at: 1000 };

    // No mode reported (old API): only public can be trusted.
    mock.requestJson.mockResolvedValue(record);
    await expect(client.createSandbox({ egress: "public" })).resolves.toMatchObject({
      egress: "public",
    });
    const err = await client.createSandbox({ egress: "none" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(EgressMismatchError);
    expect(err).toMatchObject({ sandboxId: "abc", requested: "none", actual: "public" });

    // An API that answers a reconnect with the sandbox's own, different mode.
    mock.requestJson.mockResolvedValue({ ...record, egress: "none" });
    await expect(client.createSandbox({ id: "abc", egress: "public" })).rejects.toBeInstanceOf(
      EgressMismatchError,
    );
  });

  it("updateEgressProxy and model egress use their routes", async () => {
    const mock = getMockHttp(client);
    const spec = { allow: [{ name: "git", hosts: ["git.example.com"] }] };
    await client.updateEgressProxy("abc", spec);
    expect(mock.requestVoid).toHaveBeenCalledWith("PUT", "/sandboxes/abc/egress-proxy", {
      data: spec,
      isSafeToRetry: true,
    });
    mock.requestJson.mockResolvedValue({ blocked: true });
    await expect(client.setModelEgress("abc", true)).resolves.toBe(true);
    expect(mock.requestJson).toHaveBeenCalledWith("PUT", "/sandboxes/abc/model-egress", {
      data: { blocked: true },
      isSafeToRetry: true,
    });
    await expect(client.getModelEgress("abc")).resolves.toBe(true);
  });

  it("archiveSandbox and restoreSandbox use their routes and map archive fields", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "archived",
      created_at: 1,
      last_active_at: 1,
      ephemeral: false,
      egress: "public",
      template: "default",
      idle_archive_after: 7200,
      archived_at: 99,
    });
    await expect(client.archiveSandbox("abc")).resolves.toMatchObject({
      status: "archived",
      archivedAt: 99,
      idleArchiveAfter: 7200,
    });
    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes/abc/archive", {
      isSafeToRetry: true,
    });
    await client.restoreSandbox("abc");
    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes/abc/restore", {
      isSafeToRetry: true,
    });
  });

  it("listTemplates maps the catalog", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      templates: [{ name: "python", vcpus: 4, memory_mb: 8192, disk_mb: 20480, runners: 1 }],
    });
    await expect(client.listTemplates()).resolves.toEqual([
      { name: "python", vcpus: 4, memoryMb: 8192, diskMb: 20480, runners: 1 },
    ]);
    expect(mock.requestJson).toHaveBeenCalledWith("GET", "/templates", { isSafeToRetry: true });
  });

  it("updateIdleWindows sends PATCH in wire form, null clearing a window", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "stopped",
      created_at: 1000,
      last_active_at: 1000,
      ephemeral: false,
      egress: "public",
      template: "default",
      idle_delete_after: 604800,
    });

    const result = await client.updateIdleWindows("abc", {
      idleStopAfter: null,
      idleDeleteAfter: 604800,
    });

    expect(mock.requestJson).toHaveBeenCalledWith("PATCH", "/sandboxes/abc", {
      data: { idle_stop_after: null, idle_delete_after: 604800 },
      isSafeToRetry: true,
    });
    expect(result).toMatchObject({ id: "abc", idleDeleteAfter: 604800 });
    expect(result.idleStopAfter).toBeUndefined();
  });

  it("createSandbox sends template, idle windows and the proxy spec in wire form", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
      ephemeral: false,
      egress: "proxy",
      template: "default",
      idle_stop_after: 1800,
      idle_delete_after: 604800,
    });
    const egressProxy = {
      allow: [{ name: "git", hosts: ["git.example.com"] }],
      repos: [{ host: "git.example.com", path: "org/repo", access: "write" as const }],
      push_refs: ["refs/heads/factory/i1/"],
    };

    const result = await client.createSandbox({
      egress: "proxy",
      egressProxy,
      template: "default",
      idleStopAfter: 1800,
      idleDeleteAfter: 604800,
    });

    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes", {
      data: {
        egress: "proxy",
        egress_proxy: egressProxy,
        template: "default",
        idle_stop_after: 1800,
        idle_delete_after: 604800,
      },
      isSafeToRetry: false,
    });
    expect(result).toMatchObject({
      template: "default",
      idleStopAfter: 1800,
      idleDeleteAfter: 604800,
    });
  });

  it("createSandbox rejects a sandbox an older API created without the requested options", async () => {
    const mock = getMockHttp(client);
    // An API from before templates and idle windows reports neither.
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
      egress: "public",
    });

    await expect(
      client.createSandbox({ egress: "public", template: "default" }),
    ).resolves.toMatchObject({ id: "abc" });
    const err = await client
      .createSandbox({ egress: "public", template: "python" })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(CreateOptionMismatchError);
    expect(err).toMatchObject({
      sandboxId: "abc",
      option: "template",
      requested: "python",
      actual: "default",
    });
    await expect(
      client.createSandbox({ egress: "public", idleDeleteAfter: 604800 }),
    ).rejects.toBeInstanceOf(CreateOptionMismatchError);
  });

  it("maps missing ephemeral and egress fields to their pre-flag values", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
    });

    const result = await client.getSandbox("abc");

    expect(result.ephemeral).toBe(false);
    expect(result.egress).toBe("public");
  });

  it("creates or reuses a caller-supplied sandbox ID", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "11111111-1111-4111-8111-111111111111",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
      ephemeral: false,
      egress: "public",
    });

    const result = await client.createSandbox({
      id: "11111111-1111-4111-8111-111111111111",
      egress: "public",
    });

    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes", {
      data: { id: "11111111-1111-4111-8111-111111111111", egress: "public" },
      isSafeToRetry: true,
    });
    expect(result.id).toBe("11111111-1111-4111-8111-111111111111");
  });

  it("getSandbox sends GET /sandboxes/{id}", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "xyz",
      status: "running",
      created_at: 2000,
      last_active_at: 2000,
      ephemeral: false,
    });

    const result = await client.getSandbox("xyz");

    expect(mock.requestJson).toHaveBeenCalledWith("GET", "/sandboxes/xyz");
    expect(result.id).toBe("xyz");
    // A service from before ownership was reported leaves the field out entirely.
    expect("tenantId" in result).toBe(false);
  });

  it("maps tenant_id to tenantId when the service reports it", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "xyz",
      status: "running",
      created_at: 2000,
      last_active_at: 2000,
      ephemeral: false,
      egress: "public",
      tenant_id: "0f0e0d0c-0000-4000-8000-000000000001",
    });

    const result = await client.getSandbox("xyz");

    expect(result.tenantId).toBe("0f0e0d0c-0000-4000-8000-000000000001");
  });

  it("deleteSandbox sends DELETE /sandboxes/{id}", async () => {
    const mock = getMockHttp(client);
    mock.requestVoid.mockResolvedValue(undefined);

    await client.deleteSandbox("abc");

    expect(mock.requestVoid).toHaveBeenCalledWith("DELETE", "/sandboxes/abc");
  });

  it("deleteSandbox treats 404 as success", async () => {
    const mock = getMockHttp(client);
    mock.requestVoid.mockRejectedValue(new SandboxServiceError("sandbox not found", 404));

    await expect(client.deleteSandbox("abc")).resolves.toBeUndefined();
  });

  it("readFile sends GET /sandboxes/{id}/files/content", async () => {
    const mock = getMockHttp(client);
    mock.requestBuffer.mockResolvedValue(Buffer.from("file content"));

    const result = await client.readFile("abc", "/test.txt");

    expect(mock.requestBuffer).toHaveBeenCalledWith("GET", "/sandboxes/abc/files/content", {
      params: { path: "/test.txt" },
    });
    expect(result.toString()).toBe("file content");
  });

  it("writeFile sends PUT /sandboxes/{id}/files", async () => {
    const mock = getMockHttp(client);
    mock.requestVoid.mockResolvedValue(undefined);

    await client.writeFile("abc", "/test.txt", "hello");

    expect(mock.requestVoid).toHaveBeenCalledWith(
      "PUT",
      "/sandboxes/abc/files",
      expect.objectContaining({
        params: { path: "/test.txt", overwrite: "true" },
        headers: { "Content-Type": "application/octet-stream" },
      }),
    );
  });

  it("listFiles sends GET /sandboxes/{id}/files with params", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue([
      { name: "a.ts", size: 100, is_dir: false, type: "file", mod_time: "2024-01-01T00:00:00Z" },
    ]);

    const result = await client.listFiles("abc", {
      path: "/src",
      recursive: true,
      extension: ".ts",
    });

    expect(mock.requestJson).toHaveBeenCalledWith("GET", "/sandboxes/abc/files", {
      params: { path: "/src", recursive: "true", extension: ".ts" },
    });
    expect(result).toEqual([
      { name: "a.ts", size: 100, isDir: false, type: "file", modTime: "2024-01-01T00:00:00Z" },
    ]);
  });

  it("stat sends GET /sandboxes/{id}/stat", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      name: "file.txt",
      path: "/home/file.txt",
      type: "file",
      size: 512,
      created_at: "2024-01-01T00:00:00Z",
      modified_at: "2024-01-01T00:00:00Z",
    });

    const result = await client.stat("abc", "/home/file.txt");

    expect(result).toEqual({
      name: "file.txt",
      path: "/home/file.txt",
      type: "file",
      size: 512,
      createdAt: "2024-01-01T00:00:00Z",
      modifiedAt: "2024-01-01T00:00:00Z",
    });
  });
});
