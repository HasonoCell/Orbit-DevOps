import { client, requireData } from "@/api/http";

export type BuildCommand = "retry" | "cancel" | "reconcile" | "force-fail";

export async function runBuildCommand(
  buildOperationId: string,
  command: BuildCommand,
  idempotencyKey: string,
  reason?: string,
) {
  const params = {
    path: { buildOperationId },
    header: { "Idempotency-Key": idempotencyKey },
  };
  switch (command) {
    case "retry":
      return requireData(
        await client.POST("/api/v1/build-operations/{buildOperationId}/retry", {
          params,
        }),
        "重试构建",
      );
    case "cancel":
      return requireData(
        await client.POST(
          "/api/v1/build-operations/{buildOperationId}/cancel",
          { params },
        ),
        "取消构建",
      );
    case "reconcile":
      return requireData(
        await client.POST(
          "/api/v1/build-operations/{buildOperationId}/reconcile",
          { params },
        ),
        "重新对账构建",
      );
    case "force-fail":
      return requireData(
        await client.POST(
          "/api/v1/build-operations/{buildOperationId}/force-fail",
          {
            params,
            body: { reason: reason ?? "" },
          },
        ),
        "人工结束构建",
      );
  }
}
