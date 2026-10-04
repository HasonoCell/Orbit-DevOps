import { getProject, getProjectPermissions } from "@/api/catalog";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { useQuery } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { accessQueries } from "./api";

import { HostCreate } from "./HostCreate";
import { HostDetail } from "./HostDetail";

/** 路由边界只核验项目、权限和资源归属，再组合相应功能。 */
export function AccessHostPage() {
  const { projectId = "", hostId = "" } = useParams();
  const creating = hostId === "new";
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => getProject(projectId),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", projectId],
    queryFn: () => getProjectPermissions(projectId),
  });
  const host = useQuery({
    ...accessQueries.host(projectId, hostId),
    enabled: !creating,
  });
  if (
    project.isPending ||
    permissions.isPending ||
    (!creating && host.isPending)
  )
    return <LoadingPage label="正在加载访问域名" />;
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
        title="无法确认项目权限"
        error={permissions.error}
        onRetry={() => void permissions.refetch()}
      />
    );
  if (!creating && host.error)
    return (
      <ErrorPanel
        title="无法加载访问域名"
        error={host.error}
        onRetry={() => void host.refetch()}
      />
    );
  if (!creating && host.data?.projectId !== projectId)
    return (
      <EmptyState
        title="域名不属于该项目"
        description="请从所属项目重新进入。"
      />
    );
  if (creating) {
    if (!permissions.data.allowed.includes("manage_access_hosts"))
      return (
        <EmptyState
          title="当前角色不能创建访问域名"
          description="请联系项目管理员确认入口管理权限。"
        />
      );
    return <HostCreate projectId={projectId} projectName={project.data.name} />;
  }
  return (
    <HostDetail
      key={hostId}
      projectId={projectId}
      host={host.data!}
      canManageHost={permissions.data.allowed.includes("manage_access_hosts")}
      canManageRoutes={permissions.data.allowed.includes(
        "manage_access_routes",
      )}
    />
  );
}
