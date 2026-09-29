import { defineConfig, devices } from "@playwright/test";

// 只在显式验收时运行：真实 API、数据库、Redis 和本地 Kind 由调用方准备。
export default defineConfig({
  testDir: "./tests",
  testMatch: "real-stack.spec.ts",
  fullyParallel: false,
  reporter: "list",
  timeout: 120_000,
  use: {
    ...devices["Desktop Chrome"],
    channel: "chromium",
    baseURL: "http://127.0.0.1:5173",
    trace: "retain-on-failure",
  },
  webServer: {
    command: "pnpm dev --host 127.0.0.1 --port 5173",
    url: "http://127.0.0.1:5173",
    reuseExistingServer: true,
    timeout: 120_000,
  },
});
