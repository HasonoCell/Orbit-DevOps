import { useQuery } from "@tanstack/react-query";
import { Server } from "lucide-react";
import type { DeploymentTarget } from "@/api/http";
import { getDiagnostics, latestRelease } from "@/api/overview";
import { Fact, Panel, QueryNotice, QuietEmpty, StatusPill, Timestamp } from "./OverviewUI";
import { isActiveOperation, operationStatus, pollInterval, runtimeStatus } from "@/lib/overview-status";

export function TargetSummary({ target, applicationId, until }: { target: DeploymentTarget; applicationId: string; until: number }) {
  const release = useQuery({
    queryKey: ["application-overview", applicationId, "release", target.id],
    queryFn: () => latestRelease(target.id),
    refetchInterval: (query) => pollInterval(until, query.state.error, !!query.state.data && isActiveOperation(query.state.data.releaseOperation.status)),
    refetchIntervalInBackground: false,
  });
  return <Panel title={target.stage === "production" ? "生产环境" : "开发环境"} icon={<Server className="size-4" />} subtitle={target.stage}>
    <dl className="grid grid-cols-2 gap-4 px-5 py-5"><Fact label="集群">{target.clusterRef}</Fact><Fact label="命名空间">{target.namespace}</Fact><Fact label="期望副本">{target.replicas}</Fact><Fact label="容器端口">{target.containerPort}</Fact></dl>
    <div className="border-t">
      {release.isPending || release.error ? <QueryNotice error={release.error} retry={() => void release.refetch()} /> : !release.data ? <QuietEmpty>尚无发布记录。运行状态暂不可判断。</QuietEmpty> : <>
        <div className="space-y-3 px-5 py-4">
          <div className="flex flex-wrap items-center justify-between gap-2"><h3 className="text-xs font-medium text-muted-foreground">最新接纳的发布</h3><StatusPill {...operationStatus[release.data.releaseOperation.status]} /></div>
          <p className="break-all font-mono text-xs leading-5">{release.data.release.imageReference}</p>
          <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted-foreground"><span title={release.data.release.id}>Release {release.data.release.id.slice(0, 8)}</span><Timestamp value={release.data.release.createdAt} /></p>
          {release.data.releaseOperation.errorSummary && <p className="break-words text-xs text-destructive">{release.data.releaseOperation.errorSummary}</p>}
        </div>
        <RuntimeSummary key={release.data.release.id} releaseId={release.data.release.id} applicationId={applicationId} until={until} />
      </>}
    </div>
  </Panel>;
}

/** 诊断独立于发布操作查询；终态发布仍需要观察后续 Pod 故障。失败时不展示缓存中的绿色健康徽标。 */
function RuntimeSummary({ releaseId, applicationId, until }: { releaseId: string; applicationId: string; until: number }) {
  const diagnostic = useQuery({
    queryKey: ["application-overview", applicationId, "diagnostics", releaseId],
    queryFn: () => getDiagnostics(releaseId),
    refetchInterval: (query) => pollInterval(until, query.state.error),
    refetchIntervalInBackground: false,
  });
  if (diagnostic.isPending || diagnostic.error) return <div className="border-t"><QueryNotice error={diagnostic.error} retry={() => void diagnostic.refetch()} /></div>;
  const report = diagnostic.data;
  const { metadata, deployment, pods } = report.workloadObservation;
  const status = runtimeStatus(report);
  return <div className="space-y-4 border-t bg-muted/30 px-5 py-4">
    <div className="flex flex-wrap items-center justify-between gap-2"><h3 className="text-xs font-medium text-muted-foreground">运行观测</h3><StatusPill {...status} /></div>
    {deployment && <div className="flex flex-wrap gap-x-6 gap-y-2 text-xs"><span>就绪 <strong className="text-base font-semibold">{deployment.readyReplicas}</strong> / {deployment.desiredReplicas} 副本</span><span>可用 {deployment.availableReplicas} · 已更新 {deployment.updatedReplicas}</span></div>}
    {report.runtimeReleaseRelation === "different" && <p className="break-all text-xs text-amber-800">集群运行 Release {deployment?.releaseId ?? "未知"}；上方镜像属于最新接纳的发布，并非当前运行镜像。</p>}
    <p className="text-xs text-muted-foreground">观测于 <Timestamp value={metadata.observedAt} /> · Kubernetes</p>
    <details className="text-xs">
      <summary className="cursor-pointer font-medium text-primary">诊断摘要{report.signals.length > 0 && ` · ${report.signals.length} 条信号`}</summary>
      <div className="mt-3 space-y-3">
        {metadata.errorCategories.length > 0 && <p className="break-all text-amber-800">观测限制：{metadata.errorCategories.join("、")}</p>}
        {report.signals.map((signal, index) => <p key={index} className="break-words leading-5">{signal.summary}</p>)}
        {pods.map((pod) => <div key={pod.uid} className="rounded border bg-background p-3"><p className="break-all font-mono">{pod.name}</p><p className="mt-1 text-muted-foreground">{pod.ready ? "Ready" : "Not Ready"} · {pod.phase}{pod.reason && ` · ${pod.reason}`}</p>{pod.containers.filter((container) => container.reason).map((container) => <p className="mt-1 break-all text-amber-800" key={container.name}>{container.name} · {container.reason}</p>)}</div>)}
        {pods.length === 0 && <p className="text-muted-foreground">此次观测未返回 Pod。</p>}
        {report.eventObservation.metadata.status !== "complete" && <p className="text-amber-800">事件观测不完整；不代表没有集群事件。</p>}
      </div>
    </details>
  </div>;
}
