import { createServer } from "node:http";
import { request, expect, test } from "@playwright/test";
import { assertRealBackend } from "./helpers/real-environment";
import realConfig from "../playwright.real.config";

for (const scenario of ["healthy", "mock-header", "mock-body", "unavailable"]) {
  test(`真实 E2E 预检：${scenario}`, async () => {
    const server = createServer((_req, res) => {
      res.writeHead(scenario === "unavailable" ? 503 : 200, {
        "Content-Type": "application/json",
        ...(scenario === "mock-header" ? { "X-Orbit-Demo": "1" } : {}),
      });
      res.end(
        JSON.stringify({ status: scenario === "mock-body" ? "mock" : "ok" }),
      );
    });
    await new Promise<void>((resolve) =>
      server.listen(0, "127.0.0.1", resolve),
    );
    const address = server.address();
    if (!address || typeof address === "string")
      throw new Error("测试 HTTP 地址无效");
    const client = await request.newContext({
      baseURL: `http://127.0.0.1:${address.port}`,
    });
    try {
      if (scenario === "healthy") await assertRealBackend(client);
      else
        await expect(assertRealBackend(client)).rejects.toThrow(
          /Mock|HTTP 503/,
        );
    } finally {
      await client.dispose();
      await new Promise<void>((resolve, reject) =>
        server.close((err) => (err ? reject(err) : resolve())),
      );
    }
  });
}

test("真实 E2E 不复用体验服务或其代理", () => {
  expect(realConfig.use?.baseURL).toBe("http://127.0.0.1:5187");
  const server = realConfig.webServer;
  if (!server || Array.isArray(server))
    throw new Error("真实 E2E 服务配置无效");
  expect(server.reuseExistingServer).toBe(false);
  expect(server.command).toContain("--strictPort");
  expect(server.env?.ORBIT_DEVOPS_WEB_API_PROXY).toBe(
    process.env.ORBIT_DEVOPS_E2E_API_URL ?? "http://127.0.0.1:8080",
  );
});
