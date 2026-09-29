import { client, requireData } from "@/api/http";

export const releaseQueryKeys = {
  detail: (releaseId: string) => ["release", releaseId] as const,
  operation: (operationId: string) =>
    ["release-operation", operationId] as const,
  diagnostics: (releaseId: string) =>
    ["release-diagnostics", releaseId] as const,
  logs: (releaseId: string, podName: string, container: string) =>
    ["release-logs", releaseId, podName, container] as const,
  artifacts: (applicationId: string, cursor?: string) =>
    ["release-artifacts", applicationId, cursor] as const,
  history: (applicationId: string, targetId: string, cursor?: string) =>
    [
      "application-overview",
      applicationId,
      "history",
      targetId,
      cursor,
    ] as const,
};

export async function getRelease(releaseId: string) {
  return requireData(
    await client.GET("/api/v1/releases/{releaseId}", {
      params: { path: { releaseId } },
    }),
    "查询发布详情",
  );
}

export async function getReleaseOperation(releaseOperationId: string) {
  return requireData(
    await client.GET("/api/v1/release-operations/{releaseOperationId}", {
      params: { path: { releaseOperationId } },
    }),
    "查询发布执行",
  );
}

export async function getRuntimeLogs(
  releaseId: string,
  podName: string,
  container: string,
) {
  return requireData(
    await client.GET("/api/v1/releases/{releaseId}/runtime-logs", {
      params: {
        path: { releaseId },
        query: { podName, container, tailLines: 200, previous: false },
      },
    }),
    "读取运行日志摘录",
  );
}

export async function latestRelease(deploymentTargetId: string) {
  const page = requireData(
    await client.GET(
      "/api/v1/deployment-targets/{deploymentTargetId}/releases",
      {
        params: { path: { deploymentTargetId }, query: { limit: 1 } },
      },
    ),
    "查询发布",
  );
  return page.items[0] ?? null;
}

export async function listReleaseHistory(
  deploymentTargetId: string,
  cursor?: string,
) {
  return requireData(
    await client.GET(
      "/api/v1/deployment-targets/{deploymentTargetId}/releases",
      {
        params: { path: { deploymentTargetId }, query: { limit: 5, cursor } },
      },
    ),
    "查询发布历史",
  );
}

export async function acceptRelease(
  deploymentTargetId: string,
  body: { imageReference: string; imageArtifactId?: string },
  idempotencyKey: string,
) {
  return requireData(
    await client.POST(
      "/api/v1/deployment-targets/{deploymentTargetId}/releases",
      {
        params: {
          path: { deploymentTargetId },
          header: { "Idempotency-Key": idempotencyKey },
        },
        body,
      },
    ),
    "接纳发布",
  );
}
