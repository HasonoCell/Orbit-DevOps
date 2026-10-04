import { expect, test, type Page } from "@playwright/test";
import {
  application,
  build,
  diagnostic,
  pipeline,
  principal,
  project,
  release,
  run,
  target,
  workbenchPage,
} from "./fixtures/overview";

async function overview(page: Page) {
  const responses: Record<string, unknown> = {
    "users/me": principal,
    "projects?*": { items: [project] },
    "projects/p-1": project,
    "projects/p-1/permissions": {
      projectId: "p-1",
      role: "owner",
      allowed: ["read", "develop"],
    },
    "projects/p-1/applications?*": { items: [application] },
    "projects/p-1/application-workbench?*": workbenchPage,
    "applications/a-1": application,
    "applications/a-1/deployment-targets?*": { items: [target] },
    "applications/a-1/builds?*": { items: [build] },
    "applications/a-1/delivery-pipelines?*": { items: [pipeline] },
    "deployment-targets/t-1/releases?*": { items: [release] },
    "releases/r-1/diagnostics": diagnostic(),
    "delivery-pipelines/pl-1/runs?*": { items: [run] },
  };
  for (const [path, json] of Object.entries(responses))
    await page.route("**/api/v1/" + path, (route) => route.fulfill({ json }));
}
const applicationURL = "/projects/p-1/applications/a-1";

test("概览与发布详情共用诊断缓存，返回概览后手动刷新仍读取诊断", async ({
  page,
}) => {
  await overview(page);
  let reads = 0;
  await page.route("**/api/v1/releases/r-1/diagnostics", (route) => {
    reads++;
    return route.fulfill({ json: diagnostic() });
  });
  await page.route("**/api/v1/releases/r-1", (route) =>
    route.fulfill({
      json: {
        release: release.release,
        releaseOperation: diagnostic().releaseOperation,
        snapshotDifferences: [],
        auditTimeline: [],
      },
    }),
  );
  await page.route("**/api/v1/release-operations/ro-1", (route) =>
    route.fulfill({ json: diagnostic().releaseOperation }),
  );
  await page.route("**/api/v1/deployment-targets/t-1", (route) =>
    route.fulfill({ json: target }),
  );
  await page.goto(applicationURL);
  await expect(page.getByText("工作负载就绪")).toBeVisible();
  expect(reads).toBe(1);
  await page
    .getByRole("navigation", { name: "应用视图" })
    .getByRole("button", { name: "交付记录" })
    .click();
  await page
    .locator(`a[href="${applicationURL}/releases/r-1"]`)
    .first()
    .click();
  await expect(page.getByText("Release r-1", { exact: true })).toBeVisible();
  expect(reads).toBe(1);
  await page.goBack();
  await page
    .getByRole("navigation", { name: "应用视图" })
    .getByRole("button", { name: "运行诊断" })
    .click();
  await expect(page.getByText("工作负载就绪")).toBeVisible();
  await page.getByRole("button", { name: "刷新概览" }).click();
  await expect.poll(() => reads).toBe(2);
});

