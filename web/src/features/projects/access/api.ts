import { client, requireData } from "@/api/http";
import type { components } from "@/api/schema";

type HostInput = components["schemas"]["AccessHostInput"];
type RouteInput = components["schemas"]["AccessRouteInput"];

export const accessKeys = {
  hosts: (projectId: string, offset: number) =>
    ["access-hosts", projectId, offset] as const,
  host: (projectId: string, hostId: string) =>
    ["access-host", projectId, hostId] as const,
  status: (projectId: string, hostId: string) =>
    ["access-status", projectId, hostId] as const,
  routes: (projectId: string, hostId: string, offset: number) =>
    ["access-routes", projectId, hostId, offset] as const,
  eligible: (projectId: string, hostId: string, offset: number) =>
    ["access-targets", projectId, hostId, offset] as const,
  targetRoutes: (targetId: string, offset: number) =>
    ["target-access-routes", targetId, offset] as const,
  secretBindings: (projectId: string, hostname: string, offset: number) =>
    ["access-secret-binding-options", projectId, hostname, offset] as const,
};

export async function listSecretBindingOptions(
  projectId: string,
  hostname: string,
  offset: number,
) {
  return requireData(
    await client.GET(
      "/api/v1/projects/{projectId}/access-secret-binding-options",
      {
        params: { path: { projectId }, query: { hostname, limit: 21, offset } },
      },
    ),
    "查询可选 TLS Secret",
  );
}

export async function listHosts(projectId: string, offset: number) {
  return requireData(
    await client.GET("/api/v1/projects/{projectId}/access-hosts", {
      params: { path: { projectId }, query: { limit: 21, offset } },
    }),
    "查询访问域名",
  );
}
export async function getHost(projectId: string, hostId: string) {
  return requireData(
    await client.GET("/api/v1/projects/{projectId}/access-hosts/{hostId}", {
      params: { path: { projectId, hostId } },
    }),
    "查询访问域名",
  );
}
export async function getHostOptions(projectId: string) {
  return requireData(
    await client.GET("/api/v1/projects/{projectId}/access-host-options", {
      params: { path: { projectId } },
    }),
    "查询 TLS 可选项",
  );
}
export async function getHostStatus(projectId: string, hostId: string) {
  return requireData(
    await client.GET(
      "/api/v1/projects/{projectId}/access-hosts/{hostId}/status",
      {
        params: { path: { projectId, hostId } },
      },
    ),
    "查询入口状态",
  );
}
export async function createHost(
  projectId: string,
  body: HostInput,
  key: string,
) {
  return requireData(
    await client.POST("/api/v1/projects/{projectId}/access-hosts", {
      params: { path: { projectId }, header: { "Idempotency-Key": key } },
      body,
    }),
    "创建访问域名",
  );
}
export async function updateHost(
  projectId: string,
  hostId: string,
  body: HostInput,
  key: string,
) {
  return requireData(
    await client.PATCH("/api/v1/projects/{projectId}/access-hosts/{hostId}", {
      params: {
        path: { projectId, hostId },
        header: { "Idempotency-Key": key },
      },
      body,
    }),
    "更新访问域名",
  );
}
export async function deleteHost(
  projectId: string,
  hostId: string,
  key: string,
) {
  return requireData(
    await client.DELETE("/api/v1/projects/{projectId}/access-hosts/{hostId}", {
      params: {
        path: { projectId, hostId },
        header: { "Idempotency-Key": key },
      },
    }),
    "清理访问域名",
  );
}
export async function listRoutes(
  projectId: string,
  hostId: string,
  offset: number,
) {
  return requireData(
    await client.GET(
      "/api/v1/projects/{projectId}/access-hosts/{hostId}/routes",
      {
        params: { path: { projectId, hostId }, query: { limit: 21, offset } },
      },
    ),
    "查询路径路由",
  );
}
export async function listEligibleTargets(
  projectId: string,
  hostId: string,
  offset: number,
) {
  return requireData(
    await client.GET(
      "/api/v1/projects/{projectId}/access-hosts/{hostId}/eligible-targets",
      {
        params: { path: { projectId, hostId }, query: { limit: 21, offset } },
      },
    ),
    "查询可选部署目标",
  );
}
export async function createRoute(
  projectId: string,
  hostId: string,
  body: RouteInput,
  key: string,
) {
  return requireData(
    await client.POST(
      "/api/v1/projects/{projectId}/access-hosts/{hostId}/routes",
      {
        params: {
          path: { projectId, hostId },
          header: { "Idempotency-Key": key },
        },
        body,
      },
    ),
    "创建路径路由",
  );
}
export async function updateRoute(
  projectId: string,
  hostId: string,
  routeId: string,
  body: RouteInput,
  key: string,
) {
  return requireData(
    await client.PATCH(
      "/api/v1/projects/{projectId}/access-hosts/{hostId}/routes/{routeId}",
      {
        params: {
          path: { projectId, hostId, routeId },
          header: { "Idempotency-Key": key },
        },
        body,
      },
    ),
    "更新路径路由",
  );
}
export async function deleteRoute(
  projectId: string,
  hostId: string,
  routeId: string,
  key: string,
) {
  return requireData(
    await client.DELETE(
      "/api/v1/projects/{projectId}/access-hosts/{hostId}/routes/{routeId}",
      {
        params: {
          path: { projectId, hostId, routeId },
          header: { "Idempotency-Key": key },
        },
      },
    ),
    "清理路径路由",
  );
}
export async function listTargetRoutes(targetId: string, offset: number) {
  return requireData(
    await client.GET(
      "/api/v1/deployment-targets/{deploymentTargetId}/access-routes",
      {
        params: {
          path: { deploymentTargetId: targetId },
          query: { limit: 21, offset },
        },
      },
    ),
    "查询目标关联入口",
  );
}
