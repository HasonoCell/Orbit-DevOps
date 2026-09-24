import { useIsFetching, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, Layers3, RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { getApplication, getProject, listDeploymentTargets } from "@/api/catalog";
import type { Application, Project } from "@/api/http";
import { EmptyState, ErrorPanel, LoadingPage } from "@/components/PageState";
import { Button } from "@/components/ui/button";
import { BuildSummary } from "@/components/overview/BuildSummary";
import { PipelineSummary } from "@/components/overview/PipelineSummary";
import { TargetSummary } from "@/components/overview/TargetSummary";
import { QueryNotice } from "@/components/overview/OverviewUI";

export function ApplicationPage() {
  const { projectId = "", applicationId = "" } = useParams();
  const project = useQuery({ queryKey: ["project", projectId], queryFn: () => getProject(projectId) });
  const application = useQuery({ queryKey: ["application", applicationId], queryFn: () => getApplication(applicationId) });
  if (project.isPending || application.isPending) return <LoadingPage label="正在加载应用" />;
  if (project.error) return <ErrorPanel title="无法加载项目" error={project.error} onRetry={() => void project.refetch()} />;
  if (application.error) return <ErrorPanel title="无法加载应用" error={application.error} onRetry={() => void application.refetch()} />;
  // 确认层级归属之后才挂载子查询，避免错误 URL 发起另一项目的概览请求。
  if (application.data.projectId !== projectId) return <ErrorPanel title="应用不属于该项目" error={new Error("请从应用所属的项目重新进入。")} />;
  return <ApplicationOverview key={applicationId} application={application.data} project={project.data} />;
}

function ApplicationOverview({ application, project }: { application: Application; project: Project }) {
  const queryClient = useQueryClient();
  const [until, setUntil] = useState(() => Date.now() + 5 * 60_000);
  const [paused, setPaused] = useState(false);
  const fetching = useIsFetching({ queryKey: ["application-overview", application.id] }) > 0;
  const targets = useQuery({
    queryKey: ["application-overview", application.id, "targets"],
    queryFn: () => listDeploymentTargets(application.id),
  });
  useEffect(() => {
    const timer = window.setTimeout(() => setPaused(true), Math.max(0, until - Date.now()));
    return () => window.clearTimeout(timer);
  }, [until]);
  function refresh() {
    setPaused(false);
    setUntil(Date.now() + 5 * 60_000);
    void queryClient.invalidateQueries({ queryKey: ["application-overview", application.id] });
  }
  return <div className="space-y-6">
    <nav aria-label="面包屑" className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground"><Link className="hover:text-foreground" to="/projects">项目</Link><ChevronRight className="size-3" /><Link className="max-w-52 truncate hover:text-foreground" to={`/projects/${project.id}`}>{project.name}</Link><ChevronRight className="size-3" /><span className="break-all text-foreground">{application.name}</span></nav>
    <div className="flex flex-wrap items-center justify-between gap-4">
      <div className="min-w-0"><h1 className="break-all text-[26px] font-semibold tracking-tight">{application.name}</h1><p className="mt-2 break-all font-mono text-xs text-muted-foreground">{application.slug}</p></div>
      <Button variant="outline" onClick={refresh} disabled={fetching}><RefreshCw className={`size-4 ${fetching ? "animate-spin" : ""}`} />刷新概览</Button>
    </div>
    <div className="flex flex-wrap items-center justify-between gap-3 border-b">
      <h2 className="border-b-2 border-primary px-1 pb-3 text-sm font-medium">应用概览</h2>
      <p className="pb-3 text-xs text-muted-foreground">{paused ? "自动刷新已暂停，点击刷新查看最新状态" : "活动记录与运行观测每 30 秒刷新 · 持续 5 分钟"}</p>
    </div>
    <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,0.9fr)_minmax(0,1.25fr)]">
      <div className="min-w-0 space-y-4"><div className="flex items-center gap-2 text-sm font-semibold"><Layers3 className="size-4 text-muted-foreground" /><h2>部署环境</h2></div>
        {targets.isPending || targets.error ? <div className="rounded-lg border"><QueryNotice error={targets.error} retry={() => void targets.refetch()} /></div> : targets.data.length === 0 ? <EmptyState title="还没有部署目标" description="部署目标将应用关联到集群、命名空间和运行环境。" /> : targets.data.map((target) => <TargetSummary key={target.id} target={target} applicationId={application.id} until={until} />)}
      </div>
      <div className="min-w-0 space-y-4"><h2 className="text-sm font-semibold">交付活动</h2><PipelineSummary applicationId={application.id} until={until} /><BuildSummary applicationId={application.id} until={until} /></div>
    </div>
    <p className="break-all border-t pt-4 text-xs text-muted-foreground">Application ID · <span className="font-mono">{application.id}</span></p>
  </div>;
}
