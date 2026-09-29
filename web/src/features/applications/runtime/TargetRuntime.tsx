import { useQuery } from "@tanstack/react-query";
import { Activity, Box, Globe, Server } from "lucide-react";
import type { DeploymentTarget } from "@/api/http";
import {
  getDiagnostics,
  overviewQueryKeys,
  type DiagnosticReport,
} from "@/features/applications/api";
import { latestRelease } from "@/features/applications/releases/api";
import {
  Fact,
  QueryNotice,
  QuietEmpty,
  StatusPill,
  Timestamp,
} from "@/shared/OverviewUI";
import {
  isActiveOperation,
  operationStatus,
  pollInterval,
  runtimeStatus,
} from "@/shared/overview-status";
import { PodList } from "./PodList";
import { ReleaseHistory } from "@/features/applications/releases/ReleaseHistory";

/** 只观察已校验的所选目标。发布查询失败时不使用缓存继续派生运行健康。 */
export function TargetRuntime({
  target,
  applicationId,
  until,
  view,
}: {
  target: DeploymentTarget;
  applicationId: string;
  until: number;
  view: string;
}) {
  const release = useQuery({
    queryKey: overviewQueryKeys.release(applicationId, target.id),
    queryFn: () => latestRelease(target.id),
    refetchInterval: (query) =>
      pollInterval(
        until,
        query.state.error,
        !!query.state.data &&
          isActiveOperation(query.state.data.releaseOperation.status),
      ),
    refetchIntervalInBackground: false,
  });
  const releaseId = !release.error ? release.data?.release.id : undefined;
  const diagnostic = useQuery({
    queryKey: overviewQueryKeys.diagnostics(applicationId, releaseId),
    queryFn: () => getDiagnostics(releaseId!),
    enabled: !!releaseId && view !== "delivery",
    refetchInterval: (query) => pollInterval(until, query.state.error),
    refetchIntervalInBackground: false,
  });
  if (view === "delivery")
    return <ReleaseHistory target={target} applicationId={applicationId} />;
  if (release.isPending || release.error)
    return (
      <QueryNotice error={release.error} retry={() => void release.refetch()} />
    );
  if (!release.data)
    return (
      <section className="workbench-panel">
        <QuietEmpty>尚无发布记录。运行状态暂不可判断。</QuietEmpty>
        <TargetFacts target={target} />
      </section>
    );
  const report = !diagnostic.error ? diagnostic.data : undefined;
  const deployment = report?.workloadObservation.deployment;
  return (
    <>
      <dl className="runtime-summary">
        <div>
          <dt>运行状态</dt>
          <dd>
            {report ? (
              <StatusPill {...runtimeStatus(report)} />
            ) : (
              <StatusPill
                label={diagnostic.error ? "观测读取失败" : "正在观测"}
                tone="neutral"
              />
            )}
          </dd>
        </div>
        <div>
          <dt>就绪 / 期望副本</dt>
          <dd>
            {deployment ? (
              <>
                <strong>{deployment.readyReplicas}</strong> /{" "}
                {deployment.desiredReplicas}
              </>
            ) : (
              "—"
            )}
          </dd>
        </div>
        <div>
          <dt>最新发布执行结果</dt>
          <dd>
            <StatusPill
              {...operationStatus[release.data.releaseOperation.status]}
            />
          </dd>
        </div>
        <div>
          <dt>Namespace</dt>
          <dd>{target.namespace}</dd>
        </div>
      </dl>
      {diagnostic.isPending || diagnostic.error ? (
        <QueryNotice
          error={diagnostic.error}
          retry={() => void diagnostic.refetch()}
        />
      ) : (
        report && (
          <>
            {report.runtimeReleaseRelation === "different" && (
              <p className="mb-4 rounded border border-amber-200 bg-amber-50 p-3 text-sm text-amber-900">
                集群运行 Release {deployment?.releaseId ?? "未知"}
                ；下方镜像属于最新接纳的发布，并非当前运行镜像。
              </p>
            )}
            <div className="runtime-columns">
              <div>
                {view === "diagnostics" ? (
                  <DiagnosticEvidence report={report} />
                ) : view === "overview" ? (
                  <RuntimeResources report={report} />
                ) : null}
                <PodList report={report} />
                {view === "diagnostics" && <EventEvidence report={report} />}
                <section className="workbench-panel">
                  <header className="panel-heading">
                    <h2>最新接纳的发布</h2>
                    <Timestamp value={release.data.release.createdAt} />
                  </header>
                  <div className="p-5 text-xs">
                    <p className="break-all font-mono">
                      {release.data.release.id}
                    </p>
                    <details className="mt-3">
                      <summary className="cursor-pointer text-primary">
                        镜像引用与目标差异
                      </summary>
                      <p className="runtime-code mt-3">
                        {release.data.release.imageReference}
                      </p>
                      {report.targetDifferences.length ? (
                        <ul className="mt-3 space-y-2">
                          {report.targetDifferences.map((diff) => (
                            <li key={diff.field} className="break-all">
                              {diff.field}：快照 {diff.releaseValue} / 当前{" "}
                              {diff.currentValue}
                            </li>
                          ))}
                        </ul>
                      ) : (
                        <p className="mt-3 text-muted-foreground">
                          目标配置与接纳快照一致。
                        </p>
                      )}
                    </details>
                    {release.data.releaseOperation.errorSummary && (
                      <p className="mt-3 break-words text-destructive">
                        {release.data.releaseOperation.errorSummary}
                      </p>
                    )}
                  </div>
                </section>
              </div>
              <aside>
                {view !== "diagnostics" && (
                  <DiagnosticEvidence report={report} />
                )}
                <section className="workbench-panel">
                  <header className="panel-heading">
                    <h2>部署目标</h2>
                  </header>
                  <TargetFacts target={target} />
                </section>
              </aside>
            </div>
            <p className="mt-4 text-xs text-muted-foreground">
              Kubernetes 观测于{" "}
              <Timestamp
                value={report.workloadObservation.metadata.observedAt}
              />
              。Pod 就绪不等于公网访问已验证。
            </p>
          </>
        )
      )}
    </>
  );
}

