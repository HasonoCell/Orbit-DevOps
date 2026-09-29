import { client, requireData } from "@/api/http";

export type ReleaseCommand = "retry" | "cancel" | "reconcile" | "force-fail";

export async function runReleaseCommand(
  releaseOperationId: string,
  command: ReleaseCommand,
  idempotencyKey: string,
  reason?: string,
) {
  const params = {
    path: { releaseOperationId },
    header: { "Idempotency-Key": idempotencyKey },
  };
  switch (command) {
    case "retry":
      return requireData(
        await client.POST(
          "/api/v1/release-operations/{releaseOperationId}/retry",
          { params },
        ),
        "重试发布执行",
      );
    case "cancel":
      return requireData(
        await client.POST(
          "/api/v1/release-operations/{releaseOperationId}/cancel",
          { params },
        ),
        "取消发布执行",
      );
    case "reconcile":
      return requireData(
        await client.POST(
          "/api/v1/release-operations/{releaseOperationId}/reconcile",
          { params },
        ),
        "重新对账发布执行",
      );
    case "force-fail":
      return requireData(
        await client.POST(
          "/api/v1/release-operations/{releaseOperationId}/fail",
          {
            params,
            body: { reason: reason ?? "" },
          },
        ),
        "人工结束发布执行",
      );
  }
}

export async function rollbackRelease(
  releaseId: string,
  idempotencyKey: string,
) {
  return requireData(
    await client.POST("/api/v1/releases/{releaseId}/rollback", {
      params: {
        path: { releaseId },
        header: { "Idempotency-Key": idempotencyKey },
      },
    }),
    "创建回滚发布",
  );
}
