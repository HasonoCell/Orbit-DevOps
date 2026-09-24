import { client, requireData } from "./http";
import type { components } from "./schema";

export type DiagnosticReport = components["schemas"]["ReleaseDiagnosticReport"];
export type BuildRecord = components["schemas"]["BuildAcceptance"];
export type Pipeline = components["schemas"]["DeliveryPipelineDetail"];
export type DeliveryRun = components["schemas"]["DeliveryRunDetail"];
export type OperationStatus = components["schemas"]["BuildOperation"]["status"];

// 概览采用有界分页，禁止为了计算总数或汇总状态而遍历全部历史。
export async function listBuilds(applicationId: string, cursor?: string) {
  return requireData(await client.GET("/api/v1/applications/{applicationId}/builds", {
    params: { path: { applicationId }, query: { limit: 5, cursor } },
  }), "查询构建");
}

export async function latestRelease(deploymentTargetId: string) {
  const page = requireData(await client.GET("/api/v1/deployment-targets/{deploymentTargetId}/releases", {
    params: { path: { deploymentTargetId }, query: { limit: 1 } },
  }), "查询发布");
  return page.items[0] ?? null;
}

export async function getDiagnostics(releaseId: string): Promise<DiagnosticReport> {
  return requireData(await client.GET("/api/v1/releases/{releaseId}/diagnostics", {
    params: { path: { releaseId } },
  }), "查询运行诊断");
}

export async function listPipelines(applicationId: string, cursor?: string) {
  return requireData(await client.GET("/api/v1/applications/{applicationId}/delivery-pipelines", {
    params: { path: { applicationId }, query: { limit: 3, cursor } },
  }), "查询自动交付");
}

export async function latestRun(deliveryPipelineId: string) {
  const page = requireData(await client.GET("/api/v1/delivery-pipelines/{deliveryPipelineId}/runs", {
    params: { path: { deliveryPipelineId }, query: { limit: 1 } },
  }), "查询交付运行");
  return page.items[0] ?? null;
}
