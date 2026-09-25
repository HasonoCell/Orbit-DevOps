import { client, requireData, errorText, type Application } from "./http";
import type { components } from "./schema";

type Schema = components["schemas"];
export type Observation<T> = { data: T; error?: never } | { data?: never; error: string };
type TargetRelease = { target: Schema["DeploymentTarget"]; release: Observation<Schema["ReleaseHistoryItem"] | null> };
export type ApplicationSummary = {
  application: Application;
  targets: Observation<TargetRelease[]>;
  build: Observation<Schema["BuildAcceptance"] | null>;
  pipelines: Observation<{ pipeline: Schema["DeliveryPipelineDetail"]; run: Observation<Schema["DeliveryRunDetail"] | null> }[]>;
};

async function observe<T>(read: () => Promise<T>, signal: AbortSignal): Promise<Observation<T>> {
  try { return { data: await read() }; }
  catch (error) { if (signal.aborted) throw error; return { error: errorText(error) }; }
}

/** 仅加载当前页八个应用、各目标最新发布、最新构建和最多三条 Pipeline。
 * 不遍历历史、不在项目列表触发 Kubernetes 诊断；最多同时处理两个应用。
 * 局部失败保留为独立错误，取消翻页时停止继续排队，避免过期请求占用连接。
 */
export async function loadWorkbench(applications: Application[], signal: AbortSignal): Promise<ApplicationSummary[]> {
  let index = 0;
  const result: ApplicationSummary[] = new Array(applications.length);
  async function worker() {
    while (index < applications.length) {
      signal.throwIfAborted();
      const position = index++;
      const application = applications[position];
      const applicationId = application.id;
      const [targets, build, pipelines] = await Promise.all([
        observe(async () => {
          const page = requireData(await client.GET("/api/v1/applications/{applicationId}/deployment-targets", { signal, params: { path: { applicationId }, query: { limit: 20 } } }), "查询部署目标");
          // 当前契约每个应用仅两个 Stage；若未来扩展导致分页，不能静默当作完整集合。
          if (page.nextCursor) throw new Error("部署目标超过当前摘要范围，请进入应用查看。");
          return Promise.all(page.items.map(async target => ({ target, release: await observe(async () => {
            const history = requireData(await client.GET("/api/v1/deployment-targets/{deploymentTargetId}/releases", { signal, params: { path: { deploymentTargetId: target.id }, query: { limit: 1 } } }), "查询发布");
            return history.items[0] ?? null;
          }, signal) })));
        }, signal),
        observe(async () => {
          const page = requireData(await client.GET("/api/v1/applications/{applicationId}/builds", { signal, params: { path: { applicationId }, query: { limit: 1 } } }), "查询构建");
          return page.items[0] ?? null;
        }, signal),
        observe(async () => {
          const page = requireData(await client.GET("/api/v1/applications/{applicationId}/delivery-pipelines", { signal, params: { path: { applicationId }, query: { limit: 3 } } }), "查询自动交付");
          return Promise.all(page.items.map(async pipeline => ({ pipeline, run: await observe(async () => {
            const runs = requireData(await client.GET("/api/v1/delivery-pipelines/{deliveryPipelineId}/runs", { signal, params: { path: { deliveryPipelineId: pipeline.pipeline.id }, query: { limit: 1 } } }), "查询交付运行");
            return runs.items[0] ?? null;
          }, signal) })));
        }, signal),
      ]);
      result[position] = { application, targets, build, pipelines };
    }
  }
  await Promise.all([worker(), worker()]);
  return result;
}

/** 异常过滤只基于成功读取的执行结果；查询失败单独提示，不当作执行失败。 */
export function summaryIssues(summary: ApplicationSummary): string[] {
  const issues: string[] = [];
  const actionable = (status: string) => status === "failed" || status === "attention_required";
  if (summary.build.data && actionable(summary.build.data.buildOperation.status)) issues.push("最近构建需要处理");
  for (const item of summary.targets.data ?? []) {
    if (item.release.data && actionable(item.release.data.releaseOperation.status)) issues.push(`${item.target.stage} 发布需要处理`);
  }
  for (const item of summary.pipelines.data ?? []) {
    if (item.run.data && ["build_failed", "release_failed", "blocked", "attention_required"].includes(item.run.data.status)) issues.push(`${item.pipeline.pipeline.name} 交付需要处理`);
  }
  return issues;
}

export function summaryErrors(summary: ApplicationSummary): string[] {
  return [summary.targets.error, summary.build.error, summary.pipelines.error,
    ...(summary.targets.data ?? []).map(item => item.release.error),
    ...(summary.pipelines.data ?? []).map(item => item.run.error)].filter((error): error is string => !!error);
}
