import createClient from "openapi-fetch";
import type { components, paths } from "./schema";

export const client = createClient<paths>({
  baseUrl: import.meta.env.VITE_ORBIT_DEVOPS_API_URL ?? "",
  credentials: "include",
});

export const sessionInvalidEvent = "orbit:session-invalid";

// Cookie 仅由浏览器管理；写命令统一带上服务端要求的非简单 Header。
client.use({
  onRequest({ request }) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      request.headers.set("X-Orbit-CSRF", "1");
    }
    return request;
  },
  onResponse({ request, response }) {
    // 业务查询收到 401 时重新核验会话；登录失败和身份核验本身不触发循环。
    if (response.status === 401 && !request.url.includes("/api/v1/auth/") && !request.url.endsWith("/api/v1/users/me")) {
      window.dispatchEvent(new Event(sessionInvalidEvent));
    }
    return response;
  },
});

export type CurrentPrincipal = components["schemas"]["CurrentPrincipal"];
export type AuthProvider = components["schemas"]["AuthProviderSummary"];
export type Project = components["schemas"]["Project"];
export type ProjectPage = components["schemas"]["ProjectPage"];
export type ProjectPermissions = components["schemas"]["ProjectPermissions"];
export type Application = components["schemas"]["Application"];
export type ApplicationPage = components["schemas"]["ApplicationPage"];
export type DeploymentTarget = components["schemas"]["DeploymentTarget"];

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export function requireData<T>(
  result: { data?: T; error?: unknown; response: Response },
  action: string,
): T {
  if (result.data !== undefined) return result.data;
  throw toApiError(result.error, result.response.status, action);
}

export function requireSuccess(
  result: { error?: unknown; response: Response },
  action: string,
): void {
  if (result.response.ok) return;
  throw toApiError(result.error, result.response.status, action);
}

function toApiError(error: unknown, status: number, action: string): ApiError {
  if (typeof error === "object" && error !== null) {
    const record = error as { code?: unknown; message?: unknown };
    if (typeof record.message === "string" && record.message !== "") {
      return new ApiError(record.message, status, typeof record.code === "string" ? record.code : undefined);
    }
  }
  return new ApiError(`${action}失败，请稍后重试`, status);
}

export function errorText(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 429) return "尝试过于频繁，请稍后再试。";
    if (error.status === 403) return "当前账号无权执行此操作，或需要重新验证身份。";
    if (error.status === 409) return "资源状态发生冲突，请刷新后重试。";
    return error.message;
  }
  return error instanceof Error ? error.message : "请求失败，请稍后重试。";
}
