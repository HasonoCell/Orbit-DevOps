import createClient from "openapi-fetch";
import type { components, paths } from "./schema";

const client = createClient<paths>({
  baseUrl: import.meta.env.VITE_ORBITOPS_API_URL ?? "",
});

export type DeliveryInput = {
  projectName: string;
  projectSlug: string;
  applicationName: string;
  applicationSlug: string;
  replicas: number;
  containerPort: number;
  imageReference: string;
};

export type Project = components["schemas"]["Project"];
export type Application = components["schemas"]["Application"];
export type DeploymentTarget = components["schemas"]["DeploymentTarget"];
export type ReleaseAcceptance = components["schemas"]["ReleaseAcceptance"];
export type Operation = components["schemas"]["Operation"];
export type RuntimeSnapshot = components["schemas"]["RuntimeSnapshot"];

export type DeliveryAcceptance = {
  project: Project;
  application: Application;
  target: DeploymentTarget;
  release: ReleaseAcceptance["release"];
  operation: ReleaseAcceptance["operation"];
};

export async function createDelivery(
  input: DeliveryInput,
  idempotencyRoot: string,
): Promise<DeliveryAcceptance> {
  const projectResult = await client.POST("/api/v1/projects", {
    params: { header: { "Idempotency-Key": `${idempotencyRoot}-project` } },
    body: { name: input.projectName, slug: input.projectSlug },
  });
  const project = requireData(projectResult.data, projectResult.error, "创建项目");

  const applicationResult = await client.POST("/api/v1/projects/{projectId}/applications", {
    params: {
      path: { projectId: project.id },
      header: { "Idempotency-Key": `${idempotencyRoot}-application` },
    },
    body: { name: input.applicationName, slug: input.applicationSlug },
  });
  const application = requireData(
    applicationResult.data,
    applicationResult.error,
    "创建应用",
  );

  const targetResult = await client.POST(
    "/api/v1/applications/{applicationId}/deployment-targets",
    {
      params: {
        path: { applicationId: application.id },
        header: { "Idempotency-Key": `${idempotencyRoot}-target` },
      },
      body: {
        stage: "development",
        replicas: input.replicas,
        containerPort: input.containerPort,
      },
    },
  );
  const target = requireData(targetResult.data, targetResult.error, "创建部署目标");

  const releaseResult = await client.POST(
    "/api/v1/deployment-targets/{deploymentTargetId}/releases",
    {
      params: {
        path: { deploymentTargetId: target.id },
        header: { "Idempotency-Key": `${idempotencyRoot}-release` },
      },
      body: { imageReference: input.imageReference },
    },
  );
  const acceptance = requireData(releaseResult.data, releaseResult.error, "创建发布");

  return {
    project,
    application,
    target,
    release: acceptance.release,
    operation: acceptance.operation,
  };
}

export async function getOperation(operationId: string): Promise<Operation> {
  const result = await client.GET("/api/v1/operations/{operationId}", {
    params: { path: { operationId } },
  });
  return requireData(result.data, result.error, "查询操作状态");
}

export async function getRuntimeSnapshot(targetId: string): Promise<RuntimeSnapshot> {
  const result = await client.GET(
    "/api/v1/deployment-targets/{deploymentTargetId}/runtime-snapshot",
    { params: { path: { deploymentTargetId: targetId } } },
  );
  return requireData(result.data, result.error, "查询运行状态");
}

function requireData<T>(data: T | undefined, error: unknown, action: string): T {
  if (data !== undefined) {
    return data;
  }
  throw new Error(`${action}失败：${errorMessage(error)}`);
}

function errorMessage(error: unknown): string {
  if (typeof error === "object" && error !== null && "message" in error) {
    const message = (error as { message?: unknown }).message;
    if (typeof message === "string" && message !== "") {
      return message;
    }
  }
  return "控制平面没有返回可用结果";
}
