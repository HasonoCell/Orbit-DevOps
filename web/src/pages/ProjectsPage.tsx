import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { createProject, listProjects } from "@/api/catalog";
import { EmptyState, ErrorPanel, LoadingPage } from "@/components/PageState";
import { ResourceCreateDialog } from "@/components/ResourceCreateDialog";
import { ResourceList } from "@/components/ResourceList";
import { Button } from "@/components/ui/button";
import { useResourceCreateDialog } from "@/lib/use-resource-create-dialog";

export function ProjectsPage() {
  const [params] = useSearchParams();
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
    <p className="text-xs text-muted-foreground">工作区 / 项目</p>
    <div className="flex flex-wrap items-center justify-between gap-4"><div><h1 className="text-[26px] font-semibold tracking-tight">项目</h1><p className="mt-2 text-sm text-muted-foreground">管理应用、构建与部署，从一个项目开始。</p></div>{createButton}</div>
    <div className="border-b"><span className="inline-block border-b-2 border-primary px-1 pb-3 text-sm font-medium">我的项目</span></div>
    {projects.isPending ? <LoadingPage label="正在加载项目" /> : projects.error ? <ErrorPanel title="无法加载项目" error={projects.error} onRetry={() => void projects.refetch()} /> :
      projects.data.items.length === 0 && !cursor ? <EmptyState title="还没有项目" description="创建一个项目，将相关应用和部署目标组织到同一工作区。" action={createButton} /> :
      <ResourceList items={projects.data.items} nextCursor={projects.data.nextCursor} kind="项目" href={(project) => `/projects/${project.id}`} />}
    <ResourceCreateDialog kind="项目" {...createDialog} />
  </div>;
}
