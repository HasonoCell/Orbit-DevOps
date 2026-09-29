import { useIsFetching, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";
import {
  Link,
  useNavigate,
  useParams,
  useSearchParams,
} from "react-router-dom";
import {
  getApplication,
  getProject,
  getProjectPermissions,
  listDeploymentTargets,
} from "@/api/catalog";
import type { Application, Project } from "@/api/http";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { Button } from "@/components/ui/button";
import { BuildSummary } from "@/features/applications/delivery/BuildSummary";
import { PipelineSummary } from "@/features/applications/delivery/PipelineSummary";
import { QueryNotice } from "@/shared/OverviewUI";
import { TargetRuntime } from "@/features/applications/runtime/TargetRuntime";
import { ReleaseCreateDialog } from "@/features/applications/releases/ReleaseCreateDialog";
import { TargetCreateDialog } from "@/features/applications/targets/TargetCreateDialog";
import { BuildCreateDialog } from "@/features/applications/builds/BuildCreateDialog";
import { overviewQueryKeys } from "@/features/applications/api";
import { workbenchQueryKeys } from "@/features/projects/workbench/api";

export function ApplicationPage() {
  const { projectId = "", applicationId = "" } = useParams();
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => getProject(projectId),
  });
  const application = useQuery({
    queryKey: ["application", applicationId],
    queryFn: () => getApplication(applicationId),
  });
  if (project.isPending || application.isPending)
    return <LoadingPage label="正在加载应用" />;
  if (project.error)
    return (
      <ErrorPanel
        title="无法加载项目"
        error={project.error}
        onRetry={() => void project.refetch()}
      />
    );
  if (application.error)
    return (
      <ErrorPanel
        title="无法加载应用"
        error={application.error}
        onRetry={() => void application.refetch()}
      />
    );
  // 层级校验通过后才允许挂载权限、目标和交付子查询。
  if (application.data.projectId !== projectId)
    return (
      <ErrorPanel
        title="应用不属于该项目"
        error={new Error("请从应用所属的项目重新进入。")}
      />
    );
  return (
    <ApplicationOverview
      key={applicationId}
      application={application.data}
      project={project.data}
    />
  );
}

