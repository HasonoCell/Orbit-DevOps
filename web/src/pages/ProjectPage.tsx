import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, Plus } from "lucide-react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { createApplication, getProject, getProjectPermissions, listApplications } from "@/api/catalog";
import { EmptyState, ErrorPanel, LoadingPage } from "@/components/PageState";
import { ResourceCreateDialog } from "@/components/ResourceCreateDialog";
import { ResourceList } from "@/components/ResourceList";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useResourceCreateDialog } from "@/lib/use-resource-create-dialog";

const roleLabel = { owner: "所有者", admin: "管理员", developer: "开发者", viewer: "观察者" } as const;

export function ProjectPage() {
  const { projectId = "" } = useParams();
  const [params] = useSearchParams();
  const cursor = params.get("cursor") ?? undefined;
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const project = useQuery({ queryKey: ["project", projectId], queryFn: () => getProject(projectId) });
  const permissions = useQuery({ queryKey: ["project-permissions", projectId], queryFn: () => getProjectPermissions(projectId) });
  const applications = useQuery({ queryKey: ["applications", projectId, cursor], queryFn: () => listApplications(projectId, cursor) });
  const createDialog = useResourceCreateDialog(
    (values, key) => createApplication(projectId, values.name, values.slug, key),
    (application) => {
      void queryClient.invalidateQueries({ queryKey: ["applications", projectId] });
      navigate(`/projects/${projectId}/applications/${application.id}`);
    },
  );
  if (project.isPending || permissions.isPending) return <LoadingPage label="正在加载项目" />;
  if (project.error) return <ErrorPanel title="无法加载项目" error={project.error} onRetry={() => void project.refetch()} />;
  if (permissions.error) return <ErrorPanel title="无法查询项目权限" error={permissions.error} onRetry={() => void permissions.refetch()} />;
  const canDevelop = permissions.data.allowed.includes("develop");
  const createButton = <Button disabled={!canDevelop} title={!canDevelop ? "当前角色没有开发权限" : undefined} onClick={() => createDialog.onOpenChange(true)}><Plus className="size-4" />创建应用</Button>;
  return <div className="space-y-7">
    <nav aria-label="面包屑" className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground"><Link className="hover:text-foreground" to="/projects">项目</Link><ChevronRight className="size-3 shrink-0" /><span className="truncate text-foreground">{project.data.name}</span></nav>
    <div className="flex flex-wrap items-center justify-between gap-4"><div className="min-w-0"><div className="flex flex-wrap items-center gap-3"><h1 className="break-all text-[26px] font-semibold tracking-tight">{project.data.name}</h1><Badge variant="secondary">{roleLabel[permissions.data.role]}</Badge></div><p className="mt-2 break-all text-sm text-muted-foreground">项目内的应用与交付工作区</p></div><div>{createButton}{!canDevelop && <p className="mt-1 text-xs text-muted-foreground">需要开发权限</p>}</div></div>
    <div className="border-b"><h2 className="inline-block border-b-2 border-primary px-1 pb-3 text-sm font-medium">应用</h2></div>
    <div className="grid gap-8 lg:grid-cols-[220px_minmax(0,1fr)]">
      <aside className="self-start rounded-lg border bg-muted/40 p-5 lg:border-0 lg:bg-transparent lg:p-0"><h2 className="mb-5 text-sm font-semibold">项目资料</h2><dl className="grid gap-5 text-sm sm:grid-cols-2 lg:grid-cols-1"><Metadata label="项目标识" value={project.data.slug} /><Metadata label="我的角色" value={roleLabel[permissions.data.role]} /><Metadata label="创建时间" value={new Date(project.data.createdAt).toLocaleDateString("zh-CN")} /><Metadata label="项目 ID" value={project.data.id} /></dl></aside>
      <div className="min-w-0">{applications.isPending ? <LoadingPage label="正在加载应用" /> : applications.error ? <ErrorPanel title="无法加载应用" error={applications.error} onRetry={() => void applications.refetch()} /> :
        applications.data.items.length === 0 && !cursor ? <EmptyState title="还没有应用" description="应用承载构建和交付配置；先创建应用，再配置部署目标。" action={canDevelop ? createButton : undefined} /> :
        <ResourceList items={applications.data.items} nextCursor={applications.data.nextCursor} kind="应用" href={(application) => `/projects/${projectId}/applications/${application.id}`} />}</div>
    </div>
    {canDevelop && <ResourceCreateDialog kind="应用" {...createDialog} />}
  </div>;
}

function Metadata({ label, value }: { label: string; value: string }) {
  return <div className="min-w-0"><dt className="text-xs text-muted-foreground">{label}</dt><dd className="mt-1.5 break-all text-sm">{value}</dd></div>;
}
