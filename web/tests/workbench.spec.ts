import { chooseOption } from "./helpers/select";
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

const appURL = "/projects/p-1/applications/a-1";
async function mock(page: Page, develop = true) {
  const responses: Record<string, unknown> = {
    "users/me": principal,
    "projects/p-1": project,
    "projects/p-1/permissions": {
      projectId: "p-1",
      role: develop ? "owner" : "viewer",
      allowed: develop ? ["read", "develop"] : ["read"],
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

test("项目摘要仅请求当前页，刷新失败后不显示旧摘要", async ({ page }) => {
  await mock(page);
  const summaryRequests: string[] = [];
  const detailRequests: string[] = [];
  page.on("request", (request) => {
    if (request.url().includes("/application-workbench?"))
      summaryRequests.push(request.url());
    if (
      /\/applications\/a-1\/(builds|delivery-pipelines|deployment-targets)|\/diagnostics/.test(
        request.url(),
      )
    )
      detailRequests.push(request.url());
  });
  await page.goto("/projects/p-1");
  await expect(
    page.getByRole("link", { name: /Payment Service/ }),
  ).toBeVisible();
  await expect(page.getByText("仅当前页", { exact: true })).toBeVisible();
  expect(summaryRequests).toHaveLength(1);
  expect(detailRequests).toEqual([]);
  await page.route("**/api/v1/projects/p-1/application-workbench?*", (route) =>
    route.fulfill({ status: 503, json: { message: "摘要服务不可用" } }),
  );
  await page.getByRole("button", { name: "刷新摘要" }).click();
  await expect(page.getByText("摘要服务不可用")).toBeVisible();
  await expect(page.getByRole("link", { name: /Payment Service/ })).toHaveCount(
    0,
  );
});

test("工作台筛选可刷新，失败运行链接保留应用上下文", async ({ page }) => {
  await mock(page);
  await page.route("**/api/v1/projects/p-1/application-workbench?*", (route) =>
    route.fulfill({
      json: {
        items: [
          {
            ...workbenchPage.items[0],
            pipelines: [
              {
                ...workbenchPage.items[0].pipelines[0],
                runStatus: "build_failed",
              },
            ],
          },
        ],
      },
    }),
  );
  await page.goto("/projects/p-1");
  await expect(
    page.getByRole("link", { name: "查看交付与诊断" }),
  ).toBeVisible();
  await expect(page.locator(".attention-panel")).toHaveAttribute(
    "data-attention",
    "true",
  );
  await page.getByLabel("筛选当前页应用").fill("payment");
  await page.getByRole("button", { name: "仅看待处理" }).click();
  await page.reload();
  await expect(page.getByLabel("筛选当前页应用")).toHaveValue("payment");
  await expect(
    page.getByRole("button", { name: "仅看待处理" }),
  ).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("link", { name: "查看交付与诊断" }).click();
  await expect(page).toHaveURL(/applications\/a-1\?view=delivery/);
  await expect(page.getByRole("heading", { name: "发布记录" })).toBeVisible();
});

test("目标深链验证归属，切换目标只观察选中目标并能刷新恢复", async ({
  page,
}) => {
  await mock(page);
  const targetTwo = { ...target, id: "t-2", stage: "development" };
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) =>
    route.fulfill({ json: { items: [target, targetTwo] } }),
  );
  await page.route("**/api/v1/deployment-targets/t-2/releases?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  const requests: string[] = [];
  page.on("request", (request) => {
    if (/\/releases\?|\/diagnostics/.test(request.url()))
      requests.push(request.url());
  });
  await page.goto(appURL + "?target=unrelated");
  await expect(page.getByText("部署目标不属于该应用或已不可用")).toBeVisible();
  expect(requests).toEqual([]);
  await chooseOption(page.getByLabel("部署目标", { exact: true }), "t-2");
  await expect(
    page.getByText("尚无发布记录。运行状态暂不可判断。"),
  ).toBeVisible();
  expect(requests.some((url) => url.includes("/t-1/"))).toBe(false);
  await page.reload();
  await expect(page.getByLabel("部署目标", { exact: true })).toHaveAttribute(
    "data-value",
    "t-2",
  );
  await chooseOption(page.getByLabel("部署目标", { exact: true }), "t-1");
  await expect(page.getByText("工作负载就绪")).toBeVisible();
});

test("部分观测保持未知，Pod 抽屉支持键盘关闭与焦点恢复", async ({ page }) => {
  await mock(page);
  const report = diagnostic();
  report.workloadObservation.metadata.status = "partial";
  report.workloadObservation.metadata.errorCategories = ["service_forbidden"];
  delete report.workloadObservation.service;
  await page.route("**/api/v1/releases/r-1/diagnostics", (route) =>
    route.fulfill({ json: report }),
  );
  await page.goto(appURL);
  await expect(page.getByText("运行观测不完整", { exact: true })).toBeVisible();
  await expect(
    page.getByText("观测不完整，不能确认资源是否存在"),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "payment-service-1", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Pod 详情" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(
    page.getByRole("button", { name: "payment-service-1", exact: true }),
  ).toBeFocused();
});

test("发布确认组件支持键盘和标签点击，未确认不能提交", async ({ page }) => {
  await mock(page);
  let submissions = 0;
  page.on("request", (request) => {
    if (request.method() === "POST" && request.url().endsWith("/releases"))
      submissions++;
  });
  await page.goto(appURL);
  await page.getByRole("button", { name: "新建发布" }).click();
  await chooseOption(page.getByLabel("镜像来源"), "reference");
  await page
    .getByLabel("镜像引用", { exact: true })
    .fill(build.imageArtifact!.imageReference);
  const confirmation = page.getByRole("checkbox", {
    name: /我确认发布到生产环境/,
  });
  const submit = page.getByRole("button", { name: "确认发布到生产环境" });
  await expect(confirmation).toHaveAttribute("data-slot", "checkbox");
  await expect(submit).toBeDisabled();
  await confirmation.focus();
  await page.keyboard.press("Space");
  await expect(confirmation).toBeChecked();
  await expect(submit).toBeEnabled();
  await page.keyboard.press("Space");
  await expect(confirmation).not.toBeChecked();
  await expect(submit).toBeDisabled();
  await page.locator('label[for="release-confirmation"]').click();
  await expect(confirmation).toBeChecked();
  expect(submissions).toBe(0);
});

test("发布必须有权限、不可变镜像与明确确认", async ({ page }) => {
  await mock(page, false);
  await page.goto(appURL);
  await expect(page.getByRole("button", { name: "新建发布" })).toBeDisabled();
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: { projectId: "p-1", role: "owner", allowed: ["read", "develop"] },
    }),
  );
  await page.reload();
  await page.getByRole("button", { name: "新建发布" }).click();
  await expect(
    page.getByRole("button", { name: "确认发布到生产环境" }),
  ).toBeDisabled();
  await chooseOption(page.getByLabel("镜像来源"), "reference");
  await page
    .getByLabel("镜像引用", { exact: true })
    .fill("registry.example.com/app:latest");
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到生产环境" }).click();
  await expect(
    page.getByText(/请选择成功构建的产物，或输入包含/),
  ).toBeVisible();
});

