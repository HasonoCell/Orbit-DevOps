import type { IncomingHttpHeaders } from "node:http";
import type { DemoSession, DemoStore } from "./state.ts";
import { fail } from "./state.ts";

export type Result = {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
};
export type Context = {
  method: string;
  path: string;
  url: URL;
  body: Record<string, unknown>;
  headers: IncomingHttpHeaders;
  store: DemoStore;
  session?: DemoSession;
  origin: string;
  apiOrigin: string;
};
export type Handler = (ctx: Context) => Result | undefined;
export const ok = (
  body?: unknown,
  status = 200,
  headers?: Record<string, string>,
): Result => ({ body, status, headers });
export function text(
  body: Record<string, unknown>,
  key: string,
  fallback?: string,
) {
  const value = body[key] ?? fallback;
  if (typeof value !== "string" || !value.trim())
    fail(400, "invalid_request", `请输入 ${key}`);
  return value.trim();
}
export function integer(
  body: Record<string, unknown>,
  key: string,
  fallback: number,
  min = 0,
  max = 65535,
) {
  const value = body[key] ?? fallback;
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value < min ||
    value > max
  )
    fail(400, "invalid_request", `${key} 超出允许范围`);
  return value;
}
export function choice<T extends string>(
  body: Record<string, unknown>,
  key: string,
  values: readonly T[],
  fallback?: T,
): T {
  const value = body[key] ?? fallback;
  if (!values.includes(value as T)) fail(400, "invalid_request", `${key} 无效`);
  return value as T;
}
export function cookie(sessionId = "") {
  return {
    "Set-Cookie": `orbit-demo-session=${sessionId}; HttpOnly; SameSite=Strict; Path=/${sessionId ? "" : "; Max-Age=0"}`,
  };
}
