import { client, requireData } from "@/api/http";

export const buildQueryKeys = {
  detail: (buildId: string) => ["build", buildId] as const,
  operation: (operationId: string) => ["build-operation", operationId] as const,
  log: (attemptId: string) => ["build-log", attemptId] as const,
};

export async function createBuild(
  applicationId: string,
  input: {
    repositoryUrl: string;
    sourceCommit: string;
    dockerfilePath: string;
    contextPath: string;
  },
  idempotencyKey: string,
) {
  return requireData(
    await client.POST("/api/v1/applications/{applicationId}/builds", {
      params: {
        path: { applicationId },
        header: { "Idempotency-Key": idempotencyKey },
      },
      body: input,
    }),
    "创建构建",
  );
}

export async function getBuild(buildId: string) {
  return requireData(
    await client.GET("/api/v1/builds/{buildId}", {
      params: { path: { buildId } },
    }),
    "查询构建",
  );
}

export async function getBuildOperation(buildOperationId: string) {
  return requireData(
    await client.GET("/api/v1/build-operations/{buildOperationId}", {
      params: { path: { buildOperationId } },
    }),
    "查询构建执行",
  );
}

export async function getBuildAttemptLog(buildAttemptId: string) {
  return requireData(
    await client.GET("/api/v1/build-attempts/{buildAttemptId}/log", {
      params: { path: { buildAttemptId } },
    }),
    "读取构建日志摘录",
  );
}
