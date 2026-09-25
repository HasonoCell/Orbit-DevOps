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

test("项目摘要限定当前页且不调用 Kubernetes 诊断，局部失败不会伪装成空数据", async ({
  page,
}) => {
  await mock(page);
  const diagnostics: string[] = [];
  page.on("request", (request) => {
    if (request.url().includes("/diagnostics")) diagnostics.push(request.url());
  });
  await page.route("**/api/v1/applications/a-1/builds?*", (route) =>
    route.fulfill({ status: 503, json: { message: "构建服务不可用" } }),
  );
  await page.goto("/projects/p-1");
  await expect(page.getByText(/1 个应用的摘要读取不完整/)).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "读取失败", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("工作负载就绪")).toHaveCount(0);
  await expect(page.getByText(/仅当前页，不代表项目总量/)).toBeVisible();
  expect(diagnostics).toEqual([]);
  await page.getByRole("button", { name: "仅看待处理" }).click();
  await expect(page.getByText("当前页没有匹配的结果")).toBeVisible();
  await expect(page.getByText(/摘要不完整，不能确认/)).toBeVisible();
  await expect(page.locator(".attention-panel")).toHaveAttribute(
    "data-attention",
    "false",
  );
});

test("工作台筛选可刷新，失败运行链接保留应用上下文", async ({ page }) => {
  await mock(page);
  await page.route("**/api/v1/delivery-pipelines/pl-1/runs?*", (route) =>
    route.fulfill({ json: { items: [{ ...run, status: "build_failed" }] } }),
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
  await page.getByLabel("部署目标", { exact: true }).selectOption("t-2");
  await expect(
    page.getByText("尚无发布记录。运行状态暂不可判断。"),
  ).toBeVisible();
  expect(requests.some((url) => url.includes("/t-1/"))).toBe(false);
  await page.reload();
  await expect(page.getByLabel("部署目标", { exact: true })).toHaveValue("t-2");
  await page.getByLabel("部署目标", { exact: true }).selectOption("t-1");
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
    page.getByRole("button", { name: "确认发布到 production" }),
  ).toBeDisabled();
  await page.getByLabel("镜像来源").selectOption("reference");
  await page
    .getByLabel("镜像引用", { exact: true })
    .fill("registry.example.com/app:latest");
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到 production" }).click();
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
  await page.goto(appURL);
  await page.getByRole("button", { name: "新建发布" }).click();
  await page.getByLabel("选择产物").selectOption("i-1");
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到 production" }).click();
  await expect(
    page.getByText("接纳结果暂不可确认", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "确认发布到 production" }).click();
  await expect.poll(() => submitted.length).toBe(2);
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "新建发布" }).click();
  await expect(page.getByLabel("选择产物")).toHaveValue("i-1");
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到 production" }).click();
  await expect(page.getByText(/发布已接纳：r-1/)).toBeVisible();
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
