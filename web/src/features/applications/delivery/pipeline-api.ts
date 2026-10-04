import { client, requireData } from "@/api/http";
import type { components } from "@/api/schema";
import { queryOptions } from "@tanstack/react-query";

export type PipelineInput =
  components["schemas"]["CreateDeliveryPipelineRequest"];
export type PipelineUpdate =
  components["schemas"]["UpdateDeliveryPipelineRequest"];

export const pipelineKeys = {
  detail: (id: string) => ["delivery-pipeline", id] as const,
  lists: (applicationId: string) =>
    ["application-pipelines", applicationId] as const,
  list: (applicationId: string, cursor?: string) =>
    [...pipelineKeys.lists(applicationId), 3, cursor] as const,
  latest: (id: string) => ["delivery-pipeline", id, "latest-run", 1] as const,
  run: (id: string) => ["delivery-run", id] as const,
  runs: (id: string, cursor?: string) =>
    ["delivery-pipeline", id, "runs", 10, cursor] as const,
};

export async function getPipeline(deliveryPipelineId: string) {
  return requireData(
    await client.GET("/api/v1/delivery-pipelines/{deliveryPipelineId}", {
      params: { path: { deliveryPipelineId } },
    }),
    "查询自动交付配置",
  );
}

export async function createPipeline(
  applicationId: string,
  body: PipelineInput,
  key: string,
) {
  return requireData(
    await client.POST(
      "/api/v1/applications/{applicationId}/delivery-pipelines",
      {
        params: {
          path: { applicationId },
          header: { "Idempotency-Key": key },
        },
        body,
      },
    ),
    "创建自动交付配置",
  );
}

export async function updatePipeline(
  deliveryPipelineId: string,
  body: PipelineUpdate,
  key: string,
) {
  return requireData(
    await client.PUT("/api/v1/delivery-pipelines/{deliveryPipelineId}", {
      params: {
        path: { deliveryPipelineId },
        header: { "Idempotency-Key": key },
      },
      body,
    }),
    "修订自动交付配置",
  );
}

export async function changePipelineState(
  deliveryPipelineId: string,
  action: "enable" | "disable",
  key: string,
) {
  const params = {
    path: { deliveryPipelineId },
    header: { "Idempotency-Key": key },
  };
  return action === "enable"
    ? requireData(
        await client.POST(
          "/api/v1/delivery-pipelines/{deliveryPipelineId}/enable",
          { params },
        ),
        "启用自动交付",
      )
    : requireData(
        await client.POST(
          "/api/v1/delivery-pipelines/{deliveryPipelineId}/disable",
          { params },
        ),
        "停用自动交付",
      );
}

export async function listRuns(deliveryPipelineId: string, cursor?: string) {
  return requireData(
    await client.GET("/api/v1/delivery-pipelines/{deliveryPipelineId}/runs", {
      params: {
        path: { deliveryPipelineId },
        query: { limit: 10, cursor },
      },
    }),
    "查询交付运行",
  );
}

export async function getRun(deliveryRunId: string) {
  return requireData(
    await client.GET("/api/v1/delivery-runs/{deliveryRunId}", {
      params: { path: { deliveryRunId } },
    }),
    "查询交付运行详情",
  );
}

export async function reconcileRun(deliveryRunId: string, key: string) {
  return requireData(
    await client.POST("/api/v1/delivery-runs/{deliveryRunId}/reconcile", {
      params: {
        path: { deliveryRunId },
        header: { "Idempotency-Key": key },
      },
    }),
    "重新对账交付运行",
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

export const pipelineQueries = {
  detail: (id: string) =>
    queryOptions({
      queryKey: pipelineKeys.detail(id),
      queryFn: () => getPipeline(id),
      staleTime: 30_000,
    }),
  list: (id: string, cursor?: string) =>
    queryOptions({
      queryKey: pipelineKeys.list(id, cursor),
      queryFn: () => listPipelines(id, cursor),
      staleTime: 30_000,
    }),
  latest: (id: string) =>
    queryOptions({
      queryKey: pipelineKeys.latest(id),
      queryFn: () => latestRun(id),
      staleTime: 30_000,
    }),
  runs: (id: string, cursor?: string) =>
    queryOptions({
      queryKey: pipelineKeys.runs(id, cursor),
      queryFn: () => listRuns(id, cursor),
      staleTime: 30_000,
    }),
  run: (id: string) =>
    queryOptions({
      queryKey: pipelineKeys.run(id),
      queryFn: () => getRun(id),
      staleTime: 30_000,
    }),
};
