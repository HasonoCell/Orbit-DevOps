import { client, requireData } from "@/api/http";
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
