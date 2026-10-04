import { client, requireData } from "@/api/http";
import { queryOptions } from "@tanstack/react-query";

export const buildQueryKeys = {
  detail: (buildId: string) => ["build", buildId] as const,
  operation: (operationId: string) => ["build-operation", operationId] as const,
  lists: (applicationId: string) =>
    ["application-builds", applicationId] as const,
  list: (applicationId: string, cursor?: string) =>
    [...buildQueryKeys.lists(applicationId), 5, cursor] as const,
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

// 同一构建分页同时供概览与制品选择使用，不遍历历史来计算总数。
export async function listBuilds(applicationId: string, cursor?: string) {
  return requireData(
    await client.GET("/api/v1/applications/{applicationId}/builds", {
      params: { path: { applicationId }, query: { limit: 5, cursor } },
    }),
    "查询构建",
  );
}

/** 查询定义只描述资源身份；页面自行选择观察预算与活跃条件。 */
export const buildQueries = {
  detail: (id: string) =>
    queryOptions({
      queryKey: buildQueryKeys.detail(id),
      queryFn: () => getBuild(id),
      staleTime: 30_000,
    }),
  operation: (id: string) =>
    queryOptions({
      queryKey: buildQueryKeys.operation(id),
      queryFn: () => getBuildOperation(id),
      staleTime: 30_000,
    }),
  list: (id: string, cursor?: string) =>
    queryOptions({
      queryKey: buildQueryKeys.list(id, cursor),
      queryFn: () => listBuilds(id, cursor),
      staleTime: 30_000,
    }),
  log: (id: string) =>
    queryOptions({
      queryKey: buildQueryKeys.log(id),
      queryFn: () => getBuildAttemptLog(id),
    }),
};
