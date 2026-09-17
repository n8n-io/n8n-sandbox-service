import { describe, expect, it, vi, beforeEach } from "vitest";
import { SandboxClient } from "../src/client.js";
import { EgressMismatchError, SandboxServiceError } from "../src/errors.js";
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