test("共享折叠区支持键盘开关，隐藏内容不进入焦点顺序", async ({ page }) => {
  await overview(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto(applicationURL);
  const trigger = page.getByRole("button", {
    name: new RegExp(build.build.sourceCommit.slice(0, 8)),
  });
  const detailLink = page.getByRole("link", {
    name: "查看构建详情",
    includeHidden: true,
  });
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
  await expect(detailLink).toBeHidden();
  await trigger.focus();
  await page.keyboard.press("Enter");
  await expect(trigger).toHaveAttribute("aria-expanded", "true");
  await expect(detailLink).toBeVisible();
  const contentId = await trigger.getAttribute("aria-controls");
  expect(contentId).toBeTruthy();
  await expect(page.locator(`[id=${JSON.stringify(contentId)}]`)).toBeVisible();
  await page.keyboard.press("Space");
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
  await expect(trigger).toBeFocused();
  await expect(detailLink).toBeHidden();
  await page.keyboard.press("Tab");
  await expect(detailLink).not.toBeFocused();
});

test("共享视图按钮保留 URL 参数与选中态，键盘切换后可刷新", async ({
  page,
}) => {
  await overview(page);
  await page.goto(
    applicationURL + "?view=delivery&target=t-1&buildCursor=saved",
  );
  const navigation = page.getByRole("navigation", { name: "应用视图" });
  const delivery = navigation.getByRole("button", { name: "交付记录" });
  const diagnostics = navigation.getByRole("button", { name: "运行诊断" });
  await expect(delivery).toHaveAttribute("aria-pressed", "true");
  await diagnostics.focus();
  await page.keyboard.press("Space");
  await expect(diagnostics).toHaveAttribute("aria-pressed", "true");
  await expect(delivery).toHaveAttribute("aria-pressed", "false");
  const params = new URL(page.url()).searchParams;
  expect(params.get("view")).toBe("diagnostics");
  expect(params.get("target")).toBe("t-1");
  expect(params.get("buildCursor")).toBe("saved");
  await page.reload();
  await expect(diagnostics).toHaveAttribute("aria-pressed", "true");
});

test("发布记录的时间和详情链接留有间距，窄屏可换行", async ({ page }) => {
  await overview(page);
  for (const width of [1440, 375]) {
    await page.setViewportSize({ width, height: 1000 });
    await page.goto(applicationURL + "?view=delivery");
    const record = page
      .locator("section.workbench-panel")
      .filter({ has: page.getByRole("heading", { name: "发布记录" }) })
      .locator("article")
      .first();
    const link = record.getByRole("link", { name: "查看发布详情" });
    await expect(link).toBeVisible();
    const timeBox = await record.locator("time").first().boundingBox();
    const linkBox = await link.boundingBox();
    expect(timeBox).not.toBeNull();
    expect(linkBox).not.toBeNull();
    if (timeBox && linkBox) {
      const sameLine = Math.abs(timeBox.y - linkBox.y) < 4;
      expect(
        sameLine
          ? linkBox.x - (timeBox.x + timeBox.width)
          : linkBox.y - (timeBox.y + timeBox.height),
      ).toBeGreaterThanOrEqual(sameLine ? 16 : 8);
    }
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(width);
  }
});

test("工作台三层导航在桌面和移动端不溢出，摘要展示真实查询内容", async ({
  page,
}) => {
  await overview(page);
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 1000 });
    await page.goto("/projects");
    await page.getByRole("link", { name: /Yuuki/ }).click();
    await expect(page.getByRole("heading", { name: "项目资料" })).toBeVisible();
    await expect(
      page.getByText("应用交付与运行管理", { exact: true }),
    ).toHaveCount(0);
    await expect(page.getByText(/摘要按需读取|运行健康请进入应用/)).toHaveCount(
      0,
    );
    await page.getByRole("link", { name: /Payment Service/ }).click();
    await expect(page.getByText("工作负载就绪")).toBeVisible();
    await expect(page.getByText("交付成功")).toBeVisible();
    await page.getByText("镜像引用与目标差异", { exact: true }).click();
    await page.getByText("展开详情").click();
    await expect(
      page.getByText(build.imageArtifact!.imageReference, { exact: true }),
    ).toHaveCount(2);
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(width);
  }
});

test("当前页筛选保留游标与 URL，不把匹配数显示为项目总数", async ({ page }) => {
  await overview(page);
  await page.route("**/api/v1/projects?*", (route) =>
    route.fulfill({
      json: new URL(route.request().url()).searchParams.has("cursor")
        ? { items: [{ ...project, id: "p-2", name: "Second" }] }
        : { items: [project], nextCursor: "second" },
    }),
  );
  await page.goto("/projects");
  await page.getByLabel("筛选当前页项目").fill("second");
  await expect(page.getByText("当前页没有匹配的结果")).toBeVisible();
  await page.getByRole("button", { name: "下一页" }).click();
  await expect(page).toHaveURL(/q=second&cursor=second/);
  await expect(page.getByRole("link", { name: /Second/ })).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("筛选当前页项目")).toHaveValue("second");
  await expect(page.getByText("本页 1 个项目 · 匹配 1 个")).toBeVisible();
});

