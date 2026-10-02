import { defineConfig, devices } from "@playwright/test";

const apiURL = process.env.ORBIT_DEVOPS_E2E_API_URL ?? "http://127.0.0.1:8080";
const endpoint = new URL(apiURL);
if (
  !["127.0.0.1", "localhost", "[::1]"].includes(endpoint.hostname) ||
  !["http:", "https:"].includes(endpoint.protocol) ||
  endpoint.username ||
  endpoint.password ||
  endpoint.pathname !== "/" ||
  endpoint.search ||
  endpoint.hash
)
  throw new Error("真实 E2E 需要本地 API 根地址");

// 只在显式验收时运行：真实 API、数据库、Redis 和本地 Kind 由调用方准备。
export default defineConfig({
  testDir: "./tests",
  testMatch: "real-stack.spec.ts",
  globalSetup: "./real-e2e-setup.ts",
  fullyParallel: false,
  reporter: "list",
  timeout: 120_000,
  use: {
    ...devices["Desktop Chrome"],
    channel: "chromium",
    baseURL: "http://127.0.0.1:5187",
    trace: "retain-on-failure",
  },
  webServer: {
    command:
      "node node_modules/vite/bin/vite.js --host 127.0.0.1 --port 5187 --strictPort",
    url: "http://127.0.0.1:5187",
    reuseExistingServer: false,
    env: {
      ORBIT_DEVOPS_WEB_API_PROXY: apiURL,
      VITE_ORBIT_DEVOPS_API_URL: "",
    },
    timeout: 120_000,
  },
});
