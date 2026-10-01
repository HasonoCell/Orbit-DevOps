import { chooseOption } from "./helpers/select";
import { expect, test } from "@playwright/test";
import {
  application,
  build,
  diagnostic,
  principal,
  project,
  release,
  target,
} from "./fixtures/overview";

const developmentTarget = {
  ...target,
  id: "t-dev",
  stage: "development" as const,
};
const productionTarget = {
  ...target,
  id: "t-prod",
  stage: "production" as const,
};
const developmentRelease = {
  ...release.release,
  deploymentTargetId: "t-dev",
  targetSnapshot: {
    ...release.release.targetSnapshot,
    stage: "development" as const,
  },
};
const developmentOperation = {
  ...diagnostic().releaseOperation,
  deploymentTargetId: "t-dev",
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
  await page.route("**/api/v1/applications/a-1/builds?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/applications/a-1/delivery-pipelines?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/deployment-targets/*/releases?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/releases/r-2", (route) =>
    route.fulfill({
      json: {
        release: {
          ...release.release,
          id: "r-2",
          deploymentTargetId: "t-prod",
        },
        releaseOperation: {
          ...diagnostic().releaseOperation,
          id: "ro-2",
          releaseId: "r-2",
        },
        snapshotDifferences: [],
        auditTimeline: [],
      },
    }),
  );
  await page.route("**/api/v1/deployment-targets/t-prod", (route) =>
    route.fulfill({ json: productionTarget }),
  );
  await page.route("**/api/v1/release-operations/ro-2", (route) =>
    route.fulfill({
      json: { ...diagnostic().releaseOperation, id: "ro-2", releaseId: "r-2" },
    }),
  );
  await page.route("**/api/v1/releases/r-2/diagnostics", (route) =>
    route.fulfill({ json: diagnostic() }),
  );
});

test("从成功 Build 预选当前应用产物，确认生产目标后创建新发布", async ({
  page,
}) => {
  let accepted = false;
  await page.route("**/api/v1/builds/b-1", (route) =>
    route.fulfill({ json: build }),
  );
  await page.route("**/api/v1/build-operations/bo-1", (route) =>
    route.fulfill({ json: build.buildOperation }),
  );
  await page.route("**/api/v1/deployment-targets/t-prod/releases", (route) => {
    accepted = true;
    expect(route.request().postDataJSON()).toEqual({
      imageReference: build.imageArtifact?.imageReference,
      imageArtifactId: build.imageArtifact?.id,
    });
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    return route.fulfill({
      status: 201,
      json: {
        release: {
          ...release.release,
          id: "r-2",
          deploymentTargetId: "t-prod",
        },
        releaseOperation: { ...diagnostic().releaseOperation, id: "ro-2" },
      },
    });
  });
  await page.goto("/projects/p-1/applications/a-1/builds/b-1");
  await page.getByRole("link", { name: "使用此产物发布" }).click();
  await expect(page.getByRole("dialog", { name: "新建发布" })).toBeVisible();
  await expect(page.getByText(`来源：构建 ${build.build.id}`)).toBeVisible();
  await expect(
    page.getByText(build.imageArtifact!.imageReference),
  ).toBeVisible();
  // 先关弹窗再选择页面目标；不绕过模态边界操作被遮挡的后台控件。
  await page.getByRole("dialog").getByRole("button", { name: "Close" }).click();
  await chooseOption(page.getByLabel("部署目标", { exact: true }), "t-prod");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  await expect(page.getByText(`来源：构建 ${build.build.id}`)).toBeVisible();
  await expect(page.getByText("生产目标", { exact: true })).toBeVisible();
  expect(accepted).toBe(false);
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到生产环境" }).click();
  await expect(page).toHaveURL(/\/releases\/r-2$/);
  expect(accepted).toBe(true);
});

test("开发 Release 晋级生产仅预填来源镜像，生产接纳仍需明确确认", async ({
  page,
}) => {
  let payload: unknown;
  await page.route("**/api/v1/deployment-targets/t-dev", (route) =>
    route.fulfill({ json: developmentTarget }),
  );
  await page.route("**/api/v1/release-operations/ro-1", (route) =>
    route.fulfill({ json: developmentOperation }),
  );
  await page.route("**/api/v1/releases/r-1", (route) =>
    route.fulfill({
      json: {
        release: developmentRelease,
        releaseOperation: developmentOperation,
        snapshotDifferences: [],
        auditTimeline: [],
      },
    }),
  );
  await page.route("**/api/v1/releases/r-1/diagnostics", (route) =>
    route.fulfill({ json: diagnostic() }),
  );
  await page.route("**/api/v1/deployment-targets/t-prod/releases", (route) => {
    payload = route.request().postDataJSON();
    return route.fulfill({
      status: 201,
      json: {
        release: {
          ...release.release,
          id: "r-2",
          deploymentTargetId: "t-prod",
        },
        releaseOperation: { ...diagnostic().releaseOperation, id: "ro-2" },
      },
    });
  });
  await page.goto("/projects/p-1/applications/a-1/releases/r-1");
  await page.getByRole("link", { name: "晋级到生产" }).click();
  await expect(page.getByRole("dialog", { name: "新建发布" })).toBeVisible();
  await expect(
    page.getByText("将开发发布的镜像晋级生产", { exact: false }),
  ).toBeVisible();
  await expect(page.getByLabel("镜像引用")).toHaveValue(
    developmentRelease.imageReference,
  );
  await expect(
    page.getByRole("button", { name: "确认发布到生产环境" }),
  ).toBeDisabled();
  expect(payload).toBeUndefined();
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到生产环境" }).click();
  await expect(page).toHaveURL(/\/releases\/r-2$/);
  expect(payload).toEqual({
    imageReference: developmentRelease.imageReference,
  });
});

test("失败或跨应用 Build 不作为可选 Artifact", async ({ page }) => {
  await page.route("**/api/v1/builds/b-1", (route) =>
    route.fulfill({
      json: {
        ...build,
        build: { ...build.build, applicationId: "other" },
      },
    }),
  );
  await page.goto(
    "/projects/p-1/applications/a-1?view=delivery&buildSource=b-1&createRelease=1",
  );
  await expect(
    page.getByText("此构建没有属于当前应用的成功产物"),
  ).toBeVisible();
  await page.getByRole("checkbox").check();
  await expect(
    page.getByRole("button", { name: "确认发布到开发环境" }),
  ).toBeDisabled();
});
