import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowRight, Plus } from "lucide-react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { createProject, listProjects } from "@/api/catalog";
import { CursorPagination } from "@/components/CursorPagination";
import { EmptyState, ErrorPanel, LoadingPage } from "@/components/PageState";
import { ResourceCreateDialog } from "@/components/ResourceCreateDialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { useResourceCreateDialog } from "@/lib/use-resource-create-dialog";

export function ProjectsPage() {
  const [params, setParams] = useSearchParams();
  const cursor = params.get("cursor") ?? undefined;
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const projects = useQuery({ queryKey: ["projects", cursor], queryFn: () => listProjects(cursor) });
  const createDialog = useResourceCreateDialog(
    (values, key) => createProject(values.name, values.slug, key),
    (project) => {
      void queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate(`/projects/${project.id}`);
    },
  );
  const createButton = <Button onClick={() => createDialog.onOpenChange(true)}><Plus className="size-4" />创建项目</Button>;
  return <div className="space-y-7">
    <div className="flex flex-wrap items-end justify-between gap-4"><div><p className="text-sm text-muted-foreground">工作区 / 项目</p><h1 className="mt-1 text-3xl font-semibold tracking-tight">项目</h1><p className="mt-2 text-sm text-muted-foreground">从项目进入应用，再定位构建与发布资源。</p></div>{createButton}</div>
    {projects.isPending ? <LoadingPage label="正在加载项目" /> : projects.error ? <ErrorPanel title="无法加载项目" error={projects.error} onRetry={() => void projects.refetch()} /> : <>
      {projects.data.items.length === 0 ? <EmptyState title={cursor ? "这一页没有项目" : "还没有项目"} description={cursor ? "返回第一页重新浏览。" : "创建一个项目，将相关应用和部署目标组织到同一工作区。"} action={cursor ? <Button variant="outline" onClick={() => setParams({})}>返回第一页</Button> : createButton} /> : <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">{projects.data.items.map((project) => <Link key={project.id} to={`/projects/${project.id}`} className="group block"><Card className="h-full transition-colors group-hover:border-primary/50 group-hover:bg-accent/30"><CardContent className="flex items-start justify-between gap-4 py-2"><div className="min-w-0"><h2 className="truncate text-lg font-semibold">{project.name}</h2><p className="mt-1 truncate font-mono text-xs text-muted-foreground">{project.slug}</p><p className="mt-5 text-xs text-muted-foreground">创建于 {new Date(project.createdAt).toLocaleDateString("zh-CN")}</p></div><ArrowRight className="mt-1 size-4 shrink-0 text-muted-foreground group-hover:text-primary" /></CardContent></Card></Link>)}</div>}
      <CursorPagination cursor={cursor} nextCursor={projects.data.nextCursor} onChange={(next) => setParams(next ? { cursor: next } : {})} />
    </>}
    <ResourceCreateDialog kind="项目" {...createDialog} />
  </div>;
}
