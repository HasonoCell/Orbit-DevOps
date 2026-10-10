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
    .getByLabel("选择产物", { exact: true });
  await expect(picker).toBeDisabled();
  succeeded = true;
  await page.clock.fastForward(30_001);
  await expect.poll(() => listRequests).toBeGreaterThan(1);
  await expect(picker).toBeEnabled();
  await chooseOption(picker, "i-1");
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
  const picker = dialog.getByLabel("选择产物", { exact: true });
  await dialog.getByRole("button", { name: "加载更多", exact: true }).click();
  await chooseOption(picker, "artifact-b-2");
  await dialog.getByRole("checkbox").check();
  updated = true;
  await page.clock.fastForward(30_001);
  await picker.click();
  await expect(
    page.getByRole("option", { name: /^b-3 \/ sha256:/ }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  expect(requested).not.toContain("new-page-2");
  await expect(picker).toHaveAttribute("data-value", "artifact-b-2");
  await expect(dialog.getByRole("checkbox")).toBeChecked();
  await dialog.getByRole("button", { name: "加载更多", exact: true }).click();
  await chooseOption(picker, "artifact-b-4");
  expect(requested.filter((cursor) => cursor === "old-page-2")).toHaveLength(1);
  expect(requested.filter((cursor) => cursor === "new-page-2")).toHaveLength(1);
});

test("加载下一批产物保留选择与发布确认，且不会提交发布表单", async ({
  page,
}) => {
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
  const picker = dialog.getByLabel("选择产物", { exact: true });
  await expect(picker).toHaveText("选择构建产物");
  await picker.click();
  await expect(page.getByRole("option", { name: "选择构建产物" })).toHaveCount(
    0,
  );
  await page.keyboard.press("Escape");
  await chooseOption(picker, "i-1");
  await dialog.getByRole("checkbox").check();
  await dialog.getByRole("button", { name: /^(下一页|加载更多)$/ }).click();
  await expect(picker).toHaveAttribute("data-value", "i-1");
  await expect(dialog.getByRole("checkbox")).toBeChecked();
  await expect(
    dialog.getByRole("button", { name: "确认发布到开发环境" }),
  ).toBeEnabled();
  await expect(dialog.getByRole("alert")).toHaveCount(0);
  await picker.click();
  await expect(
    page.getByRole("option", { name: /^b-2 \/ sha256:/ }),
  ).toBeVisible();
  await expect(
    page.getByRole("option", { name: /^b-1 \/ sha256:/ }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  expect(releaseRequests).toBe(0);
});

test("产物按需追加，跳过失败与跨应用构建并去重", async ({ page }) => {
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
          ? { items: [build, third] }
          : cursor === "page-2"
            ? { items: [failed, foreign], nextCursor: "page-3" }
            : { items: [build], nextCursor: "page-2" },
    });
  });
  await page.goto("/projects/p-1/applications/a-1?view=delivery");
  await page.getByRole("button", { name: "新建发布", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "新建发布" });
  const picker = dialog.getByLabel("选择产物", { exact: true });
  await chooseOption(picker, "i-1");
  expect(requestedCursors).not.toContain("page-2");
  await dialog.getByRole("button", { name: /^(下一页|加载更多)$/ }).click();
  await expect(picker).toHaveAttribute("data-value", "i-1");
  expect(requestedCursors).not.toContain("page-3");
  await dialog.getByRole("button", { name: /^(下一页|加载更多)$/ }).click();
  await expect(
    dialog.getByRole("button", { name: /^(下一页|加载更多)$/ }),
  ).toHaveCount(0);
  await picker.click();
  const options = page.getByRole("option");
  await expect(options.filter({ hasText: /^b-1 \/ sha256:/ })).toHaveCount(1);
  await expect(options.filter({ hasText: /^b-3 \/ sha256:/ })).toHaveCount(1);
  await expect(options.filter({ hasText: /failed|foreign/ })).toHaveCount(0);
  await page.keyboard.press("Escape");
});

test("加载更多失败可重试，不丢失已有产物和选择", async ({ page }) => {
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
  const picker = dialog.getByLabel("选择产物", { exact: true });
  await chooseOption(picker, "i-1");
  await dialog.getByRole("button", { name: /^(下一页|加载更多)$/ }).click();
  await expect(dialog.getByRole("alert")).toBeVisible();
  await expect(picker).toHaveAttribute("data-value", "i-1");
  failNext = false;
  await dialog.getByRole("button", { name: "重试", exact: true }).click();
  await expect(dialog.getByRole("alert")).toHaveCount(0);
  await chooseOption(picker, "artifact-b-2");
  await expect(picker).toHaveAttribute("data-value", "artifact-b-2");
});

test("首批无产物仍可继续加载，加载期间不发起重复请求", async ({ page }) => {
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
  const picker = dialog.getByLabel("选择产物", { exact: true });
  await expect(picker).toBeDisabled();
  await expect(picker).toHaveText("暂无产物");
  try {
    await dialog.getByRole("button", { name: "加载更多", exact: true }).click();
    const loading = dialog.getByRole("button", {
      name: "加载中…",
      exact: true,
    });
    await expect(loading).toBeDisabled();
    await loading.evaluate((button: HTMLButtonElement) => button.click());
    await expect.poll(() => nextPageRequests).toBe(1);
    await expect(picker).toBeVisible();
  } finally {
    finishNextPage();
  }
  await expect(picker).toBeEnabled();
  await chooseOption(picker, "artifact-b-2");
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
]) {
  test(`产物选择在 ${viewport.width}px 支持键盘且不溢出视口`, async ({
    page,
  }, testInfo) => {
    await page.setViewportSize(viewport);
    await page.route("**/api/v1/applications/a-1/builds?*", (route) =>
      route.fulfill({ json: { items: [build, anotherBuild("b-2")] } }),
    );
    await page.goto("/projects/p-1/applications/a-1?view=delivery");
    await page.getByRole("button", { name: "新建发布", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "新建发布" });
    const picker = dialog.getByLabel("选择产物", { exact: true });
    await picker.focus();
    await page.keyboard.press("ArrowDown");
    const listbox = page.getByRole("listbox");
    await expect(listbox).toBeVisible();
    const bounds = await listbox.boundingBox();
    expect(bounds).not.toBeNull();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(viewport.width);
    await page.screenshot({
      path: testInfo.outputPath(`artifact-picker-${viewport.width}.png`),
    });
    await page.keyboard.press("Enter");
    await expect(listbox).toHaveCount(0);
    await expect(picker).toHaveAttribute("data-value", "i-1");
  });
}
