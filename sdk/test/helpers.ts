import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";

export type TestServer = {
  server: Server;
  baseUrl: string;
  close: () => Promise<void>;
};

export async function startTestServer(
  handler: (req: IncomingMessage, res: ServerResponse<IncomingMessage>) => void,
): Promise<TestServer> {
  const server = createServer(handler);

  await new Promise<void>((resolve, reject) => {
    server.listen(0, "127.0.0.1", () => resolve());
    server.once("error", reject);
  });

  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("Expected an ephemeral TCP port");
  }

  return {
    server,
    baseUrl: `http://127.0.0.1:${address.port}`,
    close: async () => {
      await new Promise<void>((resolve, reject) => {
        server.close((error) => (error ? reject(error) : resolve()));
      });
    },
  };
}
