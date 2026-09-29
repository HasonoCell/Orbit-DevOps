import { expect, test } from "@playwright/test";

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
  sourceCommit: "a".repeat(40),
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
  status: "attention_required",
  attemptCount: 1,
  automaticRetryCount: 0,
  recoveryRequired: true,
  errorCode: "outcome_unknown",
  errorSummary: "Job 结果未知",
  queuedAt: "2026-01-01T00:00:00Z",
  availableAt: "2026-01-01T00:00:00Z",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:01:00Z",
  attempts: [
    {
      id: "ba-1",
      number: 1,
      workerId: "worker-a",
      status: "outcome_unknown",
      startedAt: "2026-01-01T00:00:10Z",
    },
  ],
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
});

test("结果未知时仅 owner 可确认人工结束，失败重试复用幂等键", async ({
  page,
}) => {
  let requests = 0;
  const keys: string[] = [];
  let operationState = operation;
  await page.route("**/api/v1/builds/b-1", (route) =>
    route.fulfill({ json: { build, buildOperation: operationState } }),
  );
  await page.route("**/api/v1/build-operations/bo-1", (route) =>
    route.fulfill({ json: operationState }),
  );
  await page.route("**/api/v1/build-operations/bo-1/force-fail", (route) => {
    requests++;
    keys.push(route.request().headers()["idempotency-key"]);
    expect(route.request().postDataJSON()).toEqual({
      reason: "已核对 Job，手动结束",
    });
    if (requests === 1)
      return route.fulfill({
        status: 503,
        json: { code: "unavailable", message: "网络中断" },
      });
    operationState = {
      ...operation,
      status: "failed",
      recoveryRequired: false,
      errorSummary: "已核对 Job，手动结束",
    };
    return route.fulfill({ json: operationState });
  });
  await page.goto("/projects/p-1/applications/a-1/builds/b-1");
  await page.getByRole("button", { name: "强制失败" }).click();
  await expect(page.getByText("Job 结果未知")).toBeVisible();
  await page.getByLabel("结束原因").fill("已核对 Job，手动结束");
  await page.getByRole("button", { name: "确认强制失败" }).click();
  await expect(page.getByText("网络中断")).toBeVisible();
  await page.getByRole("button", { name: "确认强制失败" }).click();
  await expect(page.getByText("已核对 Job，手动结束")).toBeVisible();
  expect(keys).toHaveLength(2);
  expect(keys[0]).toBe(keys[1]);
});

for (const scenario of [
  { status: "failed", command: "retry", label: "重试构建", next: "pending" },
  { status: "pending", command: "cancel", label: "取消构建", next: "canceled" },
  {
    status: "attention_required",
    command: "reconcile",
    label: "重新对账",
    next: "pending",
  },
] as const) {
  test(`${scenario.status} 状态可确认${scenario.label}`, async ({ page }) => {
    let current: typeof operation = { ...operation, status: scenario.status };
    await page.route("**/api/v1/builds/b-1", (route) =>
      route.fulfill({ json: { build, buildOperation: current } }),
    );
    await page.route("**/api/v1/build-operations/bo-1", (route) =>
      route.fulfill({ json: current }),
    );
    let called = false;
    await page.route(
      `**/api/v1/build-operations/bo-1/${scenario.command}`,
      (route) => {
        called = true;
        current = { ...current, status: scenario.next };
        return route.fulfill({ json: current });
      },
    );
    await page.goto("/projects/p-1/applications/a-1/builds/b-1");
    await page
      .getByRole("button", { name: scenario.label, exact: true })
      .click();
    await expect(
      page.getByText("Operation bo-1", { exact: true }),
    ).toBeVisible();
    await page.getByRole("button", { name: `确认${scenario.label}` }).click();
    expect(called).toBe(true);
  });
}

test("只读成员不能执行构建命令", async ({ page }) => {
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: { projectId: "p-1", role: "viewer", allowed: ["read"] },
    }),
  );
  await page.route("**/api/v1/builds/b-1", (route) =>
    route.fulfill({ json: { build, buildOperation: operation } }),
  );
  await page.route("**/api/v1/build-operations/bo-1", (route) =>
    route.fulfill({ json: operation }),
  );
  await page.goto("/projects/p-1/applications/a-1/builds/b-1");
  await expect(
    page.getByRole("heading", { name: "构建 aaaaaaaa" }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "强制失败" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "重新对账" })).toHaveCount(0);
});
