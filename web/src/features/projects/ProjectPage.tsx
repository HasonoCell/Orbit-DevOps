import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  createApplication,
  getProject,
  getProjectPermissions,
} from "@/api/catalog";
import type { Project } from "@/api/http";
import {
  listWorkbench,
  workbenchQueryKeys,
} from "@/features/projects/workbench/api";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { ResourceCreateDialog } from "@/features/projects/ResourceCreateDialog";
import { ProjectWorkbench } from "@/features/projects/workbench/ProjectWorkbench";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useResourceCreateDialog } from "@/features/projects/use-resource-create-dialog";
import { AccessHostsPanel } from "@/features/projects/access/AccessHostsPanel";
import { ProjectMembersPanel } from "@/features/projects/members/ProjectMembersPanel";

const roleLabel = {
  owner: "所有者",
  admin: "管理员",
  developer: "开发者",
  viewer: "观察者",
} as const;

export function ProjectPage() {
  const { projectId = "" } = useParams();
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => getProject(projectId),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", projectId],
    queryFn: () => getProjectPermissions(projectId),
  });
  if (project.isPending || permissions.isPending)
    return <LoadingPage label="正在加载项目" />;
  if (project.error)
    return (
      <ErrorPanel
        title="无法加载项目"
        error={project.error}
        onRetry={() => void project.refetch()}
      />
    );
  if (permissions.error)
    return (
      <ErrorPanel
        title="无法查询项目权限"
        error={permissions.error}
        onRetry={() => void permissions.refetch()}
      />
    );
  return (
    <ProjectContent
      key={projectId}
      project={project.data}
      role={roleLabel[permissions.data.role]}
      canDevelop={permissions.data.allowed.includes("develop")}
      canManageHosts={permissions.data.allowed.includes("manage_access_hosts")}
      canManageMembers={permissions.data.allowed.includes("manage_members")}
      canManageOwners={permissions.data.allowed.includes("manage_owners")}
    />
  );
}

function ProjectContent({
  project,
  role,
  canDevelop,
  canManageHosts,
  canManageMembers,
  canManageOwners,
}: {
  project: Project;
  role: string;
  canDevelop: boolean;
  canManageHosts: boolean;
  canManageMembers: boolean;
  canManageOwners: boolean;
}) {
  const [params, setParams] = useSearchParams();
  const view =
    params.get("view") === "delivery"
      ? "delivery"
      : params.get("view") === "access"
        ? "access"
        : params.get("view") === "members"
          ? "members"
          : "overview";
  const cursor = params.get("cursor") ?? undefined;
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const applications = useQuery({
    queryKey: workbenchQueryKeys.page(project.id, cursor),
    queryFn: ({ signal }) => listWorkbench(project.id, cursor, signal),
    enabled: view !== "access" && view !== "members",
  });
  const createDialog = useResourceCreateDialog(
    (values, key) =>
      createApplication(project.id, values.name, values.slug, key),
    (application) => {
      void queryClient.invalidateQueries({
        queryKey: workbenchQueryKeys.project(project.id),
      });
      navigate(`/projects/${project.id}/applications/${application.id}`);
    },
  );
  const createButton = (
    <Button
      disabled={!canDevelop}
      title={!canDevelop ? "当前角色没有开发权限" : undefined}
      onClick={() => createDialog.onOpenChange(true)}
    >
      <Plus aria-hidden="true" className="size-4" />
      创建应用
    </Button>
  );
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div className="flex min-w-0 items-center gap-3">
          <span className="project-emblem" aria-hidden="true">
            {project.name.slice(0, 1)}
          </span>
          <div className="min-w-0">
            <h1>{project.name}</h1>
            <p className="mt-1 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
              应用交付与运行管理 <Badge variant="secondary">{role}</Badge>
            </p>
          </div>
        </div>
        <div>
          {createButton}
          {!canDevelop && (
            <p className="mt-1 text-xs text-muted-foreground">需要开发权限</p>
          )}
        </div>
      </div>
      <nav className="workbench-tabs" aria-label="项目视图">
        {[
          ["overview", "应用总览"],
          ["delivery", "自动交付"],
          ["access", "访问入口"],
          ["members", "项目成员"],
        ].map(([view, label]) => (
          <button
            key={view}
            aria-pressed={
              params.get("view") === view ||
              (view === "overview" &&
                !["delivery", "access", "members"].includes(
                  params.get("view") ?? "",
                ))
            }
            onClick={() =>
              setParams((previous) => {
                const next = new URLSearchParams(previous);
                next.set("view", view);
                return next;
              })
            }
          >
            {label}
          </button>
        ))}
      </nav>
      {view === "access" ? (
        <AccessHostsPanel projectId={project.id} canManage={canManageHosts} />
      ) : view === "members" ? (
        // 管理权限变化时丢弃候选人和操作草稿，避免降权后残留旧表单。
        <ProjectMembersPanel
          key={`${project.id}:${canManageMembers}:${canManageOwners}`}
          projectId={project.id}
          canManage={canManageMembers}
          canManageOwners={canManageOwners}
        />
      ) : applications.isPending ? (
        <LoadingPage label="正在加载应用" />
      ) : applications.error ? (
        <ErrorPanel
          title="无法加载应用"
          error={applications.error}
          onRetry={() => void applications.refetch()}
        />
      ) : applications.data.items.length === 0 && !cursor ? (
        <EmptyState
          title="还没有应用"
          description="应用承载构建和交付配置；先创建应用，再配置部署目标。"
          action={canDevelop ? createButton : undefined}
        />
      ) : (
        <ProjectWorkbench
          project={project}
          page={applications.data}
          cursor={cursor}
          isFetching={applications.isFetching}
          dataUpdatedAt={applications.dataUpdatedAt}
          onRefresh={() => void applications.refetch()}
        />
      )}
      {canDevelop && <ResourceCreateDialog kind="应用" {...createDialog} />}
    </div>
  );
}
