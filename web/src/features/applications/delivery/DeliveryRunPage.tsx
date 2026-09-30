import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { getApplication, getProjectPermissions } from "@/api/catalog";
import { errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { Fact, StatusPill, Timestamp } from "@/shared/OverviewUI";
import { isActiveRun, pollInterval, runStatus } from "@/shared/overview-status";
import { useCommandKey } from "@/shared/use-command-key";
import { DeliveryStages } from "./DeliveryStages";
import {
  getPipeline,
  getRun,
  pipelineKeys,
  reconcileRun,
} from "./pipeline-api";

const stageLabel = {
  build: "构建",
  source_verification: "来源校验",
  release: "部署",
} as const;

export function DeliveryRunPage() {
  const {
    projectId = "",
    applicationId = "",
    pipelineId = "",
    runId = "",
  } = useParams();
  return (
    <DeliveryRunContent
      key={runId}
      projectId={projectId}
      applicationId={applicationId}
      pipelineId={pipelineId}
      runId={runId}
    />
  );
}

function DeliveryRunContent({
  projectId,
  applicationId,
  pipelineId,
  runId,
}: {
  projectId: string;
  applicationId: string;
  pipelineId: string;
  runId: string;
}) {
  const [until, setUntil] = useState(() => Date.now() + 5 * 60_000);
  const [confirm, setConfirm] = useState(false);
  const [accepted, setAccepted] = useState(false);
  const application = useQuery({
    queryKey: ["application", applicationId],
    queryFn: () => getApplication(applicationId),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", projectId],
    queryFn: () => getProjectPermissions(projectId),
  });
  const pipeline = useQuery({
    queryKey: pipelineKeys.detail(pipelineId),
    queryFn: () => getPipeline(pipelineId),
  });
  const run = useQuery({
    queryKey: ["delivery-run", runId],
    queryFn: () => getRun(runId),
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      // 重新对账的 202 可能仍是阻塞状态，需要短期观察后台推进，终态仍停止。
      return pollInterval(
        until,
        query.state.error,
        !!status &&
          (isActiveRun(status) ||
            (accepted &&
              (status === "blocked" || status === "attention_required"))),
      );
    },
  });
  const commandKey = useCommandKey();
  const queryClient = useQueryClient();
  const reconcile = useMutation({
    mutationFn: () => reconcileRun(runId, commandKey.forPayload({ runId })),
    onSuccess(result) {
      commandKey.clear();
      queryClient.setQueryData(["delivery-run", runId], result);
      setConfirm(false);
      setAccepted(true);
      setUntil(Date.now() + 5 * 60_000);
    },
  });
  function refresh() {
    setAccepted(false);
    setUntil(Date.now() + 5 * 60_000);
    void run.refetch();
  }
  if (
    application.isPending ||
    permissions.isPending ||
    pipeline.isPending ||
    run.isPending
  )
    return <LoadingPage label="正在加载交付运行" />;
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
        title="无法确认权限"
        error={permissions.error}
        onRetry={() => void permissions.refetch()}
      />
    );
  if (pipeline.error)
    return (
      <ErrorPanel
        title="无法加载 Pipeline"
        error={pipeline.error}
        onRetry={() => void pipeline.refetch()}
      />
    );
  if (run.error)
    return (
      <ErrorPanel
        title="无法加载交付运行"
        error={run.error}
        onRetry={refresh}
      />
    );
  if (
    application.data.projectId !== projectId ||
    pipeline.data.pipeline.projectId !== projectId ||
    pipeline.data.pipeline.applicationId !== applicationId ||
    run.data.run.deliveryPipelineId !== pipelineId
  )
    return (
      <EmptyState
        title="交付运行不属于当前 Pipeline"
        description="请从所属 Pipeline 重新进入。"
      />
    );
  const detail = run.data;
  const canReconcile =
    permissions.data.allowed.includes("develop") &&
    (detail.status === "attention_required" || detail.status === "blocked");
  const buildOnly = detail.mode === "build_only";
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div className="min-w-0">
          <Link
            className="text-xs text-muted-foreground hover:text-primary"
            to={`/projects/${projectId}/applications/${applicationId}/pipelines/${pipelineId}`}
          >
            {pipeline.data.pipeline.name} / 运行历史
          </Link>
          <h1 className="mt-2">
            交付运行 {detail.run.sourceCommit.slice(0, 8)}
          </h1>
          <p className="mt-1 break-all text-xs text-muted-foreground">
            Run {detail.run.id}
          </p>
        </div>
        <Button variant="outline" onClick={refresh} disabled={run.isFetching}>
          <RefreshCw className="size-4" aria-hidden="true" />
          刷新运行
        </Button>
      </div>
      <section className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>交付状态</h2>
          <StatusPill {...runStatus[detail.status]} />
        </header>
        <div className="space-y-5 p-5 text-sm">
          <DeliveryStages run={detail} buildOnly={buildOnly} />
          <dl className="grid gap-4 sm:grid-cols-2">
            <Fact label="当前阶段">
              {detail.activeStage
                ? stageLabel[detail.activeStage]
                : "无活动阶段"}
            </Fact>
            <Fact label="来源 Commit">
              <span className="break-all font-mono">
                {detail.run.sourceCommit}
              </span>
            </Fact>
            <Fact label="触发事件">
              {detail.trigger.eventType} · {detail.trigger.repositoryFullName}
            </Fact>
            <Fact label="分支">{detail.trigger.gitRef}</Fact>
            <Fact label="触发时间">
              <Timestamp value={detail.trigger.receivedAt} />
            </Fact>
            <Fact label="Pipeline Revision">{detail.run.pipelineRevision}</Fact>
          </dl>
          {detail.run.reasonCode && (
            <p
              role="alert"
              className="rounded border border-amber-300 bg-amber-50 p-3 text-amber-950"
            >
              原因：{detail.run.reasonCode}
            </p>
          )}
          {detail.run.pipelineRevision !== pipeline.data.revision.revision && (
            <p className="text-xs text-muted-foreground">
              此运行使用历史 Revision {detail.run.pipelineRevision}。
            </p>
          )}
        </div>
      </section>
      <section className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>关联资源</h2>
        </header>
        <dl className="grid gap-5 p-5 text-sm sm:grid-cols-2">
          <Fact label="构建">
            <Link
              className="text-primary hover:underline"
              to={`/projects/${projectId}/applications/${applicationId}/builds/${detail.run.buildId}`}
            >
              查看 Build {detail.run.buildId}
            </Link>
          </Fact>
          <Fact label="产物">
            {detail.run.imageArtifactId ? (
              <Link
                className="text-primary hover:underline"
                to={`/projects/${projectId}/applications/${applicationId}/builds/${detail.run.buildId}#artifact`}
              >
                查看 Artifact {detail.run.imageArtifactId}
              </Link>
            ) : (
              "尚未生成"
            )}
          </Fact>
          <Fact label="开发发布">
            {detail.run.releaseId ? (
              <Link
                className="text-primary hover:underline"
                to={`/projects/${projectId}/applications/${applicationId}/releases/${detail.run.releaseId}`}
              >
                查看 Release {detail.run.releaseId}
              </Link>
            ) : (
              "尚未创建"
            )}
          </Fact>
        </dl>
      </section>
      {canReconcile && (
        <section className="workbench-panel p-5 text-sm">
          <h2 className="font-semibold">人工处理</h2>
          <p className="mt-2 text-muted-foreground">
            重新对账只登记一次后台推进请求；底层 Build、Release
            和发布门禁仍分别判断。
          </p>
          {confirm ? (
            <div className="mt-4 space-y-3 rounded border p-4">
              <p>确认重新对账 Run {runId}？</p>
              {reconcile.error && (
                <p role="alert" className="text-destructive">
                  {errorText(reconcile.error)}
                  。结果未确认时，相同输入重试会复用幂等键。
                </p>
              )}
              <div className="flex gap-2">
                <Button
                  disabled={reconcile.isPending}
                  onClick={() => reconcile.mutate()}
                >
                  {reconcile.isPending ? "正在提交…" : "确认重新对账"}
                </Button>
                <Button
                  variant="outline"
                  disabled={reconcile.isPending}
                  onClick={() => setConfirm(false)}
                >
                  返回
                </Button>
              </div>
            </div>
          ) : (
            <Button
              className="mt-4"
              variant="outline"
              onClick={() => {
                reconcile.reset();
                setConfirm(true);
              }}
            >
              重新对账
            </Button>
          )}
        </section>
      )}
      {accepted && (
        <p role="status" className="mt-4 rounded border bg-accent p-4 text-sm">
          推进请求已接纳，请稍后刷新运行状态。
        </p>
      )}
    </div>
  );
}