test("发布重试与关闭重开复用命令键，成功后显示接纳而非部署成功", async ({
  page,
}) => {
  await mock(page);
  const submitted: { key?: string; csrf?: string; body: unknown }[] = [];
  await page.route("**/api/v1/deployment-targets/t-1/releases", (route) => {
    submitted.push({
      key: route.request().headers()["idempotency-key"],
      csrf: route.request().headers()["x-orbit-csrf"],
      body: route.request().postDataJSON(),
    });
    return route.fulfill(
      submitted.length < 3
        ? { status: 503, json: { message: "接纳结果暂不可确认" } }
        : {
            status: 202,
            json: {
              release: release.release,
              releaseOperation: diagnostic().releaseOperation,
            },
          },
    );
  });
  await page.route("**/api/v1/releases/r-1", (route) =>
    route.fulfill({
      json: {
        ...release,
        releaseOperation: { ...release.releaseOperation, status: "pending" },
        snapshotDifferences: [],
        auditTimeline: [],
      },
    }),
  );
  await page.route("**/api/v1/deployment-targets/t-1", (route) =>
    route.fulfill({ json: target }),
  );
  await page.route("**/api/v1/release-operations/ro-1", (route) =>
    route.fulfill({
      json: { ...diagnostic().releaseOperation, status: "pending" },
    }),
  );
  await page.goto(appURL);
  await page.getByRole("button", { name: "新建发布" }).click();
  await chooseOption(page.getByLabel("选择产物"), "i-1");
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到生产环境" }).click();
  await expect(
    page.getByText("接纳结果暂不可确认", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "确认发布到生产环境" }).click();
  await expect.poll(() => submitted.length).toBe(2);
  await expect(
    page.getByRole("button", { name: "确认发布到生产环境" }),
  ).toBeEnabled();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page.getByRole("button", { name: "新建发布" }).click();
  await expect(page.getByLabel("选择产物")).toHaveAttribute(
    "data-value",
    "i-1",
  );
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到生产环境" }).click();
  await expect(page).toHaveURL(/\/releases\/r-1$/);
  await expect(page.getByRole("heading", { name: "生产发布" })).toBeVisible();
  await expect(page.getByText("排队中", { exact: true }).first()).toBeVisible();
  expect(new Set(submitted.map((item) => item.key)).size).toBe(1);
  expect(submitted[0].key).toBeTruthy();
  expect(submitted[0].csrf).toBe("1");
  expect(submitted[0].body).toEqual({
    imageReference: build.imageArtifact!.imageReference,
    imageArtifactId: "i-1",
  });
});

