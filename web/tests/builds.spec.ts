import { expect, test } from "@playwright/test";

const sha = "a".repeat(40);
const user = {
  id: "u-1",
  displayName: "Alice",
  status: "active",
  platformRole: "user",
  createdAt: "2026-01-01T00:00:00Z",
};
const project = {
  id: "p-1",
  name: "Yuuki",
  slug: "yuuki",
  createdBy: "u-1",
  createdAt: "2026-01-01T00:00:00Z",
};
const application = {
  id: "a-1",
  projectId: "p-1",
  name: "Payment Service",
  slug: "payment-service",
  createdBy: "u-1",
  createdAt: "2026-01-01T00:00:00Z",
};
const build = {
  id: "b-1",
  projectId: "p-1",
  applicationId: "a-1",
  repositoryUrl: "https://github.com/HasonoCell/Yuuki.git",
  sourceCommit: sha,
  dockerfilePath: "Dockerfile",
  contextPath: ".",
  platform: "linux/amd64",
  destinationRepository: "registry.example.com/yuuki/payment",
  createdBy: "u-1",
  createdAt: "2026-01-01T00:00:00Z",
};
const operation = {
  id: "bo-1",
  buildId: "b-1",
  createdBy: "u-1",
  idempotencyKey: "k-1",
  status: "succeeded",
  attemptCount: 1,
  automaticRetryCount: 0,
  recoveryRequired: false,
  queuedAt: "2026-01-01T00:00:00Z",
  availableAt: "2026-01-01T00:00:00Z",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:01:00Z",
  attempts: [
    {
      id: "ba-1",
      number: 1,
      workerId: "worker-a",
      status: "succeeded",
      startedAt: "2026-01-01T00:00:10Z",
      finishedAt: "2026-01-01T00:01:00Z",
    },
  ],
};
const artifact = {
  id: "ar-1",
  buildId: "b-1",
  projectId: "p-1",
  applicationId: "a-1",
  repository: "registry.example.com/yuuki/payment",
  digest: `sha256:${"f".repeat(64)}`,
  imageReference: `registry.example.com/yuuki/payment@sha256:${"f".repeat(64)}`,
  platform: "linux/amd64",
  createdBy: "u-1",
  createdAt: "2026-01-01T00:01:00Z",
};

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: { kind: "user", user, mustChangePassword: false } }),
  );
  await page.route("**/api/v1/projects/p-1", (route) =>
    route.fulfill({ json: project }),
  );
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "owner",
        allowed: ["read", "read_logs", "develop", "resolve_unknown"],
      },
    }),
  );
  await page.route("**/api/v1/applications/a-1", (route) =>
    route.fulfill({ json: application }),
  );
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/applications/a-1/delivery-pipelines?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
});

test("手动构建只接受不可变源码，接纳后进入详情并读取日志摘录", async ({
  page,
}) => {
  let created = false;
  await page.route("**/api/v1/applications/a-1/builds?*", (route) =>
    route.fulfill({
      json: {
        items: created
          ? [{ build, buildOperation: operation, imageArtifact: artifact }]
          : [],
      },
    }),
  );
  await page.route("**/api/v1/applications/a-1/builds", (route) => {
    expect(route.request().method()).toBe("POST");
    expect(route.request().postDataJSON()).toEqual({
      repositoryUrl: build.repositoryUrl,
      sourceCommit: sha,
      dockerfilePath: "Dockerfile",
      contextPath: ".",
    });
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    created = true;
    return route.fulfill({
      status: 201,
      json: {
        build,
        buildOperation: { ...operation, status: "pending", attempts: [] },
      },
    });
  });
  await page.route("**/api/v1/builds/b-1", (route) =>
    route.fulfill({
      json: { build, buildOperation: operation, imageArtifact: artifact },
    }),
  );
  await page.route("**/api/v1/build-operations/bo-1", (route) =>
    route.fulfill({ json: operation }),
  );
  await page.route("**/api/v1/build-attempts/ba-1/log", (route) =>
    route.fulfill({
      json: {
        buildAttemptId: "ba-1",
        excerpt: "BuildKit: exported image",
        truncated: true,
      },
    }),
  );

  await page.goto("/projects/p-1/applications/a-1");
  await page.getByRole("button", { name: "手动构建" }).click();
  await page
    .getByLabel("Git Clone URL")
    .fill("https://user:secret@github.com/HasonoCell/Yuuki.git");
  await page.getByLabel("Commit SHA").fill("main");
  await page.getByRole("button", { name: "创建构建" }).click();
  await expect(page.getByText("仅接受不含凭据的 HTTPS 地址")).toBeVisible();
  await expect(
    page.getByText("请输入完整的 40 或 64 位 Commit SHA"),
  ).toBeVisible();
  await page.getByLabel("Git Clone URL").fill(build.repositoryUrl);
  await page.getByLabel("Commit SHA").fill(sha);
  await page.getByRole("button", { name: "创建构建" }).click();
  await expect(page).toHaveURL(/\/builds\/b-1$/);
  await expect(page.getByText(artifact.digest, { exact: true })).toBeVisible();
  await expect(page.getByText(artifact.imageReference)).toBeVisible();
  await page.getByRole("button", { name: "读取日志摘录" }).click();
  await expect(page.getByText("BuildKit: exported image")).toBeVisible();
  await expect(page.getByText("已截断")).toBeVisible();
});

test("失败构建保留错误与 Attempt，不显示镜像产物", async ({ page }) => {
  const failed = {
    ...operation,
    status: "failed",
    errorCode: "build_job_failed",
    errorSummary: "BuildKit 构建失败",
    attempts: [
      {
        ...operation.attempts[0],
        status: "failed",
        errorSummary: "Dockerfile 步骤失败",
      },
    ],
  };
  await page.route("**/api/v1/builds/b-1", (route) =>
    route.fulfill({ json: { build, buildOperation: failed } }),
  );
  await page.route("**/api/v1/build-operations/bo-1", (route) =>
    route.fulfill({ json: failed }),
  );
  await page.goto("/projects/p-1/applications/a-1/builds/b-1");
  await expect(page.getByText("BuildKit 构建失败")).toBeVisible();
  await expect(page.getByText("Dockerfile 步骤失败")).toBeVisible();
  await expect(page.getByText("暂无产物")).toBeVisible();
  await expect(page.getByText(artifact.digest)).toHaveCount(0);
});
