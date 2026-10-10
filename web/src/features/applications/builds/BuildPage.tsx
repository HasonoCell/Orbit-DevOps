import { getApplication, getProjectPermissions } from "@/api/catalog";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import {
  isActiveOperation,
  operationStatus,
  pollInterval,
} from "@/shared/overview-status";
import { Fact, QueryNotice, StatusPill, Timestamp } from "@/shared/OverviewUI";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { useObservationWindow } from "@/shared/use-observation-window";
import { useOperationRefresh } from "@/shared/use-operation-refresh";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useCallback } from "react";
import { Link, useParams } from "react-router-dom";
import { buildQueries } from "./api";
import { BuildCommands } from "./BuildCommands";
import { invalidateBuild } from "./mutations";

type Attempt = components["schemas"]["BuildAttempt"];

export function BuildPage() {
  const { projectId = "", applicationId = "", buildId = "" } = useParams();
  return (
    <BuildContent
      key={buildId}
      projectId={projectId}
      applicationId={applicationId}
      buildId={buildId}
    />
  );
}

// 资源切换时重建观察窗口；只在用户刷新或命令接纳后重新开始自动读取。
function BuildContent({
  projectId,
  applicationId,
  buildId,
}: {
  projectId: string;
  applicationId: string;
  buildId: string;
}) {
  const { until, restart: restartObservation } = useObservationWindow(buildId);
  const queryClient = useQueryClient();
  const application = useQuery({
    queryKey: ["application", applicationId],
    queryFn: () => getApplication(applicationId),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", projectId],
    queryFn: () => getProjectPermissions(projectId),
  });
  const build = useQuery({
    ...buildQueries.detail(buildId),
  });
  const operationId = build.data?.buildOperation.id ?? "";
  const operation = useQuery({
    ...buildQueries.operation(operationId),
    enabled: !!operationId,
    refetchInterval: (query) =>
      pollInterval(
        until,
        query.state.error,
        !!query.state.data && isActiveOperation(query.state.data.status),
      ),
    refetchIntervalInBackground: false,
  });
  const refreshRelated = useCallback(() => {
    invalidateBuild(queryClient, applicationId, buildId);
  }, [queryClient, applicationId, buildId]);
  useOperationRefresh(
    operation.data,
    build.data?.buildOperation,
    operation.error,
    refreshRelated,
  );
  function refresh() {
    restartObservation();
    void build.refetch();
    if (operationId) void operation.refetch();
  }
  if (application.isPending || build.isPending || permissions.isPending)
    return <LoadingPage label="正在加载构建" />;
  if (application.error)
    return (
      <ErrorPanel
        title="无法加载应用"
        error={application.error}
        onRetry={() => void application.refetch()}
      />
    );
  if (build.error)
    return (
      <ErrorPanel title="无法加载构建" error={build.error} onRetry={refresh} />
    );
  if (permissions.error)
    return (
      <ErrorPanel
        title="无法确认项目权限"
        error={permissions.error}
        onRetry={() => void permissions.refetch()}
      />
    );
  if (
    application.data.projectId !== projectId ||
    build.data.build.applicationId !== applicationId ||
    build.data.build.projectId !== projectId
  ) {
    return <EmptyState title="构建不属于当前应用" />;
  }
  const { build: record, imageArtifact } = build.data;
  const current = operation.error ? undefined : operation.data;
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div className="min-w-0">
          <Link
            to={`/projects/${projectId}/applications/${applicationId}?view=delivery`}
            className="text-xs text-muted-foreground hover:text-primary"
          >
            应用 / 构建记录
          </Link>
          <h1 className="mt-2">构建 {record.sourceCommit.slice(0, 8)}</h1>
          <p className="mt-1 break-all text-xs text-muted-foreground">
            {record.id}
          </p>
        </div>
        <Button
          variant="outline"
          onClick={refresh}
          disabled={build.isFetching || operation.isFetching}
        >
          <RefreshCw aria-hidden="true" className="size-4" />
          刷新状态
        </Button>
      </div>
      <section className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>构建输入</h2>
          <Timestamp value={record.createdAt} />
        </header>
        <dl className="grid gap-5 p-5 text-sm sm:grid-cols-2">
          <Fact label="代码仓库">
            <span className="break-all">{record.repositoryUrl}</span>
          </Fact>
          <Fact label="Commit SHA">
            <span className="break-all font-mono">{record.sourceCommit}</span>
          </Fact>
          <Fact label="Dockerfile">{record.dockerfilePath}</Fact>
          <Fact label="构建上下文">{record.contextPath}</Fact>
          <Fact label="平台">{record.platform}</Fact>
        </dl>
      </section>
      <section className="workbench-panel mb-5" id="operation">
        <header className="panel-heading">
          <h2>执行状态</h2>
          {current && <StatusPill {...operationStatus[current.status]} />}
        </header>
        {operation.isPending || operation.error ? (
          <div className="p-5">
            <QueryNotice error={operation.error} retry={refresh} />
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
              {current.recoveryRequired && (
                <p className="text-amber-800">需要人工确认执行结果。</p>
              )}
              <div className="space-y-3">
                <h3 className="font-semibold">执行尝试</h3>
                {current.attempts.length === 0 ? (
                  <p className="text-muted-foreground">尚无执行尝试</p>
                ) : (
                  current.attempts.map((attempt) => (
                    <AttemptCard
                      key={attempt.id}
                      attempt={attempt}
                      canReadLog={permissions.data.allowed.includes(
                        "read_logs",
                      )}
                    />
                  ))
                )}
              </div>
            </div>
          )
        )}
      </section>
      <section className="workbench-panel" id="artifact">
        <header className="panel-heading">
          <h2>镜像产物</h2>
        </header>
        {current?.status === "succeeded" && imageArtifact ? (
          <dl className="grid gap-4 p-5 text-sm">
            <Fact label="Digest">
              <span className="break-all font-mono">
                {imageArtifact.digest}
              </span>
            </Fact>
            <Fact label="不可变镜像引用">
              <span className="break-all font-mono">
                {imageArtifact.imageReference}
              </span>
            </Fact>
            {permissions.data.allowed.includes("develop") && (
              <div>
                <Button asChild>
                  <Link
                    to={`/projects/${projectId}/applications/${applicationId}?view=delivery&buildSource=${record.id}&createRelease=1`}
                  >
                    使用此产物发布
                  </Link>
                </Button>
              </div>
            )}
          </dl>
        ) : (
          <p className="p-5 text-sm text-muted-foreground">暂无产物</p>
        )}
      </section>
      {current && (
        <BuildCommands
          applicationId={applicationId}
          operation={current}
          buildId={buildId}
          canDevelop={permissions.data.allowed.includes("develop")}
          canResolveUnknown={permissions.data.allowed.includes(
            "resolve_unknown",
          )}
          onAccepted={restartObservation}
        />
      )}
    </div>
  );
}

