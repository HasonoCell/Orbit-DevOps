import { client, requireData, requireSuccess } from "@/api/http";
import type { components } from "@/api/schema";

type BindingInput = components["schemas"]["AccessSecretBindingInput"];

export async function listSecretBindings(offset: number) {
  return requireData(
    await client.GET("/api/v1/platform/access-secret-bindings", {
      params: { query: { limit: 21, offset } },
    }),
    "查询 TLS Secret 授权",
  );
}

export async function registerSecretBinding(body: BindingInput, key: string) {
  return requireData(
    await client.POST("/api/v1/platform/access-secret-bindings", {
      params: { header: { "Idempotency-Key": key } },
      body,
    }),
    "登记 TLS Secret 授权",
  );
}

export async function revokeSecretBinding(bindingId: string, key: string) {
  return requireData(
    await client.DELETE("/api/v1/platform/access-secret-bindings/{bindingId}", {
      params: {
        path: { bindingId },
        header: { "Idempotency-Key": key },
      },
    }),
    "撤销 TLS Secret 授权",
  );
}

export async function listUsers(cursor?: string) {
  return requireData(
    await client.GET("/api/v1/users", {
      params: { query: { limit: 20, cursor } },
    }),
    "查询平台用户",
  );
}

export async function getUser(userId: string) {
  return requireData(
    await client.GET("/api/v1/users/{userId}", {
      params: { path: { userId } },
    }),
    "查询用户详情",
  );
}

export async function createLocalUser(
  body: components["schemas"]["CreateLocalUserRequest"],
) {
  return requireData(
    await client.POST("/api/v1/users", { body }),
    "创建本地用户",
  );
}

export async function resetUserPassword(
  userId: string,
  body: components["schemas"]["ResetLocalPasswordRequest"],
) {
  requireSuccess(
    await client.POST("/api/v1/users/{userId}/password/reset", {
      params: { path: { userId } },
      body,
    }),
    "重置用户密码",
  );
}

export async function changePlatformRole(
  userId: string,
  role: components["schemas"]["PlatformRole"],
) {
  return requireData(
    await client.PUT("/api/v1/users/{userId}/platform-role", {
      params: { path: { userId } },
      body: { role },
    }),
    "修改平台角色",
  );
}

export async function setUserEnabled(userId: string, enabled: boolean) {
  const path = enabled
    ? "/api/v1/users/{userId}/enable"
    : "/api/v1/users/{userId}/disable";
  return requireData(
    await client.POST(path, { params: { path: { userId } } }),
    enabled ? "启用用户" : "停用用户",
  );
}
