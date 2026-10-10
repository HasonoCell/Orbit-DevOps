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

function anotherBuild(id: string, digestCharacter = "b"): typeof build {
  const digest = "sha256:" + digestCharacter.repeat(64);
  return {
    ...build,
    build: { ...build.build, id },
    buildOperation: {
      ...build.buildOperation,
      id: `operation-${id}`,
      buildId: id,
    },
    imageArtifact: {
      ...build.imageArtifact!,
      id: `artifact-${id}`,
      buildId: id,
      digest,
      imageReference: `${build.imageArtifact!.repository}@${digest}`,
    },
  };
}

test("发布产物按页单选，跨页保留已选快照并可返回已访问页", async ({ page }) => {
  const requestedCursors: (string | null)[] = [];
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    const params = new URL(route.request().url()).searchParams;
    expect(params.get("limit")).toBe("5");
    const cursor = params.get("cursor");
    requestedCursors.push(cursor);
    return route.fulfill({
      json: cursor
        ? { items: [anotherBuild("b-6", "f"), anotherBuild("b-7", "7")] }
        : {
            items: [
              build,
              anotherBuild("b-2", "b"),
              anotherBuild("b-3", "c"),
              anotherBuild("b-4", "d"),
              anotherBuild("b-5", "e"),
            ],
            nextCursor: "page-2",
          },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  await expect(picker).toBeVisible();
  await expect(picker.getByRole("radio")).toHaveCount(5);
  await expect(dialog.getByRole("combobox", { name: "选择产物" })).toHaveCount(
    0,
  );
  await expect(
    dialog.getByRole("button", { name: "加载更多", exact: true }),
  ).toHaveCount(0);
  await picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }).check();
  await dialog.getByRole("checkbox").check();
  await dialog.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(picker.getByRole("radio")).toHaveCount(2);
  await expect(
    picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }),
  ).toHaveCount(0);
  await expect(
    picker.getByRole("radio", { name: /^b-6 \/ sha256:/ }),
  ).toBeVisible();
  const selected = dialog.getByRole("region", {
    name: "已选产物",
    exact: true,
  });
  await expect(selected).toContainText(build.imageArtifact!.imageReference);
  await expect(selected.getByRole("radio")).toHaveCount(0);
  await expect(dialog.getByRole("checkbox")).toBeChecked();
  await dialog.getByRole("button", { name: "上一页", exact: true }).click();
  await expect(picker.getByRole("radio")).toHaveCount(5);
  await dialog.getByRole("button", { name: "第 2 页", exact: true }).click();
  await expect(picker.getByRole("radio")).toHaveCount(2);
  await dialog.getByRole("button", { name: "第 1 页", exact: true }).click();
  await expect(picker.getByRole("radio")).toHaveCount(5);
  await expect(
    picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }),
  ).toBeChecked();
  await dialog.getByRole("button", { name: "第 2 页", exact: true }).click();
  await expect(picker.getByRole("radio")).toHaveCount(2);
  expect(requestedCursors.filter((cursor) => cursor === "page-2")).toHaveLength(
    1,
  );
});

