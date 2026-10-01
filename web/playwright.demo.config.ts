import { defineConfig, devices } from "@playwright/test";

// Mock 验收与正常 fixture 测试、真实容器验收完全隔离，浏览器不拦截任何业务请求。
export default defineConfig({
  testDir: "./demo/tests",
  testMatch: "**/*.spec.ts",
  workers: 1,
  timeout: 60_000,
  retries: 0,
  reporter: "list",
  use: {
    baseURL: "http://127.0.0.1:5190",
    trace: "retain-on-failure",
    actionTimeout: 10_000,
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"], channel: "chromium" },
    },
  ],
  webServer: {
    command: "node demo/start.mjs",
    url: "http://127.0.0.1:5190",
    reuseExistingServer: false,
    env: {
      ORBIT_DEMO_WEB_PORT: "5190",
      ORBIT_DEMO_API_PORT: "18091",
      ORBIT_DEMO_STEP_MS: "250",
    },
    timeout: 30_000,
  },
});
