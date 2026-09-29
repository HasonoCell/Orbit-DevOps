import { client, requireData } from "@/api/http";
import type { components } from "@/api/schema";

type MemberRole = components["schemas"]["ProjectRole"];
type Lookup = components["schemas"]["ResolveProjectMemberCandidateRequest"];

export const memberKeys = {
  page: (projectId: string, cursor?: string) =>
    ["project-members", projectId, cursor] as const,
};

export async function listMembers(projectId: string, cursor?: string) {
  return requireData(
    await client.GET("/api/v1/projects/{projectId}/members", {
      params: { path: { projectId }, query: { limit: 20, cursor } },
    }),
    "查询项目成员",
  );
}

export async function resolveMember(projectId: string, body: Lookup) {
  return requireData(
    await client.POST("/api/v1/projects/{projectId}/member-candidate:resolve", {
      params: { path: { projectId } },
      body,
    }),
    "查找用户",
  );
}

export async function addMember(
  projectId: string,
  userId: string,
  role: MemberRole,
  key: string,
) {
  return requireData(
    await client.POST("/api/v1/projects/{projectId}/members", {
      params: { path: { projectId }, header: { "Idempotency-Key": key } },
      body: { userId, role },
    }),
    "添加项目成员",
  );
}

export async function updateMember(
  projectId: string,
  userId: string,
  role: MemberRole,
  key: string,
) {
  return requireData(
    await client.PUT("/api/v1/projects/{projectId}/members/{userId}", {
      params: {
        path: { projectId, userId },
        header: { "Idempotency-Key": key },
      },
      body: { role },
    }),
    "修改成员角色",
  );
}

export async function removeMember(
  projectId: string,
  userId: string,
  key: string,
) {
  return requireData(
    await client.DELETE("/api/v1/projects/{projectId}/members/{userId}", {
      params: {
        path: { projectId, userId },
        header: { "Idempotency-Key": key },
      },
    }),
    "移除项目成员",
  );
}