test("应用归属不匹配时不挂载任何概览子查询", async ({ page }) => {
  await overview(page);
  await page.route("**/api/v1/applications/a-1", (route) =>
    route.fulfill({ json: { ...application, projectId: "other" } }),
  );
  const requests: string[] = [];
  page.on("request", (request) => {
    if (/deployment-targets|builds|delivery-pipelines/.test(request.url()))
      requests.push(request.url());
  });
  await page.goto(applicationURL);
  await expect(page.getByText("应用不属于该项目")).toBeVisible();
  expect(requests).toEqual([]);
});

test("刷新诊断失败后隐藏旧健康徽标，其他面板仍然可读", async ({ page }) => {
  await overview(page);
  await page.goto(applicationURL);
  await expect(page.getByText("工作负载就绪")).toBeVisible();
  await page.route("**/api/v1/releases/r-1/diagnostics", (route) =>
    route.fulfill({ status: 503, json: { message: "集群暂不可达" } }),
  );
  await page.getByRole("button", { name: "刷新概览" }).click();
  await expect(page.getByText("集群暂不可达")).toBeVisible();
  await expect(page.getByText("工作负载就绪")).toHaveCount(0);
  await expect(page.getByText("交付成功")).toBeVisible();
  await expect(page.getByText("暂无构建记录")).toHaveCount(0);
});

test("不同运行版本与发布成功分别显示，仅构建 Pipeline 不显示成功部署", async ({
  page,
}) => {
  await overview(page);
  const report = diagnostic();
  report.runtimeReleaseRelation = "different";
  report.workloadObservation.deployment!.releaseId = "older-release";
  await page.route("**/api/v1/releases/r-1/diagnostics", (route) =>
    route.fulfill({ json: report }),
  );
  await page.route("**/api/v1/applications/a-1/delivery-pipelines?*", (route) =>
    route.fulfill({
      json: {
        items: [
          {
            ...pipeline,
            revision: { ...pipeline.revision, mode: "build_only" },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/delivery-pipelines/pl-1/runs?*", (route) =>
    route.fulfill({
      json: {
        items: [{ ...run, mode: "build_only", status: "candidate_ready" }],
      },
    }),
  );
  await page.goto(applicationURL);
  await expect(page.getByText("运行其他版本", { exact: true })).toBeVisible();
  await expect(page.getByText(/集群运行 Release older-release/)).toBeVisible();
  await expect(page.getByText("执行成功").first()).toBeVisible();
  await expect(page.getByText("不自动部署")).toBeVisible();
  await expect(page.getByText("部署完成")).toHaveCount(0);
});

test("终态记录停止轮询，诊断继续有界刷新并可手动重启", async ({ page }) => {
  await page.clock.install();
  await overview(page);
  let releases = 0,
    runs = 0,
    diagnostics = 0;
  page.on("request", (request) => {
    if (request.url().includes("/t-1/releases?")) releases++;
    if (request.url().includes("/pl-1/runs?")) runs++;
    if (request.url().endsWith("/r-1/diagnostics")) diagnostics++;
  });
  await page.goto(applicationURL);
  await expect(page.getByText("工作负载就绪")).toBeVisible();
  const initial = { releases, runs, diagnostics };
  await page.clock.fastForward(31_000);
  await expect.poll(() => diagnostics).toBeGreaterThan(initial.diagnostics);
  expect(releases).toBe(initial.releases);
  expect(runs).toBe(initial.runs);
  await page.clock.fastForward(5 * 60_000);
  await expect(
    page.getByText("自动刷新已暂停，点击刷新查看最新状态"),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "刷新概览" })).toBeEnabled();
  const stoppedAt = diagnostics;
  await page.clock.fastForward(60_000);
  expect(diagnostics).toBe(stoppedAt);
  await page.getByRole("button", { name: "刷新概览" }).click();
  await expect.poll(() => diagnostics).toBeGreaterThan(stoppedAt);
});
