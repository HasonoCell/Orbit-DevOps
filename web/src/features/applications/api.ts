import { client, requireData } from "@/api/http";
import type { components } from "@/api/schema";

export type DiagnosticReport = components["schemas"]["ReleaseDiagnosticReport"];
export type BuildRecord = components["schemas"]["BuildAcceptance"];
export type Pipeline = components["schemas"]["DeliveryPipelineDetail"];
export type DeliveryRun = components["schemas"]["DeliveryRunDetail"];
export type OperationStatus = components["schemas"]["BuildOperation"]["status"];

export const overviewQueryKeys = {
  root: (applicationId: string) =>
    ["application-overview", applicationId] as const,
  targets: (applicationId: string) =>
    [...overviewQueryKeys.root(applicationId), "targets"] as const,
  builds: (applicationId: string, cursor?: string) =>
    [...overviewQueryKeys.root(applicationId), "builds", cursor] as const,
  pipelines: (applicationId: string, cursor?: string) =>
    [...overviewQueryKeys.root(applicationId), "pipelines", cursor] as const,
  run: (applicationId: string, pipelineId: string) =>
    [...overviewQueryKeys.root(applicationId), "run", pipelineId] as const,
  release: (applicationId: string, targetId: string) =>
    [...overviewQueryKeys.root(applicationId), "release", targetId] as const,
  diagnostics: (applicationId: string, releaseId?: string) =>
    [
      ...overviewQueryKeys.root(applicationId),
      "diagnostics",
      releaseId,
    ] as const,
};

// 概览采用有界分页，禁止为了计算总数或汇总状态而遍历全部历史。
export async function listBuilds(applicationId: string, cursor?: string) {
  return requireData(
    await client.GET("/api/v1/applications/{applicationId}/builds", {
      params: { path: { applicationId }, query: { limit: 5, cursor } },
    }),
    "查询构建",
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

export async function listPipelines(applicationId: string, cursor?: string) {
  return requireData(
    await client.GET(
      "/api/v1/applications/{applicationId}/delivery-pipelines",
      {
        params: { path: { applicationId }, query: { limit: 3, cursor } },
      },
    ),
    "查询自动交付",
  );
}

export async function latestRun(deliveryPipelineId: string) {
  const page = requireData(
    await client.GET("/api/v1/delivery-pipelines/{deliveryPipelineId}/runs", {
      params: { path: { deliveryPipelineId }, query: { limit: 1 } },
    }),
    "查询交付运行",
  );
  return page.items[0] ?? null;
}