function TargetFacts({ target }: { target: DeploymentTarget }) {
  return (
    <dl className="grid grid-cols-2 gap-4 p-5">
      <Fact label="Stage">{target.stage}</Fact>
      <Fact label="容器端口">{target.containerPort}</Fact>
      <Fact label="集群">{target.clusterRef}</Fact>
      <Fact label="期望副本">{target.replicas}</Fact>
      <div className="col-span-2">
        <Fact label="Target ID">{target.id}</Fact>
      </div>
    </dl>
  );
}

function RuntimeResources({ report }: { report: DiagnosticReport }) {
  const { deployment, service, metadata } = report.workloadObservation;
  return (
    <section className="workbench-panel">
      <header className="panel-heading">
        <h2>目标资源</h2>
        <span className="text-xs text-muted-foreground">Kubernetes 观测</span>
      </header>
      <div className="runtime-resource-grid">
        <div className="runtime-resource">
          <h3>
            <Box className="size-4" aria-hidden="true" />
            Deployment
          </h3>
          {deployment ? (
            <>
              <p className="font-medium">{deployment.name}</p>
              <p>
                <strong>{deployment.readyReplicas}</strong> /{" "}
                {deployment.desiredReplicas} Ready
              </p>
              <p>
                已更新 {deployment.updatedReplicas} · 可用{" "}
                {deployment.availableReplicas}
              </p>
            </>
          ) : (
            <p>
              {metadata.status === "complete"
                ? "未观测到 Deployment"
                : "观测不完整，不能确认资源是否存在"}
            </p>
          )}
        </div>
        <div className="runtime-resource">
          <h3>
            <Globe className="size-4" aria-hidden="true" />
            Service
          </h3>
          {service ? (
            <>
              <p className="font-medium">{service.name}</p>
              <p>
                {service.ports
                  .map((port) => `${port.port}/${port.protocol}`)
                  .join("，")}
              </p>
              <p>
                {service.ownershipMatches ? "资源归属匹配" : "资源归属冲突"}
              </p>
            </>
          ) : (
            <p>
              {metadata.status === "complete"
                ? "未观测到 Service"
                : "观测不完整，不能确认资源是否存在"}
            </p>
          )}
        </div>
      </div>
      <p className="border-t bg-muted/40 px-5 py-3 text-xs text-muted-foreground">
        同一目标关联的资源，不表示流量或 owner 链。
      </p>
    </section>
  );
}

