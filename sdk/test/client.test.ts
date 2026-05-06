import { describe, expect, it, vi, beforeEach } from "vitest";
import { SandboxClient } from "../src/client.js";
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
    expect(HttpClient).toHaveBeenCalledWith("http://localhost:8080", "test-key");
  });

  it("createSandbox sends POST /sandboxes", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "abc",
      status: "running",
      created_at: 1000,
      last_active_at: 1000,
    });

    const result = await client.createSandbox();

    expect(mock.requestJson).toHaveBeenCalledWith("POST", "/sandboxes");
    expect(result).toEqual({
      id: "abc",
      status: "running",
      createdAt: 1000,
      lastActiveAt: 1000,
    });
  });

  it("getSandbox sends GET /sandboxes/{id}", async () => {
    const mock = getMockHttp(client);
    mock.requestJson.mockResolvedValue({
      id: "xyz",
      status: "running",
      created_at: 2000,
      last_active_at: 2000,
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