test("移动工作台与导航没有横向溢出", async ({ page }) => {
  await mock(page);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/projects/p-1");
  await expect(
    page.getByRole("link", { name: /Payment Service/ }),
  ).toBeVisible();
  await expect(page.getByText("本页 1 个应用 · 匹配 1 个")).toBeVisible();
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(375);
  await page.getByRole("button", { name: "打开导航" }).click();
  await page
    .getByRole("dialog")
    .getByRole("link", { name: "项目工作台" })
    .click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await page.getByRole("link", { name: /Payment Service/ }).click();
  await expect(page.getByText("工作负载就绪")).toBeVisible();
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth),
  ).toBeLessThanOrEqual(375);
});

test("项目应用按已发现的页码跳转，刷新后仍可返回前页", async ({ page }) => {
  await mock(page);
  const second = { ...application, id: "a-2", name: "第二页应用" };
  const third = { ...application, id: "a-3", name: "第三页应用" };
  await page.route(
    "**/api/v1/projects/p-1/application-workbench?*",
    (route) => {
      const cursor = new URL(route.request().url()).searchParams.get("cursor");
      return route.fulfill({
        json:
          cursor === "page-2"
            ? {
                items: [{ ...workbenchPage.items[0], application: second }],
                nextCursor: "page-3",
              }
            : cursor === "page-3"
              ? { items: [{ ...workbenchPage.items[0], application: third }] }
              : { ...workbenchPage, nextCursor: "page-2" },
      });
    },
  );

  await page.goto("/projects/p-1");
  await expect(page.getByRole("button", { name: "第 1 页" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await page.getByRole("button", { name: "第 2 页" }).click();
  await expect(page.getByRole("link", { name: /第二页应用/ })).toBeVisible();
  await page.getByRole("button", { name: "第 3 页" }).click();
  await expect(page.getByRole("link", { name: /第三页应用/ })).toBeVisible();
  await page.reload();
  await expect(page.getByRole("button", { name: "第 3 页" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await page.getByRole("button", { name: "第 2 页" }).click();
  await expect(page.getByRole("link", { name: /第二页应用/ })).toBeVisible();
});
