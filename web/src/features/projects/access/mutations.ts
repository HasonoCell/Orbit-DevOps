import {
  useMutation,
  useQueryClient,
  type QueryClient,
} from "@tanstack/react-query";
import type { components } from "@/api/schema";
import { useCommandKey } from "@/shared/use-command-key";
import { invalidateHostTargetRoutes } from "./target-route-cache";
import {
  accessKeys,
  createHost,
  updateHost,
  deleteHost,
  createRoute,
  updateRoute,
  deleteRoute,
} from "./api";

type Host = components["schemas"]["AccessHost"];
type Route = components["schemas"]["AccessRoute"];

/** Host 的关联入口覆盖相关 Target 的全部分页，不干扰其他项目。 */
export function invalidateHost(
  client: QueryClient,
  projectId: string,
  hostId: string,
) {
  void client.invalidateQueries({ queryKey: ["access-hosts", projectId] });
  void client.invalidateQueries({
    queryKey: accessKeys.host(projectId, hostId),
  });
  void client.invalidateQueries({
    queryKey: accessKeys.status(projectId, hostId),
  });
  invalidateHostTargetRoutes(client, projectId, hostId);
}

export function useWriteHost(
  projectId: string,
  hostId: string | undefined,
  onAccepted: (host: Host) => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: (body: Parameters<typeof createHost>[1]) => {
      const token = key.forPayload({ projectId, hostId, ...body });
      return hostId
        ? updateHost(projectId, hostId, body, token)
        : createHost(projectId, body, token);
    },
    onSuccess(host) {
      key.clear();
      client.setQueryData(accessKeys.host(projectId, host.id), host);
      invalidateHost(client, projectId, host.id);
      onAccepted(host);
    },
  });
}

export function useDeleteHost(
  projectId: string,
  hostId: string,
  onAccepted: () => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: () =>
      deleteHost(
        projectId,
        hostId,
        key.forPayload({ projectId, hostId, action: "delete" }),
      ),
    onSuccess(host) {
      key.clear();
      client.setQueryData(accessKeys.host(projectId, hostId), host);
      invalidateHost(client, projectId, hostId);
      onAccepted();
    },
  });
}

function invalidateRoute(
  client: QueryClient,
  projectId: string,
  hostId: string,
  targetIds: string[],
) {
  void client.invalidateQueries({
    queryKey: ["access-routes", projectId, hostId],
  });
  void client.invalidateQueries({
    queryKey: accessKeys.status(projectId, hostId),
  });
  for (const id of new Set(targetIds))
    void client.invalidateQueries({ queryKey: ["target-access-routes", id] });
}

export function useWriteRoute(
  projectId: string,
  hostId: string,
  editing: Route | null,
  onAccepted: () => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: (body: Parameters<typeof createRoute>[2]) => {
      const token = key.forPayload({ hostId, routeId: editing?.id, ...body });
      return editing
        ? updateRoute(projectId, hostId, editing.id, body, token)
        : createRoute(projectId, hostId, body, token);
    },
    onSuccess(route) {
      key.clear();
      invalidateRoute(client, projectId, hostId, [
        route.deploymentTargetId,
        ...(editing ? [editing.deploymentTargetId] : []),
      ]);
      onAccepted();
    },
  });
}

export function useDeleteRoute(
  projectId: string,
  hostId: string,
  onAccepted: (route: Route) => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: (routeId: string) =>
      deleteRoute(
        projectId,
        hostId,
        routeId,
        key.forPayload({ hostId, routeId, action: "delete" }),
      ),
    onSuccess(route) {
      key.clear();
      invalidateRoute(client, projectId, hostId, [route.deploymentTargetId]);
      onAccepted(route);
    },
  });
}
