import {
  client,
  requireData,
  type Application,
  type ApplicationPage,
  type DeploymentTarget,
  type Project,
  type ProjectPage,
  type ProjectPermissions,
} from "./http";

export async function listProjects(cursor?: string): Promise<ProjectPage> {
  return requireData(
    await client.GET("/api/v1/projects", {
      params: { query: { limit: 20, cursor } },
    }),
    "查询项目",
  );
}

export async function getProject(projectId: string): Promise<Project> {
  return requireData(
    await client.GET("/api/v1/projects/{projectId}", {
      params: { path: { projectId } },
    }),
    "查询项目",
  );
}

export async function createProject(
  name: string,
  slug: string,
  idempotencyKey: string,
): Promise<Project> {
  return requireData(
    await client.POST("/api/v1/projects", {
      params: { header: { "Idempotency-Key": idempotencyKey } },
      body: { name, slug },
    }),
    "创建项目",
  );
}

export async function getProjectPermissions(
  projectId: string,
): Promise<ProjectPermissions> {
  return requireData(
    await client.GET("/api/v1/projects/{projectId}/permissions", {
      params: { path: { projectId } },
    }),
    "查询项目权限",
  );
}

export async function listApplications(
  projectId: string,
  cursor?: string,
  limit = 20,
): Promise<ApplicationPage> {
  return requireData(
    await client.GET("/api/v1/projects/{projectId}/applications", {
      params: { path: { projectId }, query: { limit, cursor } },
    }),
    "查询应用",
  );
}

export async function getApplication(
  applicationId: string,
): Promise<Application> {
  return requireData(
    await client.GET("/api/v1/applications/{applicationId}", {
      params: { path: { applicationId } },
    }),
    "查询应用",
  );
}

export async function createApplication(
  projectId: string,
  name: string,
  slug: string,
  idempotencyKey: string,
): Promise<Application> {
  return requireData(
    await client.POST("/api/v1/projects/{projectId}/applications", {
      params: {
        path: { projectId },
        header: { "Idempotency-Key": idempotencyKey },
      },
      body: { name, slug },
    }),
    "创建应用",
  );
}

export async function listDeploymentTargets(
  applicationId: string,
): Promise<DeploymentTarget[]> {
  const result = await client.GET(
    "/api/v1/applications/{applicationId}/deployment-targets",
    {
      params: { path: { applicationId }, query: { limit: 20 } },
    },
  );
  return requireData(result, "查询部署目标").items;
}
