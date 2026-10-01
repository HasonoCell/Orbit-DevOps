import { expect, test } from "@playwright/test";
import {
  application,
  diagnostic,
  principal,
  project,
  release,
  target,
  timestamp,
} from "./fixtures/overview";

const operation = diagnostic().releaseOperation;
const detail = {
  release: release.release,
  releaseOperation: operation,
  snapshotDifferences: [
    { field: "replicas", releaseValue: "2", currentValue: "3" },
  ],
  auditTimeline: [
    {
      id: "audit-1",
      actorId: "u-1",
      actorKind: "user",
      action: "release.create",
      targetType: "release",
      targetId: "r-1",
      summary: {},
      createdAt: timestamp,
    },
  ],
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
        role: "owner",
        allowed: ["read", "read_logs", "develop", "resolve_unknown"],
      },
    }),
  );
  await page.route("**/api/v1/applications/a-1", (route) =>
    route.fulfill({ json: application }),
  );
  await page.route("**/api/v1/deployment-targets/t-1", (route) =>
    route.fulfill({ json: target }),
  );
  await page.route("**/api/v1/releases/r-1", (route) =>
    route.fulfill({ json: detail }),
  );
  await page.route("**/api/v1/release-operations/ro-1", (route) =>
    route.fulfill({ json: operation }),
  );
});

for (const status of [404, 503]) {
  test(`发布详情返回 ${status} 时显示错误，并可重试恢复`, async ({ page }) => {
    let failed = true;
    await page.route("**/api/v1/releases/r-1", (route) =>
      failed
        ? route.fulfill({
            status,
            json: { code: "release_unavailable", message: "发布暂不可用" },
          })
        : route.fulfill({ json: detail }),
    );
    await page.route("**/api/v1/releases/r-1/diagnostics", (route) =>
      route.fulfill({ json: diagnostic() }),
    );
    await page.goto("/projects/p-1/applications/a-1/releases/r-1");
    await expect(
      page.getByRole("heading", { name: "无法加载发布" }),
    ).toBeVisible();
    await expect(
      page.getByRole("status", { name: "正在加载发布" }),
    ).toHaveCount(0);
    failed = false;
    await page.getByRole("button", { name: "重试", exact: true }).click();
    await expect(page.getByText("Release r-1", { exact: true })).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "无法加载发布" }),
    ).toHaveCount(0);
  });
}

test("发布详情区分执行结论、部分观测与配置差异，并按 Pod 读取运行日志", async ({
  page,
}) => {
  const report = diagnostic();
  report.runtimeReleaseRelation = "different";
  report.workloadObservation.metadata.status = "partial";
  report.eventObservation.metadata.status = "unavailable";
  await page.route("**/api/v1/releases/r-1/diagnostics", (route) =>
    route.fulfill({ json: report }),
  );
  await page.route("**/api/v1/releases/r-1/runtime-logs?*", (route) => {
    const url = new URL(route.request().url());
    expect(url.searchParams.get("podName")).toBe("payment-service-1");
    expect(url.searchParams.get("container")).toBe("app");
    return route.fulfill({
      json: {
        source: "kubernetes",
        observedAt: timestamp,
        releaseId: "r-1",
        podName: "payment-service-1",
        container: "app",
        tailLines: 200,
        previous: false,
        content: "payment service started",
        truncated: true,
      },
    });
  });
  await page.goto("/projects/p-1/applications/a-1/releases/r-1");
  await expect(page.getByText("工作负载观测不完整")).toBeVisible();
  await expect(page.getByText("运行版本与本发布不同")).toBeVisible();
  await expect(page.getByText("快照 2 / 当前 3")).toBeVisible();
  await expect(page.getByText("release.create")).toBeVisible();
  await page
    .getByRole("button", { name: "读取 payment-service-1 / app 日志" })
    .click();
  await expect(page.getByText("payment service started")).toBeVisible();
  await expect(page.getByText("已截断")).toBeVisible();
});

test("运行观测不可用且无 Pod 时不显示健康结论或可选日志", async ({ page }) => {
  const report = diagnostic();
  report.runtimeReleaseRelation = "unknown";
  report.workloadObservation.metadata.status = "unavailable";
  report.workloadObservation.deployment = undefined;
  report.workloadObservation.service = undefined;
  report.workloadObservation.pods = [];
  await page.route("**/api/v1/releases/r-1/diagnostics", (route) =>
    route.fulfill({ json: report }),
  );
  await page.goto("/projects/p-1/applications/a-1/releases/r-1");
  await expect(page.getByText("运行版本尚无法判断")).toBeVisible();
  await expect(page.getByText("未观测到 Pod，暂无可选日志。")).toBeVisible();
  await expect(page.getByRole("button", { name: /读取 .* 日志/ })).toHaveCount(
    0,
  );
});

test("切换 Release 后重新开启五分钟轮询窗口", async ({ page }) => {
  await page.clock.install();
  const active = { ...operation, status: "pending" };
  let firstOperationReads = 0;
  await page.route("**/api/v1/releases/r-2", (route) =>
    route.fulfill({
      json: {
        ...detail,
        release: { ...detail.release, id: "r-2", rollbackOfReleaseId: "r-1" },
        releaseOperation: { ...active, id: "ro-2", releaseId: "r-2" },
      },
    }),
  );
  await page.route("**/api/v1/releases/r-1", (route) =>
    route.fulfill({ json: { ...detail, releaseOperation: active } }),
  );
  await page.route("**/api/v1/release-operations/ro-2", (route) =>
    route.fulfill({ json: { ...active, id: "ro-2", releaseId: "r-2" } }),
  );
  await page.route("**/api/v1/release-operations/ro-1", (route) => {
    firstOperationReads++;
    return route.fulfill({ json: active });
  });
  await page.route("**/api/v1/releases/*/diagnostics", (route) =>
    route.fulfill({ json: diagnostic() }),
  );
  await page.goto("/projects/p-1/applications/a-1/releases/r-2");
  await expect(page.getByText("Release r-2")).toBeVisible();
  await page.clock.fastForward(5 * 60_000 + 1_000);
  await page.getByRole("link", { name: "r-1" }).click();
  await expect(page.getByText("Release r-1")).toBeVisible();
  await expect.poll(() => firstOperationReads).toBeGreaterThan(0);
  const initial = firstOperationReads;
  await page.clock.fastForward(31_000);
  await expect.poll(() => firstOperationReads).toBeGreaterThan(initial);
});