function ApplicationOverview({
  application,
  project,
}: {
  application: Application;
  project: Project;
}) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const [until, setUntil] = useState(() => Date.now() + 5 * 60_000);
  const [paused, setPaused] = useState(false);
  const [releaseOpen, setReleaseOpen] = useState(false);
  const [createStage, setCreateStage] = useState<
    "development" | "production" | null
  >(null);
  const [buildOpen, setBuildOpen] = useState(false);
  const [accepted, setAccepted] = useState("");
  const fetching =
    useIsFetching({ queryKey: overviewQueryKeys.root(application.id) }) > 0;
  const targets = useQuery({
    queryKey: overviewQueryKeys.targets(application.id),
    queryFn: () => listDeploymentTargets(application.id),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", project.id],
    queryFn: () => getProjectPermissions(project.id),
  });
  const requestedTarget = params.get("target");
  const target = targets.error
    ? undefined
    : requestedTarget
      ? targets.data?.find((item) => item.id === requestedTarget)
      : targets.data?.[0];
  const view = ["overview", "workloads", "delivery", "diagnostics"].includes(
    params.get("view") ?? "",
  )
    ? params.get("view")!
    : "overview";
  const canDevelop =
    !permissions.error &&
    permissions.data?.allowed.includes("develop") === true;
  useEffect(() => {
    const timer = window.setTimeout(
      () => setPaused(true),
      Math.max(0, until - Date.now()),
    );
    return () => window.clearTimeout(timer);
  }, [until]);
  function refresh() {
    setPaused(false);
    setUntil(Date.now() + 5 * 60_000);
    void queryClient.invalidateQueries({
      queryKey: overviewQueryKeys.root(application.id),
    });
  }
  function changeTarget(value: string) {
    setAccepted("");
    setParams((previous) => {
      const next = new URLSearchParams(previous);
      if (value) next.set("target", value);
      else next.delete("target");
      next.delete("releaseCursor");
      return next;
    });
  }
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div className="min-w-0">
          <Link
            className="text-xs text-muted-foreground hover:text-primary"
            to={`/projects/${project.id}`}
          >
            {project.name} / 应用
          </Link>
          <h1 className="mt-2">{application.name}</h1>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {targets.data && !targets.error && targets.data.length > 0 && (
            <>
              <label
                htmlFor="runtime-target"
                className="text-xs text-muted-foreground"
              >
                部署目标
              </label>
              <select
                id="runtime-target"
                className="runtime-select"
                value={target?.id ?? ""}
                onChange={(event) => changeTarget(event.target.value)}
              >
                {!target && (
                  <option value="" disabled>
                    目标不存在
                  </option>
                )}
                {targets.data.map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.stage} ·{" "}
                    {item.stage === "production" ? "生产环境" : "开发环境"}
                  </option>
                ))}
              </select>
            </>
          )}
          <Button variant="outline" onClick={refresh} disabled={fetching}>
            <RefreshCw
              aria-hidden="true"
              className={`size-4 ${fetching ? "animate-spin" : ""}`}
            />
            刷新概览
          </Button>
          {canDevelop && (
            <Button variant="outline" onClick={() => setBuildOpen(true)}>
              <Plus aria-hidden="true" className="size-4" />
              手动构建
            </Button>
          )}
          <Button
            disabled={!target || !canDevelop}
            title={
              !canDevelop
                ? "需要已确认的开发权限"
                : !target
                  ? "请先选择有效目标"
                  : undefined
            }
            onClick={() => setReleaseOpen(true)}
          >
            <Plus aria-hidden="true" className="size-4" />
            新建发布
          </Button>
        </div>
      </div>
      {permissions.error && (
        <ErrorPanel
          title="无法确认发布权限，写操作已禁用"
          error={permissions.error}
          onRetry={() => void permissions.refetch()}
        />
      )}
      {permissions.data && !canDevelop && (
        <p className="mb-3 text-xs text-muted-foreground">
          当前角色仅可查看；新建发布需要开发权限。
        </p>
      )}
      {accepted && (
        <p
          role="status"
          className="mb-4 rounded border border-primary/20 bg-accent p-3 text-sm"
        >
          发布已接纳：{accepted}。部署进度请查看执行状态。
        </p>
      )}
      {targets.data && !targets.error && (
        <section
          className="mb-5 rounded-lg border bg-card p-4"
          aria-label="部署目标配置"
        >
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h2 className="text-sm font-semibold">部署目标</h2>
              <p className="mt-1 text-xs text-muted-foreground">
                每个环境独立配置，发布时记录配置快照。
              </p>
            </div>
            <div className="flex flex-wrap gap-2">
              {(["development", "production"] as const).map((stage) => {
                const existing = targets.data.find(
                  (item) => item.stage === stage,
                );
                const label = stage === "development" ? "开发" : "生产";
                return existing ? (
                  <Button key={stage} variant="outline" asChild>
                    <Link
                      to={`/projects/${project.id}/applications/${application.id}/targets/${existing.id}`}
                    >
                      {label}目标配置
                    </Link>
                  </Button>
                ) : canDevelop ? (
                  <Button
                    key={stage}
                    variant="outline"
                    onClick={() => setCreateStage(stage)}
                  >
                    <Plus aria-hidden="true" className="size-4" />
                    创建{label}目标
                  </Button>
                ) : null;
              })}
            </div>
          </div>
        </section>
      )}
      <nav className="workbench-tabs" aria-label="应用视图">
        {[
          ["overview", "运行总览"],
          ["workloads", "工作负载"],
          ["delivery", "交付记录"],
          ["diagnostics", "运行诊断"],
        ].map(([key, label]) => (
          <button
            key={key}
            aria-pressed={view === key}
            onClick={() =>
              setParams((previous) => {
                const next = new URLSearchParams(previous);
                next.set("view", key);
                return next;
              })
            }
          >
            {label}
          </button>
        ))}
      </nav>
      <p className="my-4 text-xs text-muted-foreground">
        {paused
          ? "自动刷新已暂停，点击刷新查看最新状态"
          : "活动记录与运行观测每 30 秒刷新 · 持续 5 分钟"}
      </p>
      {targets.isPending || targets.error ? (
        <QueryNotice
          error={targets.error}
          retry={() => void targets.refetch()}
        />
      ) : requestedTarget && !target ? (
        <EmptyState
          title="部署目标不属于该应用或已不可用"
          description="不会按 URL 中未校验的目标读取诊断。请选择有效目标。"
          action={
            <Button variant="outline" onClick={() => changeTarget("")}>
              选择默认目标
            </Button>
          }
        />
      ) : !target ? (
        <EmptyState
          title="还没有部署目标"
          description="部署目标将应用关联到集群、命名空间和运行环境。"
        />
      ) : (
        <TargetRuntime
          key={`runtime-${target.id}`}
          target={target}
          applicationId={application.id}
          until={until}
          view={view}
        />
      )}
      {(view === "overview" || view === "delivery") && (
        <section className="mt-7">
          <h2 className="mb-4 text-sm font-semibold">交付活动</h2>
          <div className="grid items-start gap-5 xl:grid-cols-2">
            <PipelineSummary applicationId={application.id} until={until} />
            <BuildSummary applicationId={application.id} until={until} />
          </div>
        </section>
      )}
      {target && canDevelop && (
        <ReleaseCreateDialog
          key={`release-${target.id}`}
          target={target}
          application={application}
          open={releaseOpen}
          onOpenChange={setReleaseOpen}
          onAccepted={(id) => {
            setAccepted(id);
            refresh();
            void queryClient.invalidateQueries({
              queryKey: workbenchQueryKeys.project(project.id),
            });
            navigate(
              `/projects/${project.id}/applications/${application.id}/releases/${id}`,
            );
          }}
        />
      )}
      {createStage && (
        <TargetCreateDialog
          key={createStage}
          projectId={project.id}
          applicationId={application.id}
          stage={createStage}
          open
          onOpenChange={(open) => {
            if (!open) setCreateStage(null);
          }}
        />
      )}
      {canDevelop && (
        <BuildCreateDialog
          projectId={project.id}
          applicationId={application.id}
          open={buildOpen}
          onOpenChange={setBuildOpen}
        />
      )}
      <details className="mt-6 border-t pt-4 text-xs text-muted-foreground">
        <summary className="cursor-pointer">应用标识</summary>
        <p className="mt-2 break-all">
          {application.slug} / {application.id}
        </p>
      </details>
    </div>
  );
}