test("四个已访问产物页使用紧凑页码，375px 下可逐页返回与跳转", async ({
  page,
}) => {
  await page.setViewportSize({ width: 375, height: 812 });
  const requestedCursors: (string | null)[] = [];
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    requestedCursors.push(cursor);
    return route.fulfill({
      json:
        cursor === "page-4"
          ? { items: [anotherBuild("b-4", "d")] }
          : cursor === "page-3"
            ? { items: [anotherBuild("b-3", "c")], nextCursor: "page-4" }
            : cursor === "page-2"
              ? { items: [anotherBuild("b-2", "b")], nextCursor: "page-3" }
              : { items: [build], nextCursor: "page-2" },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  const pagination = dialog.getByRole("navigation", {
    name: "产物分页",
    exact: true,
  });
  await picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }).check();
  for (const { name, visiblePageCount } of [
    { name: /^b-2 \/ sha256:/, visiblePageCount: 2 },
    { name: /^b-3 \/ sha256:/, visiblePageCount: 3 },
    { name: /^b-4 \/ sha256:/, visiblePageCount: 3 },
  ]) {
    await pagination
      .getByRole("button", { name: "下一页", exact: true })
      .click();
    await expect(picker.getByRole("radio", { name })).toBeVisible();
    await expect(
      pagination.getByRole("button", { name: /^第 \d+ 页$/ }),
    ).toHaveCount(visiblePageCount);
  }
  await expect(
    pagination.getByRole("button", { name: "第 1 页", exact: true }),
  ).toHaveCount(0);
  await expect(
    pagination.getByRole("button", { name: "第 4 页", exact: true }),
  ).toHaveAttribute("aria-current", "page");
  const bounds = await pagination.boundingBox();
  expect(bounds).not.toBeNull();
  expect(bounds!.x).toBeGreaterThanOrEqual(0);
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(375);
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(375);
  await pagination.getByRole("button", { name: "上一页", exact: true }).click();
  await expect(
    picker.getByRole("radio", { name: /^b-3 \/ sha256:/ }),
  ).toBeVisible();
  await pagination
    .getByRole("button", { name: "第 2 页", exact: true })
    .click();
  await expect(
    picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }),
  ).toBeVisible();
  await pagination
    .getByRole("button", { name: "第 1 页", exact: true })
    .click();
  await expect(
    picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }),
  ).toBeChecked();
  expect(requestedCursors.filter((cursor) => cursor === "page-2")).toHaveLength(
    1,
  );
  expect(requestedCursors.filter((cursor) => cursor === "page-3")).toHaveLength(
    1,
  );
  expect(requestedCursors.filter((cursor) => cursor === "page-4")).toHaveLength(
    1,
  );
});

test("末页游标回指已访问页时停止翻页，避免循环读取历史", async ({ page }) => {
  const requestedCursors: (string | null)[] = [];
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    requestedCursors.push(cursor);
    return route.fulfill({
      json:
        cursor === "page-3"
          ? { items: [anotherBuild("b-3", "c")], nextCursor: "page-2" }
          : cursor === "page-2"
            ? { items: [anotherBuild("b-2", "b")], nextCursor: "page-3" }
            : { items: [build], nextCursor: "page-2" },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  const next = dialog.getByRole("button", { name: "下一页", exact: true });
  await next.click();
  await expect(
    picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }),
  ).toBeVisible();
  await next.click();
  await expect(
    picker.getByRole("radio", { name: /^b-3 \/ sha256:/ }),
  ).toBeVisible();
  await expect(next).toBeDisabled();
  await next.evaluate((button: HTMLButtonElement) => button.click());
  await expect(
    dialog.getByRole("button", { name: "第 4 页", exact: true }),
  ).toHaveCount(0);
  expect(requestedCursors.filter((cursor) => cursor === "page-2")).toHaveLength(
    1,
  );
  expect(requestedCursors.filter((cursor) => cursor === "page-3")).toHaveLength(
    1,
  );
  await dialog.getByRole("button", { name: "上一页", exact: true }).click();
  await expect(
    picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }),
  ).toBeVisible();
  await expect(next).toBeEnabled();
  await next.click();
  await expect(
    picker.getByRole("radio", { name: /^b-3 \/ sha256:/ }),
  ).toBeVisible();
  await expect(next).toBeDisabled();
});

test("选择另一产物撤销发布确认，单纯翻页保留确认", async ({ page }) => {
  const second = anotherBuild("b-2", "b");
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    return route.fulfill({
      json: cursor
        ? { items: [second] }
        : { items: [build], nextCursor: "page-2" },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  const confirmation = dialog.getByRole("checkbox");
  const submit = dialog.getByRole("button", {
    name: "确认发布到开发环境",
    exact: true,
  });
  await picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }).check();
  await confirmation.check();
  await dialog.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(confirmation).toBeChecked();
  await expect(submit).toBeEnabled();
  await picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }).check();
  await expect(confirmation).not.toBeChecked();
  await expect(submit).toBeDisabled();
  await expect(dialog.getByRole("region", { name: "已选产物" })).toContainText(
    second.imageArtifact!.imageReference,
  );
  await confirmation.check();
  await dialog.getByRole("button", { name: "上一页", exact: true }).click();
  await expect(confirmation).toBeChecked();
  await picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }).check();
  await expect(confirmation).not.toBeChecked();
  await expect(submit).toBeDisabled();
});

