import type { QueryClient } from "@tanstack/react-query";

/** 从已知关联找出 Target，再刷新其所有分页；Host 清理可能改变后续分页的位置。 */
export function invalidateHostTargetRoutes(
  client: QueryClient,
  projectId: string,
  hostId: string,
) {
  const targetIds = new Set<string>();
  for (const [key, routes] of client.getQueriesData<{ hostId: string }[]>({
    queryKey: ["target-access-routes"],
  })) {
    if (
      typeof key[1] === "string" &&
      routes?.some((route) => route.hostId === hostId)
    )
      targetIds.add(key[1]);
  }
  for (const [, routes] of client.getQueriesData<
    { deploymentTargetId: string }[]
  >({ queryKey: ["access-routes", projectId, hostId] })) {
    routes?.forEach((route) => targetIds.add(route.deploymentTargetId));
  }
  for (const id of targetIds)
    void client.invalidateQueries({ queryKey: ["target-access-routes", id] });
}
