import type { components } from "@/api/schema";

type Schema = components["schemas"];
type DiagnosticReport = Schema["ReleaseDiagnosticReport"];
type DeliveryRun = Schema["DeliveryRunDetail"];
type OperationStatus = Schema["BuildOperation"]["status"];

export type Tone = "neutral" | "success" | "progress" | "warning" | "danger";
export type Status = { label: string; tone: Tone };

export const operationStatus: Record<OperationStatus, Status> = {
  pending: { label: "排队中", tone: "neutral" },
  running: { label: "执行中", tone: "progress" },
  cancel_requested: { label: "取消中", tone: "warning" },
  attention_required: { label: "需要处理", tone: "warning" },
  succeeded: { label: "执行成功", tone: "success" },
  failed: { label: "执行失败", tone: "danger" },
  canceled: { label: "已取消", tone: "neutral" },
};

export const runStatus: Record<DeliveryRun["status"], Status> = {
  building: { label: "构建中", tone: "progress" },
  build_failed: { label: "构建失败", tone: "danger" },
  build_canceled: { label: "构建已取消", tone: "neutral" },
  candidate_ready: { label: "产物就绪", tone: "success" },
  verifying_source: { label: "校验来源中", tone: "progress" },
  releasing: { label: "部署中", tone: "progress" },
  release_failed: { label: "部署失败", tone: "danger" },
  release_canceled: { label: "部署已取消", tone: "neutral" },
  succeeded: { label: "交付成功", tone: "success" },
  superseded: { label: "已被替代", tone: "neutral" },
  attention_required: { label: "需要处理", tone: "warning" },
  blocked: { label: "已阻塞", tone: "warning" },
};

export function isActiveOperation(status: OperationStatus) {
  return (
    status === "pending" ||
    status === "running" ||
    status === "cancel_requested"
  );
}
export function isActiveRun(status: DeliveryRun["status"]) {
  return (
    status === "building" ||
    status === "verifying_source" ||
    status === "releasing"
  );
}

/** 运行状态只来自工作负载观测；Operation 成功和历史 Warning Event 不能代替当前健康判断。 */
export function runtimeStatus(report: DiagnosticReport): Status {
  const { metadata, deployment, service, pods } = report.workloadObservation;
  if (metadata.status === "unavailable")
    return { label: "运行观测不可用", tone: "neutral" };
  if (metadata.status === "partial")
    return { label: "运行观测不完整", tone: "warning" };
  if (
    (deployment && !deployment.ownershipMatches) ||
    (service && !service.ownershipMatches)
  )
    return { label: "资源归属冲突", tone: "danger" };
  if (report.runtimeReleaseRelation === "different")
    return { label: "运行其他版本", tone: "warning" };
  if (!deployment || !service)
    return { label: "运行资源不完整", tone: "warning" };
  if (report.runtimeReleaseRelation !== "matches")
    return { label: "运行版本未确认", tone: "neutral" };
  if (deployment.observedGeneration < deployment.generation)
    return { label: "等待控制器观测", tone: "progress" };
  if (deployment.desiredReplicas === 0)
    return { label: "已缩容至零", tone: "neutral" };
  if (
    deployment.conditions.some(
      (condition) =>
        (condition.type === "Progressing" && condition.status === "False") ||
        (condition.type === "ReplicaFailure" && condition.status === "True"),
    )
  )
    return { label: "工作负载异常", tone: "danger" };
  if (
    deployment.updatedReplicas < deployment.desiredReplicas ||
    deployment.readyReplicas < deployment.desiredReplicas ||
    deployment.availableReplicas < deployment.desiredReplicas
  )
    return { label: "副本尚未就绪", tone: "warning" };
  if (pods.length === 0 || pods.some((pod) => !pod.ready))
    return { label: "Pod 尚未全部就绪", tone: "warning" };
  return { label: "工作负载就绪", tone: "success" };
}

/** 列表只追踪当前活动记录；运行观测独立刷新。五分钟后或请求失败时暂停，手动刷新可重启。 */
export function pollInterval(
  until: number,
  error: unknown,
  active = true,
): number | false {
  return !error && active && Date.now() < until ? 30_000 : false;
}