test("概览轮询发现构建完成后，打开的产物选择器同步更新", async ({ page }) => {
  await page.clock.install();
  let succeeded = false;
  let listRequests = 0;
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    listRequests += 1;
    return route.fulfill({
      json: {
        items: succeeded
          ? [build]
          : [
              {
                ...build,
                buildOperation: { ...build.buildOperation, status: "running" },
                imageArtifact: undefined,
              },
            ],
      },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const picker = page
    .getByRole("dialog")
    .getByRole("radiogroup", { name: "选择产物", exact: true });
  await expect(picker.getByRole("radio")).toHaveCount(0);
  succeeded = true;
  await page.clock.fastForward(30_001);
  await expect.poll(() => listRequests).toBeGreaterThan(1);
  const firstArtifact = picker.getByRole("radio", { name: /^b-1 \/ sha256:/ });
  await expect(firstArtifact).toBeEnabled();
  await firstArtifact.check();
  await expect(firstArtifact).toBeChecked();
});

test("首批刷新改变游标后从新页链继续，保留历史已选产物", async ({ page }) => {
  await page.clock.install();
  const second = anotherBuild("b-2");
  const newest = anotherBuild("b-3", "c");
  let updated = false;
  const requested: (string | null)[] = [];
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    requested.push(cursor);
    return route.fulfill({
      json:
        cursor === "new-page-2"
          ? { items: [anotherBuild("b-4", "d")] }
          : cursor === "old-page-2"
            ? { items: [second] }
            : updated
              ? { items: [newest, build], nextCursor: "new-page-2" }
              : {
                  items: [
                    build,
                    {
                      ...newest,
                      buildOperation: {
                        ...newest.buildOperation,
                        status: "running",
                      },
                      imageArtifact: undefined,
                    },
                  ],
                  nextCursor: "old-page-2",
                },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  await dialog.getByRole("button", { name: "下一页", exact: true }).click();
  await picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }).check();
  await dialog.getByRole("checkbox").check();
  updated = true;
  await page.clock.fastForward(30_001);
  await expect(
    picker.getByRole("radio", { name: /^b-3 \/ sha256:/ }),
  ).toBeVisible();
  await expect(
    picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }),
  ).toHaveCount(0);
  await expect(
    dialog.getByRole("button", { name: "第 2 页", exact: true }),
  ).toHaveCount(0);
  expect(requested).not.toContain("new-page-2");
  await expect(dialog.getByRole("region", { name: "已选产物" })).toContainText(
    second.imageArtifact!.imageReference,
  );
  await expect(dialog.getByRole("checkbox")).toBeChecked();
  await dialog.getByRole("button", { name: "下一页", exact: true }).click();
  await picker.getByRole("radio", { name: /^b-4 \/ sha256:/ }).check();
  expect(requested.filter((cursor) => cursor === "old-page-2")).toHaveLength(1);
  expect(requested.filter((cursor) => cursor === "new-page-2")).toHaveLength(1);
});

test("切换产物页保留选择与发布确认，且不会提交发布表单", async ({ page }) => {
  const second = anotherBuild("b-2");
  let releaseRequests = 0;
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    return route.fulfill({
      json: cursor
        ? { items: [second] }
        : { items: [build], nextCursor: "page-2" },
    });
  });
  await page.route("**/api/v1/deployment-targets/t-dev/releases", (route) => {
    releaseRequests += 1;
    return route.fulfill({
      status: 500,
      json: { message: "unexpected submit" },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  await picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }).check();
  await dialog.getByRole("checkbox").check();
  await dialog.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(dialog.getByRole("region", { name: "已选产物" })).toContainText(
    build.imageArtifact!.imageReference,
  );
  await expect(dialog.getByRole("checkbox")).toBeChecked();
  await expect(
    dialog.getByRole("button", { name: "确认发布到开发环境" }),
  ).toBeEnabled();
  await expect(dialog.getByRole("alert")).toHaveCount(0);
  await expect(
    picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }),
  ).toBeVisible();
  await expect(
    picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }),
  ).toHaveCount(0);
  expect(releaseRequests).toBe(0);
});