function AttemptCard({
  attempt,
  canReadLog,
}: {
  attempt: Attempt;
  canReadLog: boolean;
}) {
  const log = useQuery({
    ...buildQueries.log(attempt.id),
    enabled: false,
  });
  return (
    <div className="rounded-md border p-4" id={`attempt-${attempt.id}`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <span className="font-medium">第 {attempt.number} 次尝试</span>{" "}
          <span className="ml-2 text-xs text-muted-foreground">
            {attempt.status}
          </span>
        </div>
        {canReadLog && (
          <Button
            variant="outline"
            size="sm"
            onClick={() => void log.refetch()}
            disabled={log.isFetching}
          >
            {log.isFetching
              ? "正在读取…"
              : log.data
                ? "刷新日志摘录"
                : "读取日志摘录"}
          </Button>
        )}
      </div>
      {attempt.errorSummary && (
        <p className="mt-2 break-words text-destructive">
          {attempt.errorSummary}
        </p>
      )}
      <p className="mt-2 break-all text-xs text-muted-foreground">
        Attempt {attempt.id} · Worker {attempt.workerId}
      </p>
      {log.error && (
        <ErrorPanel
          title="日志读取失败"
          error={log.error}
          onRetry={() => void log.refetch()}
        />
      )}
      {log.data && (
        <div className="mt-4 space-y-2">
          <p className="text-xs text-muted-foreground">
            日志摘录 · 本次读取于{" "}
            {new Date(log.dataUpdatedAt).toLocaleString("zh-CN")}{" "}
            {log.data.truncated ? "· 已截断" : ""}
          </p>
          <pre className="max-h-72 overflow-auto rounded bg-slate-950 p-3 text-xs text-slate-100">
            {log.data.excerpt || "（空）"}
          </pre>
        </div>
      )}
    </div>
  );
}
