import { chooseOption } from "./helpers/select";
import { expect, test } from "@playwright/test";
import {
  application,
  principal,
  project,
  target,
  timestamp,
} from "./fixtures/overview";

const developmentTarget = { ...target, id: "t-dev", stage: "development" };
const productionTarget = { ...target, id: "t-prod", stage: "production" };
const pipeline = {
  pipeline: {
    id: "pl-1",
    projectId: "p-1",
    applicationId: "a-1",
    name: "Payment CI",
    currentRevision: 1,
    enabled: false,
    activationGeneration: 0,
    createdBy: "u-1",
    createdAt: timestamp,
    updatedAt: timestamp,
  },
  revision: {
    revision: 1,
    provider: "github",
    endpointKey: "demo",
    repositoryId: 1,
    repositoryOwnerId: 2,
    repositoryFullName: "HasonoCell/Yuuki",
    repositoryUrl: "https://github.com/HasonoCell/Yuuki.git",
    gitRef: "refs/heads/main",
    dockerfilePath: "Dockerfile",
    contextPath: ".",
    platform: "linux/amd64",
    mode: "build_only",
    createdBy: "u-1",
    createdAt: timestamp,
  },
};

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: principal }),
  );
  await page.route("**/api/v1/projects/p-1", (route) =>
    route.fulfill({ json: project }),
  );
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "developer",
        allowed: ["read", "develop"],
      },
    }),
  );
  await page.route("**/api/v1/applications/a-1", (route) =>
    route.fulfill({ json: application }),
  );
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) =>
    route.fulfill({ json: { items: [developmentTarget, productionTarget] } }),
  );
  await page.route("**/api/v1/delivery-pipelines/pl-1/runs?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
});

test("默认仅构建，创建后显示 Orbit 状态和 Webhook 地址", async ({ page }) => {
  let created = false;
  await page.route("**/api/v1/applications/a-1/delivery-pipelines", (route) => {
    expect(route.request().postDataJSON()).toEqual({
      name: "Payment CI",
      endpointKey: "demo",
      repositoryUrl: "https://github.com/HasonoCell/Yuuki.git",
      branch: "main",
      dockerfilePath: "Dockerfile",
      contextPath: ".",
      mode: "build_only",
    });
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    created = true;
    return route.fulfill({ status: 201, json: pipeline });
  });
  await page.route("**/api/v1/delivery-pipelines/pl-1", (route) =>
    route.fulfill({ json: pipeline }),
  );
  await page.goto("/projects/p-1/applications/a-1/pipelines/new");
  await page.getByLabel("名称").fill("Payment CI");
  await page.getByLabel("Endpoint Key").fill("demo");
  await page
    .getByLabel("GitHub Clone URL")
    .fill("https://github.com/HasonoCell/Yuuki.git");
  await page.getByRole("button", { name: "创建 Pipeline" }).click();
  await expect(page).toHaveURL(/\/pipelines\/pl-1$/);
  await expect(page.getByText("Orbit 已停用")).toBeVisible();
  await expect(page.getByText(/api\/v1\/webhooks\/github\/demo/)).toBeVisible();
  await expect(page.getByText("当前 Revision", { exact: true })).toBeVisible();
  await expect(page.getByText(/不表示 GitHub 已连接/)).toHaveCount(0);
  expect(created).toBe(true);
});

test("自动部署只能使用开发 Target；修订冲突后读取新 Revision", async ({
  page,
}) => {
  let current = pipeline;
  let updates = 0;
  await page.route("**/api/v1/delivery-pipelines/pl-1", (route) => {
    if (route.request().method() === "GET")
      return route.fulfill({ json: current });
    updates++;
    const body = route.request().postDataJSON();
    expect(body).toEqual({
      expectedRevision: 1,
      endpointKey: "demo",
      repositoryUrl: "https://github.com/HasonoCell/Yuuki.git",
      branch: "main",
      dockerfilePath: "Dockerfile",
      contextPath: ".",
      mode: "auto_release",
      deploymentTargetId: "t-dev",
    });
    expect(body.name).toBeUndefined();
    current = {
      ...pipeline,
      pipeline: { ...pipeline.pipeline, currentRevision: 2 },
      revision: { ...pipeline.revision, revision: 2 },
    };
    return route.fulfill({
      status: 409,
      json: {
        code: "delivery_pipeline_conflict",
        message: "revision conflict",
      },
    });
  });
  await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1");
  await chooseOption(page.getByLabel("交付模式"), "auto_release");
  await page.getByLabel("开发 Target").click();
  await expect(
    page.getByRole("listbox").locator('[data-value="t-prod"]'),
  ).toHaveCount(0);
  await page.keyboard.press("Escape");
  await chooseOption(page.getByLabel("开发 Target"), "t-dev");
  await page.getByRole("button", { name: "保存新 Revision" }).click();
  await expect(page.getByText(/配置已变化或存在冲突/)).toBeVisible();
  await page.getByRole("button", { name: "刷新服务端配置" }).click();
  await expect(page.getByText("2", { exact: true })).toBeVisible();
  expect(updates).toBe(1);
});

test("启停需确认，结果以服务端状态为准", async ({ page }) => {
  let current = pipeline;
  await page.route("**/api/v1/delivery-pipelines/pl-1", (route) =>
    route.fulfill({ json: current }),
  );
  await page.route("**/api/v1/delivery-pipelines/pl-1/enable", (route) => {
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    current = {
      ...current,
      pipeline: { ...current.pipeline, enabled: true, activationGeneration: 1 },
    };
    return route.fulfill({ json: current });
  });
  await page.route("**/api/v1/delivery-pipelines/pl-1/disable", (route) => {
    current = {
      ...current,
      pipeline: { ...current.pipeline, enabled: false },
    };
    return route.fulfill({ json: current });
  });
  await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1");
  await page.getByRole("button", { name: "启用 Pipeline" }).click();
  await expect(page.getByText("确认启用 Payment CI？")).toBeVisible();
  await page.getByRole("button", { name: "确认", exact: true }).click();
  await expect(page.getByText("Orbit 已启用", { exact: true })).toBeVisible();
  await page.reload();
  await expect(page.getByText("Orbit 已启用", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "停用 Pipeline" }).click();
  await page.getByRole("button", { name: "确认", exact: true }).click();
  await expect(page.getByText("Orbit 已停用")).toBeVisible();
});