test("产物按需翻页，跳过失败与跨应用构建并在当前页去重", async ({ page }) => {
  const third = anotherBuild("b-3", "c");
  const failed = {
    ...anotherBuild("failed"),
    buildOperation: { ...build.buildOperation, status: "failed" },
  };
  const foreign = {
    ...anotherBuild("foreign"),
    build: { ...build.build, id: "foreign", applicationId: "other" },
  };
  const requestedCursors: (string | null)[] = [];
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    requestedCursors.push(cursor);
    return route.fulfill({
      json:
        cursor === "page-3"
          ? { items: [build, third, third] }
          : cursor === "page-2"
            ? { items: [failed, foreign], nextCursor: "page-3" }
            : { items: [build], nextCursor: "page-2" },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  await picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }).check();
  expect(requestedCursors).not.toContain("page-2");
  await dialog.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(picker.getByRole("radio")).toHaveCount(0);
  await expect(dialog.getByRole("region", { name: "已选产物" })).toContainText(
    build.imageArtifact!.imageReference,
  );
  expect(requestedCursors).not.toContain("page-3");
  await dialog.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(
    dialog.getByRole("button", { name: "下一页", exact: true }),
  ).toBeDisabled();
  await expect(
    picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }),
  ).toHaveCount(1);
  await expect(
    picker.getByRole("radio", { name: /^b-3 \/ sha256:/ }),
  ).toHaveCount(1);
  await expect(
    picker.getByRole("radio", { name: /failed|foreign/ }),
  ).toHaveCount(0);
});

test("翻页失败可重试，不丢失已选产物和发布确认", async ({ page }) => {
  let failNext = true;
  await page.route("**/api/v1/applications/a-1/builds?*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    if (cursor && failNext) return route.fulfill({ status: 503, json: {} });
    return route.fulfill({
      json: cursor
        ? { items: [anotherBuild("b-2")] }
        : { items: [build], nextCursor: "page-2" },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  await picker.getByRole("radio", { name: /^b-1 \/ sha256:/ }).check();
  await dialog.getByRole("checkbox").check();
  await dialog.getByRole("button", { name: "下一页", exact: true }).click();
  await expect(dialog.getByRole("alert")).toBeVisible();
  await expect(dialog.getByRole("region", { name: "已选产物" })).toContainText(
    build.imageArtifact!.imageReference,
  );
  await expect(dialog.getByRole("checkbox")).toBeChecked();
  failNext = false;
  await dialog.getByRole("button", { name: "重试", exact: true }).click();
  await expect(dialog.getByRole("alert")).toHaveCount(0);
  await picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }).check();
  await expect(
    picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }),
  ).toBeChecked();
  await expect(dialog.getByRole("region", { name: "已选产物" })).toContainText(
    anotherBuild("b-2").imageArtifact!.imageReference,
  );
});