function DiagnosticEvidence({ report }: { report: DiagnosticReport }) {
  const incomplete = report.workloadObservation.metadata.status !== "complete";
  return (
    <section
      className="runtime-evidence"
      data-attention={report.signals.some(
        (signal) => signal.severity === "error",
      )}
    >
      <h2>
        <Activity className="size-4" aria-hidden="true" />
        运行诊断
      </h2>
      {incomplete && (
        <p className="mt-3 text-amber-900">
          工作负载观测
          {report.workloadObservation.metadata.status === "partial"
            ? "不完整"
            : "不可用"}
          ，不能据此确认运行健康。
        </p>
      )}
      {report.workloadObservation.metadata.errorCategories.length > 0 && (
        <p className="mt-2">
          观测限制：
          {report.workloadObservation.metadata.errorCategories.join("、")}
        </p>
      )}
      {report.signals.length ? (
        <ul className="mt-4 space-y-4">
          {report.signals.map((signal, index) => (
            <li key={index}>
              <StatusPill
                label={
                  signal.severity === "error"
                    ? "错误"
                    : signal.severity === "warning"
                      ? "注意"
                      : "信息"
                }
                tone={
                  signal.severity === "error"
                    ? "danger"
                    : signal.severity === "warning"
                      ? "warning"
                      : "neutral"
                }
              />
              <p className="mt-2">{signal.summary}</p>
              <details className="mt-1 text-xs">
                <summary className="cursor-pointer text-primary">
                  信号与证据
                </summary>
                <code className="mt-2 block">{signal.code}</code>
                {signal.evidenceRefs.map((ref, position) => (
                  <p key={position}>
                    {ref.source} / {ref.kind} / {ref.id}
                  </p>
                ))}
              </details>
            </li>
          ))}
        </ul>
      ) : (
        <p className="mt-3">
          此次诊断没有返回信号；请结合运行状态、观测完整性和时间判断。
        </p>
      )}
      {report.eventObservation.metadata.status !== "complete" && (
        <p className="mt-3 text-amber-900">集群事件读取不完整。</p>
      )}
    </section>
  );
}

function EventEvidence({ report }: { report: DiagnosticReport }) {
  return (
    <section className="workbench-panel">
      <header className="panel-heading">
        <h2>集群事件</h2>
        <Server aria-hidden="true" className="size-4" />
      </header>
      <p className="px-5 pt-4 text-xs text-muted-foreground">
        观测于 <Timestamp value={report.eventObservation.metadata.observedAt} />
      </p>
      {report.eventObservation.items.length ? (
        <ul className="divide-y">
          {report.eventObservation.items.map((event) => (
            <li key={event.uid} className="space-y-2 p-5 text-xs">
              <div className="flex flex-wrap gap-2">
                <StatusPill
                  label={event.type}
                  tone={event.type === "Warning" ? "warning" : "neutral"}
                />
                <strong>{event.reason}</strong>
                <span>× {event.count}</span>
              </div>
              <p className="break-all">
                {event.resourceKind} / {event.resourceName}
              </p>
              <p className="break-words leading-6">{event.message}</p>
              <Timestamp value={event.lastSeen} />
            </li>
          ))}
        </ul>
      ) : (
        <QuietEmpty>
          {report.eventObservation.metadata.status === "complete"
            ? "此次观测没有返回事件。"
            : "事件观测不可用或不完整，不能判断是否存在事件。"}
        </QuietEmpty>
      )}
    </section>
  );
}
