import { client, requireData } from "@/api/http";

export const releaseQueryKeys = {
  artifacts: (applicationId: string, cursor?: string) =>
    ["release-artifacts", applicationId, cursor] as const,
  history: (applicationId: string, targetId: string, cursor?: string) =>
    [
      "application-overview",
      applicationId,
      "history",
      targetId,
      cursor,
    ] as const,
};

export async function latestRelease(deploymentTargetId: string) {
  const page = requireData(
    await client.GET(
      "/api/v1/deployment-targets/{deploymentTargetId}/releases",
      {
        params: { path: { deploymentTargetId }, query: { limit: 1 } },
      },
    ),
    "查询发布",
  );
  return page.items[0] ?? null;
}

export async function listReleaseHistory(
  deploymentTargetId: string,
  cursor?: string,
) {
  return requireData(
    await client.GET(
      "/api/v1/deployment-targets/{deploymentTargetId}/releases",
      {
        params: { path: { deploymentTargetId }, query: { limit: 5, cursor } },
      },
    ),
    "查询发布历史",
  );
}

export async function acceptRelease(
  deploymentTargetId: string,
  body: { imageReference: string; imageArtifactId?: string },
  idempotencyKey: string,
) {
  return requireData(
    await client.POST(
      "/api/v1/deployment-targets/{deploymentTargetId}/releases",
      {
        params: {
          path: { deploymentTargetId },
          header: { "Idempotency-Key": idempotencyKey },
        },
        body,
      },
    ),
    "接纳发布",
  );
}
