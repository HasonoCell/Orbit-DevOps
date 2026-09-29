import { ArrowRight } from "lucide-react";
import type { DeliveryRun } from "@/features/applications/api";
import { runStatus } from "@/shared/overview-status";

// 来源校验与部署分别是一步；Run 只记录关联，不把某个环境当成流程阶段。
export function DeliveryStages({
  run,
  buildOnly = false,
}: {
  run: DeliveryRun;
  buildOnly?: boolean;
}) {
  const build = run.run.imageArtifactId
    ? "产物就绪"
    : run.activeStage === "build"
      ? runStatus[run.status].label
      : "构建未完成";
  const source = buildOnly
    ? "不适用"
    : run.run.releaseId
      ? "来源已校验"
      : run.activeStage === "source_verification"
        ? "校验来源中"
        : run.status === "blocked" || run.status === "superseded"
          ? runStatus[run.status].label
          : "待校验";
  const deploy = buildOnly
    ? "不自动部署"
    : run.run.releaseId
      ? run.status === "succeeded"
        ? "部署完成"
        : runStatus[run.status].label
      : "尚未创建发布";
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/40 p-3 text-xs">
      <span>构建：{build}</span>
      <ArrowRight className="size-3 text-muted-foreground" aria-hidden="true" />
      <span>来源校验：{source}</span>
      <ArrowRight className="size-3 text-muted-foreground" aria-hidden="true" />
      <span>部署：{deploy}</span>
    </div>
  );
}
