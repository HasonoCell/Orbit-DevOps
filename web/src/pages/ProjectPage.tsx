import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowRight, ChevronRight, Plus } from "lucide-react";
import { useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { createApplication, getProject, getProjectPermissions, listApplications } from "@/api/catalog";
import { EmptyState, ErrorPanel, LoadingPage } from "@/components/PageState";
import { ResourceCreateDialog, type ResourceValues } from "@/components/ResourceCreateDialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { useCommandKey } from "@/lib/use-command-key";

const roleLabel = { owner: "所有者", admin: "管理员", developer: "开发者", viewer: "观察者" } as const;

export function ProjectPage() {
  const { projectId = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const cursor = params.get("cursor") ?? undefined;
  const [open, setOpen] = useState(false);
  const commandKey = useCommandKey();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const project = useQuery({ queryKey: ["project", projectId], queryFn: () => getProject(projectId) });
  const permissions = useQuery({ queryKey: ["project-permissions", projectId], queryFn: () => getProjectPermissions(projectId) });
  const applications = useQuery({ queryKey: ["applications", projectId, cursor], queryFn: () => listApplications(projectId, cursor) });
  const create = useMutation({
    mutationFn: (values: ResourceValues) => createApplication(projectId, values.name, values.slug, commandKey.forPayload(values)),
    onSuccess(application) {
      commandKey.clear();
      void queryClient.invalidateQueries({ queryKey: ["applications", projectId] });
      setOpen(false);
      navigate(`/projects/${projectId}/applications/${application.id}`);
    },
  });
  if (project.isPending || permissions.isPending) return <LoadingPage label="正在加载项目" />;
  if (project.error) return <ErrorPanel title="无法加载项目" error={project.error} onRetry={() => void project.refetch()} />;
  if (permissions.error) return <ErrorPanel title="无法查询项目权限" error={permissions.error} onRetry={() => void permissions.refetch()} />;
  const canDevelop = permissions.data.allowed.includes("develop");
  const createButton = <Button disabled={!canDevelop} title={!canDevelop ? "当前角色没有开发权限" : undefined} onClick={() => setOpen(true)}><Plus className="size-4" />创建应用</Button>;
  return <div className="space-y-7">
    <nav aria-label="面包屑" className="flex items-center gap-2 text-sm text-muted-foreground"><Link className="hover:text-foreground" to="/projects">项目</Link><ChevronRight className="size-4" /><span className="text-foreground">{project.data.name}</span></nav>
    <div className="flex flex-wrap items-end justify-between gap-4"><div><div className="flex items-center gap-3"><h1 className="text-3xl font-semibold tracking-tight">{project.data.name}</h1><Badge variant="secondary">{roleLabel[permissions.data.role]}</Badge></div><p className="mt-2 font-mono text-sm text-muted-foreground">{project.data.slug}</p></div><div className="text-right">{createButton}{!canDevelop && <p className="mt-1 text-xs text-muted-foreground">需要开发权限</p>}</div></div>
    <div><h2 className="mb-4 text-lg font-semibold">应用</h2>{applications.isPending ? <LoadingPage label="正在加载应用" /> : applications.error ? <ErrorPanel title="无法加载应用" error={applications.error} onRetry={() => void applications.refetch()} /> : <>
      {applications.data.items.length === 0 ? <EmptyState title={cursor ? "这一页没有应用" : "还没有应用"} description={cursor ? "返回第一页重新浏览。" : "应用承载构建和交付配置；先创建应用，再配置部署目标。"} action={cursor ? <Button variant="outline" onClick={() => setParams({})}>返回第一页</Button> : canDevelop ? createButton : undefined} /> : <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">{applications.data.items.map((application) => <Link key={application.id} to={`/projects/${projectId}/applications/${application.id}`} className="group block"><Card className="h-full transition-colors group-hover:border-primary/50 group-hover:bg-accent/30"><CardContent className="flex items-start justify-between gap-4 py-2"><div className="min-w-0"><h3 className="truncate text-lg font-semibold">{application.name}</h3><p className="mt-1 truncate font-mono text-xs text-muted-foreground">{application.slug}</p><p className="mt-5 text-xs text-muted-foreground">查看应用与部署目标</p></div><ArrowRight className="mt-1 size-4 shrink-0 text-muted-foreground group-hover:text-primary" /></CardContent></Card></Link>)}</div>}
      <div className="mt-4 flex justify-end gap-2">{cursor && <Button variant="outline" onClick={() => setParams({})}>返回第一页</Button>}{applications.data.nextCursor && <Button variant="outline" onClick={() => setParams({ cursor: applications.data.nextCursor! })}>下一页</Button>}</div>
    </>}</div>
    {canDevelop && <ResourceCreateDialog kind="应用" open={open} onOpenChange={setOpen} onSubmit={(values) => create.mutate(values)} pending={create.isPending} error={create.error} />}
  </div>;
}
