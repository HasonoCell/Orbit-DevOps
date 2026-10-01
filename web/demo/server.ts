import { createServer, type IncomingMessage } from "node:http";
import { fileURLToPath } from "node:url";
import { accessHandler, advanceAccess } from "./access.ts";
import { catalogHandler } from "./catalog.ts";
import { controlHandler } from "./control.ts";
import { advance, deliveryHandler } from "./delivery.ts";
import { identityHandler } from "./identity.ts";
import { MockError, DemoStore, copy, fail } from "./state.ts";
import type { Context, Result } from "./http.ts";

async function readBody(
  request: IncomingMessage,
): Promise<Record<string, unknown>> {
  let value = "";
  for await (const chunk of request) {
    value += chunk;
    if (Buffer.byteLength(value) > 512 * 1024)
      fail(413, "body_too_large", "请求体过大");
  }
  if (!value) return {};
  try {
    const parsed: unknown = JSON.parse(value);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed))
      return parsed as Record<string, unknown>;
  } catch {
    /* 统一转为输入错误，不输出原始请求。 */
  }
  return fail(400, "invalid_json", "请求体需要 JSON 对象");
}

/** Mock 服务只监听回环地址，所有 /api 请求留在本进程，未知路由明确报错而不转发真实服务。 */
export function createDemoServer(
  options: { origin?: string; stepMs?: number } = {},
) {
  const origin = options.origin ?? "http://127.0.0.1:5173";
  const s = new DemoStore(options.stepMs);
  const server = createServer(async (request, response) => {
    const apiOrigin = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
    let status = 500;
    try {
      if (
        ![new URL(origin).host, new URL(apiOrigin).host].includes(
          request.headers.host ?? "",
        )
      )
        fail(403, "host_rejected", "只接受本地演示地址");
      if (
        request.headers.origin &&
        ![origin, apiOrigin].includes(request.headers.origin)
      )
        fail(403, "origin_rejected", "只接受本地演示页面");
      const url = new URL(request.url ?? "/", apiOrigin);
      const method = request.method ?? "GET";
      if (
        !["GET", "HEAD", "OPTIONS"].includes(method) &&
        request.headers["x-orbit-csrf"] !== "1"
      )
        fail(403, "csrf_rejected", "需要本地命令 Header");
      const token = request.headers.cookie
        ?.split(";")
        .map((c) => c.trim())
        .find((c) => c.startsWith("orbit-demo-session="))
        ?.slice("orbit-demo-session=".length);
      const session = token ? s.sessions.get(token) : undefined;
      advance(s);
      advanceAccess(s);
      const ctx: Context = {
        method,
        path: url.pathname,
        url,
        body: await readBody(request),
        headers: request.headers,
        store: s,
        session,
        origin,
        apiOrigin,
      };
      let result: Result | undefined;
      const key = request.headers["idempotency-key"];
      const dedupKey =
        typeof key === "string"
          ? `${session?.userId ?? session?.identityId}:${method}:${url.pathname}:${key}`
          : undefined;
      const payload = JSON.stringify(ctx.body);
      const previous = dedupKey ? s.dedup.get(dedupKey) : undefined;
      if (previous) {
        s.user(session);
        if (previous.payload !== payload)
          fail(409, "idempotency_conflict", "相同幂等键不能提交不同输入");
        result = { status: previous.status, body: copy(previous.result) };
      } else {
        if (url.pathname === "/healthz")
          result = {
            status: 200,
            body: { status: "mock", dependencies: "none" },
          };
        else
          for (const handler of [
            controlHandler,
            identityHandler,
            catalogHandler,
            deliveryHandler,
            accessHandler,
          ]) {
            result = handler(ctx);
            if (result) break;
          }
        if (!result)
          fail(404, "mock_route_not_found", "Mock 未提供此请求方法或接口");
        if (
          dedupKey &&
          method !== "GET" &&
          result.status >= 200 &&
          result.status < 300
        )
          s.dedup.set(dedupKey, {
            payload,
            status: result.status,
            result: copy(result.body),
          });
      }
      status = result.status;
      response.writeHead(status, {
        "Content-Type": "application/json; charset=utf-8",
        "Cache-Control": "no-store",
        "X-Orbit-Demo": "1",
        "X-Content-Type-Options": "nosniff",
        ...(result.headers ?? {}),
      });
      response.end(
        result.body === undefined
          ? undefined
          : typeof result.body === "string" &&
              result.headers?.["Content-Type"]?.startsWith("text/html")
            ? result.body
            : JSON.stringify(result.body),
      );
    } catch (error) {
      status = error instanceof MockError ? error.status : 500;
      response.writeHead(status, {
        "Content-Type": "application/json",
        "Cache-Control": "no-store",
        "X-Orbit-Demo": "1",
      });
      response.end(
        JSON.stringify({
          code: error instanceof MockError ? error.code : "mock_internal_error",
          message:
            error instanceof MockError ? error.message : "Mock 请求处理失败",
        }),
      );
      if (!(error instanceof MockError))
        console.error("Mock handler failure", error);
    }
  });
  const timer = setInterval(() => {
    advance(s);
    advanceAccess(s);
  }, 250);
  timer.unref();
  server.on("close", () => clearInterval(timer));
  return server;
}
if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const port = Number(process.env.ORBIT_DEMO_API_PORT ?? 18090);
  const stepMs = Number(process.env.ORBIT_DEMO_STEP_MS ?? 5000);
  if (
    !Number.isInteger(port) ||
    port < 1024 ||
    port > 65535 ||
    !Number.isFinite(stepMs) ||
    stepMs < 10
  )
    throw new Error("Invalid local demo configuration");
  const server = createDemoServer({
    stepMs,
    origin: `http://127.0.0.1:${process.env.ORBIT_DEMO_WEB_PORT ?? 5173}`,
  });
  server.listen(port, "127.0.0.1", () =>
    console.log(`Mock API: http://127.0.0.1:${port}`),
  );
  for (const signal of ["SIGINT", "SIGTERM"] as const)
    process.on(signal, () => server.close());
}
