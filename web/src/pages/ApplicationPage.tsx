import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Layers3 } from "lucide-react";
import { Link, useParams } from "react-router-dom";
import { getApplication, getProject, listDeploymentTargets } from "@/api/catalog";
import { EmptyState, ErrorPanel, LoadingPage } from "@/components/PageState";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export function ApplicationPage() {
  const { projectId = "", applicationId = "" } = useParams();
  const project = useQuery({ queryKey: ["project", projectId], queryFn: () => getProject(projectId) });
  const application = useQuery({ queryKey: ["application", applicationId], queryFn: () => getApplication(applicationId) });
  const targets = useQuery({ queryKey: ["deployment-targets", applicationId], queryFn: () => listDeploymentTargets(applicationId) });
  if (project.isPending || application.isPending) return <LoadingPage label="正在加载应用" />;
  if (project.error) return <ErrorPanel title="无法加载项目" error={project.error} onRetry={() => void project.refetch()} />;
  if (application.error) return <ErrorPanel title="无法加载应用" error={application.error} onRetry={() => void application.refetch()} />;
  // URL 中的 Project 与应用实际归属必须一致，避免用不匹配的层级显示资源。
  if (application.data.projectId !== projectId) return <ErrorPanel title="应用不属于该项目" error={new Error("请从应用所属的项目重新进入。")}/>;
  return <div className="space-y-7">
    <nav aria-label="面包屑" className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground"><Link className="hover:text-foreground" to="/projects">项目</Link><ChevronRight className="size-4" /><Link className="hover:text-foreground" to={`/projects/${projectId}`}>{project.data.name}</Link><ChevronRight className="size-4" /><span className="text-foreground">{application.data.name}</span></nav>
    <div><p className="text-sm text-muted-foreground">应用概览</p><h1 className="mt-1 text-3xl font-semibold tracking-tight">{application.data.name}</h1><p className="mt-2 font-mono text-sm text-muted-foreground">{application.data.slug}</p></div>
    <div><div className="mb-4 flex items-center gap-2"><Layers3 className="size-5 text-primary" /><h2 className="text-lg font-semibold">部署目标</h2></div>
      {targets.isPending ? <LoadingPage label="正在加载部署目标" /> : targets.error ? <ErrorPanel title="无法加载部署目标" error={targets.error} onRetry={() => void targets.refetch()} /> : targets.data.length === 0 ? <EmptyState title="还没有部署目标" description="部署目标将应用关联到具体的集群、命名空间和运行阶段。" /> : <div className="grid gap-4 md:grid-cols-2">{targets.data.map((target) => <Card key={target.id}><CardHeader className="flex flex-row items-center justify-between gap-3"><CardTitle className="text-base">{target.stage === "production" ? "生产阶段" : "开发阶段"}</CardTitle><Badge variant="outline">{target.stage}</Badge></CardHeader><CardContent className="grid grid-cols-2 gap-4 text-sm"><Detail label="集群" value={target.clusterRef} /><Detail label="命名空间" value={target.namespace} /><Detail label="副本数" value={String(target.replicas)} /><Detail label="容器端口" value={String(target.containerPort)} /></CardContent></Card>)}</div>}
    </div>
  </div>;
}

function Detail({ label, value }: { label: string; value: string }) {
  return <div className="min-w-0"><p className="text-xs text-muted-foreground">{label}</p><p className="mt-1 break-all font-medium">{value}</p></div>;
}
