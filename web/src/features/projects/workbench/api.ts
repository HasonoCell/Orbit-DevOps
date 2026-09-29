import { client, requireData } from "@/api/http";
import type { components } from "@/api/schema";

export type ApplicationSummary =
  components["schemas"]["ApplicationWorkbenchItem"];
export type WorkbenchPage = components["schemas"]["ApplicationWorkbenchPage"];

export const workbenchQueryKeys = {
  project: (projectId: string) => ["project-workbench", projectId] as const,
  page: (projectId: string, cursor?: string) =>
    [...workbenchQueryKeys.project(projectId), cursor] as const,
};

/** 后端只聚合当前应用页的数据库摘要；运行健康仍在应用页单独观测。 */
export async function listWorkbench(
  projectId: string,
  cursor?: string,
  signal?: AbortSignal,
): Promise<WorkbenchPage> {
  return requireData(
    await client.GET("/api/v1/projects/{projectId}/application-workbench", {
      signal,
      params: { path: { projectId }, query: { limit: 8, cursor } },
    }),
    "查询项目工作台",
  );
}

/** 未返回执行结果表示暂无记录，不把空值误判为健康或异常。 */
export function summaryIssues(summary: ApplicationSummary): string[] {
  const issues: string[] = [];
  const actionable = (status: string) =>
    status === "failed" || status === "attention_required";
  if (summary.build && actionable(summary.build.status))
    issues.push("最近构建需要处理");
  for (const target of summary.targets) {
    if (target.releaseStatus && actionable(target.releaseStatus))
      issues.push(`${target.stage} 发布需要处理`);
  }
  for (const pipeline of summary.pipelines) {
    if (
      pipeline.runStatus &&
      [
        "build_failed",
        "release_failed",
        "blocked",
        "attention_required",
      ].includes(pipeline.runStatus)
    )
      issues.push(`${pipeline.name} 交付需要处理`);
  }
  return issues;
}