test("首批无产物仍可翻页，加载期间不发起重复请求并提交实际选择", async ({
  page,
}) => {
  let finishNextPage!: () => void;
  const nextPageReady = new Promise<void>((resolve) => {
    finishNextPage = resolve;
  });
  let nextPageRequests = 0;
  await page.route("**/api/v1/applications/a-1/builds?*", async (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    if (cursor) {
      nextPageRequests += 1;
      await nextPageReady;
      return route.fulfill({ json: { items: [anotherBuild("b-2")] } });
    }
    return route.fulfill({ json: { items: [], nextCursor: "page-2" } });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByRole("radiogroup", {
    name: "选择产物",
    exact: true,
  });
  await expect(picker.getByRole("radio")).toHaveCount(0);
  await expect(dialog.getByText("暂无产物", { exact: true })).toBeVisible();
  try {
    await dialog.getByRole("button", { name: "下一页", exact: true }).click();
    const loading = dialog.getByRole("button", {
      name: "下一页",
      exact: true,
    });
    await expect(loading).toBeDisabled();
    await loading.evaluate((button: HTMLButtonElement) => button.click());
    await expect.poll(() => nextPageRequests).toBe(1);
    await expect(picker).toBeVisible();
  } finally {
    finishNextPage();
  }
  await picker.getByRole("radio", { name: /^b-2 \/ sha256:/ }).check();
  await dialog.getByRole("checkbox").check();
  const requestPromise = page.waitForRequest(
    (request) =>
      request.method() === "POST" &&
      request.url().endsWith("/deployment-targets/t-dev/releases"),
  );
  await page.route("**/api/v1/deployment-targets/t-dev/releases", (route) =>
    route.fulfill({ status: 503, json: {} }),
  );
  await dialog.getByRole("button", { name: "确认发布到开发环境" }).click();
  const request = await requestPromise;
  expect(request.postDataJSON()).toEqual({
    imageArtifactId: "artifact-b-2",
    imageReference: anotherBuild("b-2").imageArtifact!.imageReference,
  });
});

for (const viewport of [
  { width: 1280, height: 900 },
  { width: 390, height: 844 },
  { width: 375, height: 812 },
  { width: 844, height: 390 },
]) {
  test(`产物选择在 ${viewport.width}px 支持键盘且不溢出视口`, async ({
    page,
  }, testInfo) => {
    await page.setViewportSize(viewport);
    await page.route("**/api/v1/applications/a-1/builds?*", (route) =>
      route.fulfill({
        json: {
          items: [
            build,
            anotherBuild("b-2"),
            anotherBuild("b-3", "c"),
            anotherBuild("b-4", "d"),
            anotherBuild("b-5", "e"),
          ],
        },
      }),
    );
    await page.goto("/projects/p-1/applications/a-1?view=delivery");
    await page.getByRole("button", { name: "新建发布", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "新建发布" });
    const cancel = dialog.getByRole("button", { name: "取消", exact: true });
    const submit = dialog.getByRole("button", {
      name: "确认发布到开发环境",
      exact: true,
    });
    await expect(cancel).toBeInViewport({ ratio: 1 });
    await expect(submit).toBeInViewport({ ratio: 1 });
    const picker = dialog.getByRole("radiogroup", {
      name: "选择产物",
      exact: true,
    });
    const firstArtifact = picker.getByRole("radio", {
      name: /^b-1 \/ sha256:/,
    });
    const secondArtifact = picker.getByRole("radio", {
      name: /^b-2 \/ sha256:/,
    });
    await firstArtifact.focus();
    await page.keyboard.press("Space");
    await expect(firstArtifact).toBeChecked();
    // 方向键聚焦是异步的；保持按键到焦点迁移结束，再发送 keyup。
    await page.keyboard.down("ArrowDown");
    try {
      await expect(secondArtifact).toBeFocused();
      await expect(secondArtifact).toBeChecked();
    } finally {
      await page.keyboard.up("ArrowDown");
    }
    const bounds = await picker.boundingBox();
    expect(bounds).not.toBeNull();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(viewport.width);
    await page.screenshot({
      path: testInfo.outputPath(`artifact-picker-${viewport.width}.png`),
    });
    await page.keyboard.down("ArrowUp");
    try {
      await expect(firstArtifact).toBeFocused();
      await expect(firstArtifact).toBeChecked();
    } finally {
      await page.keyboard.up("ArrowUp");
    }
    await expect(
      dialog.getByRole("region", { name: "已选产物" }),
    ).toContainText(build.imageArtifact!.imageReference);
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(viewport.width);
    const confirmation = dialog.getByRole("checkbox");
    await confirmation.focus();
    await expect(confirmation).toBeInViewport({ ratio: 1 });
    // check 的真实命中测试同时验证确认框没有被固定页脚遮挡。
    await confirmation.check();
    await page.keyboard.press("Tab");
    await expect(cancel).toBeFocused();
    await expect(cancel).toBeInViewport({ ratio: 1 });
    await page.keyboard.press("Tab");
    await expect(submit).toBeFocused();
    await expect(submit).toBeInViewport({ ratio: 1 });
  });
}
