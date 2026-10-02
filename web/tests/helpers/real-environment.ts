import type { APIRequestContext } from "@playwright/test";

// 在任何登录/写入前核验正式 API，错误只包含类别，不输出认证材料或响应原文。
export async function assertRealBackend(request: APIRequestContext) {
  const response = await request.get("/healthz", { timeout: 5_000 });
  if (response.headers()["x-orbit-demo"] !== undefined) {
    throw new Error("真实 E2E 拒绝 Mock API，请检查专用后端代理");
  }
  if (response.status() !== 200) {
    throw new Error(`真实 API 尚未就绪：HTTP ${response.status()}`);
  }
  const body: unknown = await response.json();
  if (
    typeof body !== "object" ||
    body === null ||
    !("status" in body) ||
    body.status !== "ok"
  ) {
    throw new Error("真实 E2E 拒绝 Mock 或未知健康响应");
  }
}
