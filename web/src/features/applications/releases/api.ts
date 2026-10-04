import { client, requireData } from "@/api/http";
import { queryOptions } from "@tanstack/react-query";
import type { DiagnosticReport } from "../api";

export const releaseQueryKeys = {
  detail: (releaseId: string) => ["release", releaseId] as const,
  operation: (operationId: string) =>
    ["release-operation", operationId] as const,
  diagnostics: (releaseId: string) =>
    ["release-diagnostics", releaseId] as const,
  logs: (releaseId: string, podName: string, container: string) =>
    ["release-logs", releaseId, podName, container, 200, false] as const,
  lists: (targetId: string) => ["target-releases", targetId] as const,
  latest: (targetId: string) =>
    [...releaseQueryKeys.lists(targetId), 1] as const,
  history: (targetId: string, cursor?: string) =>
    [...releaseQueryKeys.lists(targetId), 5, cursor] as const,
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

export async function getDiagnostics(
  releaseId: string,
): Promise<DiagnosticReport> {
  return requireData(
    await client.GET("/api/v1/releases/{releaseId}/diagnostics", {
      params: { path: { releaseId } },
    }),
    "查询运行诊断",
  );
}

export const releaseQueries = {
  detail: (id: string) =>
    queryOptions({
      queryKey: releaseQueryKeys.detail(id),
      queryFn: () => getRelease(id),
      staleTime: 30_000,
    }),
  operation: (id: string) =>
    queryOptions({
      queryKey: releaseQueryKeys.operation(id),
      queryFn: () => getReleaseOperation(id),
      staleTime: 30_000,
    }),
  diagnostics: (id: string) =>
    queryOptions({
      queryKey: releaseQueryKeys.diagnostics(id),
      queryFn: () => getDiagnostics(id),
      staleTime: 30_000,
    }),
  latest: (id: string) =>
    queryOptions({
      queryKey: releaseQueryKeys.latest(id),
      queryFn: () => latestRelease(id),
      staleTime: 30_000,
    }),
  history: (id: string, cursor?: string) =>
    queryOptions({
      queryKey: releaseQueryKeys.history(id, cursor),
      queryFn: () => listReleaseHistory(id, cursor),
      staleTime: 30_000,
    }),
  logs: (id: string, pod: string, container: string) =>
    queryOptions({
      queryKey: releaseQueryKeys.logs(id, pod, container),
      queryFn: () => getRuntimeLogs(id, pod, container),
    }),
};
