import { expect, test } from "@playwright/test";
import type { components } from "../src/api/schema";
import {
  application,
  diagnostic,
  principal,
  project,
  release,
  target,
} from "./fixtures/overview";

const initialOperation = diagnostic().releaseOperation;

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: principal }),
  );
  await page.route("**/api/v1/projects/p-1", (route) =>
    route.fulfill({ json: project }),
  );
  await page.route("**/api/v1/applications/a-1", (route) =>
    route.fulfill({ json: application }),
  );
  await page.route("**/api/v1/deployment-targets/t-1", (route) =>
    route.fulfill({ json: target }),
  );
  await page.route("**/api/v1/releases/*/diagnostics", (route) =>
    route.fulfill({ json: diagnostic() }),
  );
});

test("失败的执行经确认后重试，同一输入的网络重试复用幂等键", async ({
  page,
}) => {
  let operation: components["schemas"]["ReleaseOperation"] = {
    ...initialOperation,
    status: "failed",
  };
  const keys: string[] = [];
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "developer",
        allowed: ["read", "develop"],
      },
    }),
  );
  await page.route("**/api/v1/releases/r-1", (route) =>
    route.fulfill({
      json: {
        release: release.release,
        releaseOperation: operation,
        snapshotDifferences: [],
        auditTimeline: [],
      },
    }),
  );
  await page.route("**/api/v1/release-operations/ro-1", (route) =>
    route.fulfill({ json: operation }),
  );
  await page.route("**/api/v1/release-operations/ro-1/retry", (route) => {
    keys.push(route.request().headers()["idempotency-key"]);
    if (keys.length === 1)
      return route.fulfill({ status: 503, json: { message: "暂时不可用" } });
    operation = { ...operation, status: "pending" };
    return route.fulfill({ json: operation });
  });
  await page.goto("/projects/p-1/applications/a-1/releases/r-1");
  await page.getByRole("button", { name: "重试执行" }).click();
  await expect(
    page.getByText("重新排队同一 Operation。", { exact: false }),
  ).toBeVisible();
  await page.getByRole("button", { name: "确认重试执行" }).click();
  await expect(page.getByRole("alert").getByText(/结果未确认/)).toBeVisible();
  await page.getByRole("button", { name: "确认重试执行" }).click();
  await expect(page.getByRole("button", { name: "取消执行" })).toBeVisible();
  expect(keys).toHaveLength(2);
  expect(keys[0]).toBeTruthy();
  expect(keys[1]).toBe(keys[0]);
});

test("回滚创建新的 Release 并跳转，不改写历史 Release", async ({ page }) => {
  const rollback = {
    release: {
      ...release.release,
      id: "r-2",
      rollbackOfReleaseId: "r-1",
    },
    releaseOperation: {
      ...initialOperation,
      id: "ro-2",
      releaseId: "r-2",
      status: "pending" as const,
    },
  };
  let posted = false;
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "developer",
        allowed: ["read", "develop"],
      },
    }),
  );
  await page.route("**/api/v1/releases/r-1", (route) =>
    route.fulfill({
      json: {
        release: release.release,
        releaseOperation: initialOperation,
        snapshotDifferences: [],
        auditTimeline: [],
      },
    }),
  );
  await page.route("**/api/v1/releases/r-2", (route) =>
    route.fulfill({
      json: { ...rollback, snapshotDifferences: [], auditTimeline: [] },
    }),
  );
  await page.route("**/api/v1/release-operations/ro-1", (route) =>
    route.fulfill({ json: initialOperation }),
  );
  await page.route("**/api/v1/release-operations/ro-2", (route) =>
    route.fulfill({ json: rollback.releaseOperation }),
  );
  await page.route("**/api/v1/releases/r-1/rollback", (route) => {
    posted = true;
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    return route.fulfill({ status: 201, json: rollback });
  });
  await page.goto("/projects/p-1/applications/a-1/releases/r-1");
  await page.getByRole("button", { name: "回滚到此发布" }).click();
  await expect(page.getByText(/以此历史 Release 的不可变快照/)).toBeVisible();
  expect(posted).toBe(false);
  await page.getByRole("button", { name: "确认回滚到此发布" }).click();
  await expect(page).toHaveURL(/\/releases\/r-2$/);
  await expect(page.getByText("回滚来源")).toBeVisible();
  expect(posted).toBe(true);
});

test("无开发权限时不展示执行命令及回滚", async ({ page }) => {
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: { projectId: "p-1", role: "viewer", allowed: ["read"] },
    }),
  );
  await page.route("**/api/v1/releases/r-1", (route) =>
    route.fulfill({
      json: {
        release: release.release,
        releaseOperation: initialOperation,
        snapshotDifferences: [],
        auditTimeline: [],
      },
    }),
  );
  await page.route("**/api/v1/release-operations/ro-1", (route) =>
    route.fulfill({ json: initialOperation }),
  );
  await page.goto("/projects/p-1/applications/a-1/releases/r-1");
  await expect(page.getByRole("button", { name: "回滚到此发布" })).toHaveCount(
    0,
  );
  await expect(page.getByRole("heading", { name: "执行操作" })).toHaveCount(0);
});
