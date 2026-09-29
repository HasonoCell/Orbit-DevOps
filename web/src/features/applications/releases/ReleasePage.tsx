import { useQuery } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  getApplication,
  getDeploymentTarget,
  getProjectPermissions,
} from "@/api/catalog";
import { errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { getDiagnostics } from "@/features/applications/api";
import { PodList } from "@/features/applications/runtime/PodList";
import {
  DiagnosticEvidence,
  EventEvidence,
  RuntimeResources,
} from "@/features/applications/runtime/TargetRuntime";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { Fact, QueryNotice, StatusPill, Timestamp } from "@/shared/OverviewUI";
import {
  isActiveOperation,
  operationStatus,
  pollInterval,
} from "@/shared/overview-status";
import {
  getRelease,
  getReleaseOperation,
  getRuntimeLogs,
  releaseQueryKeys,
} from "./api";
import type { DiagnosticReport } from "@/features/applications/api";

export function ReleasePage() {
  const { projectId = "", applicationId = "", releaseId = "" } = useParams();
  const [until, setUntil] = useState(() => Date.now() + 5 * 60_000);
  const application = useQuery({
    queryKey: ["application", applicationId],
    queryFn: () => getApplication(applicationId),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", projectId],
    queryFn: () => getProjectPermissions(projectId),
  });
  const detail = useQuery({
    queryKey: releaseQueryKeys.detail(releaseId),
    queryFn: () => getRelease(releaseId),
    refetchInterval: (query) =>
      pollInterval(
        until,
        query.state.error,
        !!query.state.data &&
          isActiveOperation(query.state.data.releaseOperation.status),
      ),
  });
  const targetId = detail.data?.release.deploymentTargetId ?? "";
  const target = useQuery({
    queryKey: ["deployment-target", targetId],
    queryFn: () => getDeploymentTarget(targetId),
    enabled: !!targetId,
  });
  const operationId = detail.data?.releaseOperation.id ?? "";
  const operation = useQuery({
    queryKey: releaseQueryKeys.operation(operationId),
    queryFn: () => getReleaseOperation(operationId),
    enabled: !!operationId,
    refetchInterval: (query) =>
      pollInterval(
        until,
        query.state.error,
        !!query.state.data && isActiveOperation(query.state.data.status),
      ),
  });
  const report = useQuery({
    queryKey: releaseQueryKeys.diagnostics(releaseId),
    queryFn: () => getDiagnostics(releaseId),
    enabled: !!detail.data && !detail.error,
    refetchInterval: (query) => pollInterval(until, query.state.error),
  });
  if (
    application.isPending ||
    permissions.isPending ||
    detail.isPending ||
    target.isPending
  )
    return <LoadingPage label="正在加载发布" />;
  if (application.error)
    return (
      <ErrorPanel
        title="无法加载应用"
        error={application.error}
        onRetry={() => void application.refetch()}
      />
    );
  if (permissions.error)
    return (
      <ErrorPanel
        title="无法确认项目权限"
        error={permissions.error}
        onRetry={() => void permissions.refetch()}
      />
    );
  if (detail.error)
    return (
      <ErrorPanel
        title="无法加载发布"
        error={detail.error}
        onRetry={() => void detail.refetch()}
      />
    );
  if (target.error)
    return (
      <ErrorPanel
        title="无法加载部署目标"
        error={target.error}
        onRetry={() => void target.refetch()}
      />
    );
  if (
    application.data.projectId !== projectId ||
    detail.data.release.targetSnapshot.projectId !== projectId ||
    detail.data.release.targetSnapshot.applicationId !== applicationId ||
    target.data?.applicationId !== applicationId
  ) {
    return (
      <EmptyState
        title="发布不属于当前应用"
        description="请从发布所属的应用重新进入。"
      />
    );
  }
  const { release, snapshotDifferences, auditTimeline } = detail.data;
  const current = operation.error ? undefined : operation.data;
  const relation = {
    matches: "运行版本与本发布一致",
    different: "运行版本与本发布不同",
    absent: "未观测到运行版本",
    unknown: "运行版本尚无法判断",
  } as const;
  function refresh() {
    setUntil(Date.now() + 5 * 60_000);
    void detail.refetch();
    void operation.refetch();
    void report.refetch();
  }
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div className="min-w-0">
          <Link
            to={`/projects/${projectId}/applications/${applicationId}?view=delivery&target=${targetId}`}
            className="text-xs text-muted-foreground hover:text-primary"
          >
            应用 / 发布记录
          </Link>
          <h1 className="mt-2">
            {release.targetSnapshot.stage === "production"
              ? "生产发布"
              : "开发发布"}
          </h1>
          <p className="mt-1 break-all text-xs text-muted-foreground">
            Release {release.id}
          </p>
        </div>
        <Button variant="outline" onClick={refresh}>
          <RefreshCw className="size-4" aria-hidden="true" />
          刷新状态
        </Button>
      </div>
      <section className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>发布记录</h2>
          <Timestamp value={release.createdAt} />
        </header>
        <dl className="grid gap-5 p-5 text-sm sm:grid-cols-2">
          <Fact label="接纳时间">
            <Timestamp value={release.createdAt} />
          </Fact>
          <Fact label="目标">
            {release.targetSnapshot.stage} · {target.data?.clusterRef} /{" "}
            {target.data?.namespace}
          </Fact>
          <Fact label="快照副本 / 容器端口">
            {release.targetSnapshot.replicas} /{" "}
            {release.targetSnapshot.containerPort}
          </Fact>
          <Fact label="镜像引用">
            <span className="break-all font-mono">
              {release.imageReference}
            </span>
          </Fact>
          {release.rollbackOfReleaseId && (
            <Fact label="回滚来源">
              <Link
                className="text-primary hover:underline"
                to={`/projects/${projectId}/applications/${applicationId}/releases/${release.rollbackOfReleaseId}`}
              >
                {release.rollbackOfReleaseId}
              </Link>
            </Fact>
          )}
        </dl>
        <div className="border-t px-5 py-4 text-sm">
          <h3 className="font-semibold">当前 Target 与发布快照</h3>
          {snapshotDifferences.length ? (
            <ul className="mt-2 space-y-1 text-muted-foreground">
              {snapshotDifferences.map((diff) => (
                <li key={diff.field}>
                  {diff.field}：快照 {diff.releaseValue} / 当前{" "}
                  {diff.currentValue}
                </li>
              ))}
            </ul>
          ) : (
            <p className="mt-2 text-muted-foreground">配置相同</p>
          )}
        </div>
      </section>
      <section id="operation" className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>执行状态</h2>
          {current && <StatusPill {...operationStatus[current.status]} />}
        </header>
        {operation.isPending || operation.error ? (
          <div className="p-5">
            <QueryNotice
              error={operation.error}
              retry={() => void operation.refetch()}
            />
          </div>
        ) : (
          current && (
            <div className="space-y-4 p-5 text-sm">
              <p className="break-all text-xs text-muted-foreground">
                Operation {current.id}
              </p>
              {current.errorSummary && (
                <p
                  role="alert"
                  className="rounded border border-amber-300 bg-amber-50 p-3 text-amber-950"
                >
                  {current.errorCode ? `${current.errorCode}：` : ""}
                  {current.errorSummary}
                </p>
              )}
              <h3 className="font-semibold">执行尝试</h3>
              {current.attempts.length ? (
                <ul className="space-y-3">
                  {current.attempts.map((attempt) => (
                    <li
                      key={attempt.id}
                      id={`attempt-${attempt.id}`}
                      className="rounded border p-3"
                    >
                      <p>
                        第 {attempt.number} 次 · {attempt.status}
                      </p>
                      <p className="break-all text-xs text-muted-foreground">
                        Attempt {attempt.id} · Worker {attempt.workerId}
                      </p>
                      {attempt.errorSummary && (
                        <p className="mt-2 text-destructive">
                          {attempt.errorSummary}
                        </p>
                      )}
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="text-muted-foreground">尚无执行尝试</p>
              )}
            </div>
          )
        )}
      </section>
      <section className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>审计时间线</h2>
        </header>
        {auditTimeline.length ? (
          <ul className="divide-y">
            {auditTimeline.map((record) => (
              <li
                key={record.id}
                className="flex flex-wrap justify-between gap-2 px-5 py-3 text-sm"
              >
                <span>
                  {record.action} · {record.actorId}
                </span>
                <Timestamp value={record.createdAt} />
              </li>
            ))}
          </ul>
        ) : (
          <p className="p-5 text-sm text-muted-foreground">暂无审计记录</p>
        )}
      </section>
      <section className="mb-5" aria-label="运行观测">
        <div className="mb-3 flex items-center justify-between gap-3">
          <h2 className="text-sm font-semibold">Kubernetes 运行观测</h2>
          <span className="text-xs text-muted-foreground">
            独立于发布执行结果
          </span>
        </div>
        {report.isPending || report.error ? (
          <QueryNotice
            error={report.error}
            retry={() => void report.refetch()}
          />
        ) : (
          report.data && (
            <div className="space-y-5">
              <p className="rounded border bg-card p-4 text-sm">
                {relation[report.data.runtimeReleaseRelation]} · 工作负载观测{" "}
                {report.data.workloadObservation.metadata.status} · 事件观测{" "}
                {report.data.eventObservation.metadata.status}
              </p>
              <div className="runtime-columns">
                <div className="space-y-5">
                  <RuntimeResources report={report.data} />
                  <PodList report={report.data} />
                  <EventEvidence report={report.data} />
                </div>
                <div className="space-y-5">
                  <DiagnosticEvidence report={report.data} />
                  <RuntimeLogs
                    releaseId={releaseId}
                    report={report.data}
                    canRead={permissions.data.allowed.includes("read_logs")}
                  />
                </div>
              </div>
              <p className="text-xs text-muted-foreground">
                观测于{" "}
                <Timestamp
                  value={report.data.workloadObservation.metadata.observedAt}
                />
                ；Pod 就绪不等于公网访问已验证。
              </p>
            </div>
          )
        )}
      </section>
    </div>
  );
}

function RuntimeLogs({
  releaseId,
  report,
  canRead,
}: {
  releaseId: string;
  report: DiagnosticReport;
  canRead: boolean;
}) {
  const [selected, setSelected] = useState<{
    podName: string;
    container: string;
  } | null>(null);
  const logs = useQuery({
    queryKey: releaseQueryKeys.logs(
      releaseId,
      selected?.podName ?? "",
      selected?.container ?? "",
    ),
    queryFn: () =>
      getRuntimeLogs(releaseId, selected!.podName, selected!.container),
    enabled: !!selected && canRead,
  });
  return (
    <section className="workbench-panel">
      <header className="panel-heading">
        <h2>运行日志摘录</h2>
      </header>
      <div className="space-y-3 p-5 text-sm">
        {!canRead ? (
          <p className="text-muted-foreground">当前角色不能读取运行日志。</p>
        ) : report.workloadObservation.pods.length === 0 ? (
          <p className="text-muted-foreground">未观测到 Pod，暂无可选日志。</p>
        ) : (
          <div className="flex flex-wrap gap-2">
            {report.workloadObservation.pods.flatMap((pod) =>
              pod.containers.map((container) => (
                <Button
                  key={`${pod.uid}/${container.name}`}
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    if (
                      selected?.podName === pod.name &&
                      selected.container === container.name
                    )
                      void logs.refetch();
                    else
                      setSelected({
                        podName: pod.name,
                        container: container.name,
                      });
                  }}
                >
                  读取 {pod.name} / {container.name} 日志
                </Button>
              )),
            )}
          </div>
        )}
        {logs.error && (
          <p role="alert" className="text-destructive">
            {errorText(logs.error)}
          </p>
        )}
        {logs.data && (
          <div className="space-y-2">
            <p className="text-xs text-muted-foreground">
              {logs.data.source} · 观测于{" "}
              <Timestamp value={logs.data.observedAt} />{" "}
              {logs.data.truncated ? "· 已截断" : ""}
            </p>
            <pre className="max-h-72 overflow-auto rounded bg-slate-950 p-3 text-xs text-slate-100">
              {logs.data.content || "（空）"}
            </pre>
          </div>
        )}
      </div>
    </section>
  );
}
