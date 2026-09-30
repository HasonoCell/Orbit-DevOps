import { expect, test } from "@playwright/test";
import {
  application,
  principal,
  project,
  target,
  timestamp,
} from "./fixtures/overview";

const pipeline = {
  pipeline: {
    id: "pl-1",
    projectId: "p-1",
    applicationId: "a-1",
    name: "Payment CI",
    currentRevision: 2,
    enabled: true,
    activationGeneration: 1,
    createdBy: "u-1",
    createdAt: timestamp,
    updatedAt: timestamp,
  },
  revision: {
    revision: 2,
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
    mode: "auto_release",
    deploymentTargetId: "t-1",
    createdBy: "u-1",
    createdAt: timestamp,
  },
};
const run = {
  run: {
    id: "dr-1",
    deliveryPipelineId: "pl-1",
    pipelineRevision: 2,
    activationGeneration: 1,
    sourceCommit: "a".repeat(40),
    repositoryUrl: "https://github.com/HasonoCell/Yuuki.git",
    phase: "release_created",
    phaseVersion: 4,
    buildId: "b-1",
    imageArtifactId: "i-1",
    releaseId: "r-1",
    createdAt: timestamp,
    updatedAt: timestamp,
  },
  mode: "auto_release",
  status: "releasing",
  activeStage: "release",
  trigger: {
    eventType: "push",
    repositoryFullName: "HasonoCell/Yuuki",
    gitRef: "refs/heads/main",
    forced: false,
    receivedAt: timestamp,
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
    route.fulfill({ json: { items: [target] } }),
  );
  await page.route("**/api/v1/delivery-pipelines/pl-1", (route) =>
    route.fulfill({ json: pipeline }),
  );
});

test("从 Pipeline 历史进入可分享的 Run，分别查看三步及关联资源", async ({
  page,
}) => {
  await page.route("**/api/v1/delivery-pipelines/pl-1/runs?*", (route) =>
    route.fulfill({ json: { items: [run] } }),
  );
  await page.route("**/api/v1/delivery-runs/dr-1", (route) =>
    route.fulfill({ json: run }),
  );
  await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1");
  await page.getByRole("link", { name: /查看运行/ }).click();
  await expect(page).toHaveURL(/\/runs\/dr-1$/);
  await expect(page.getByText("构建：产物就绪")).toBeVisible();
  await expect(page.getByText("来源校验：来源已校验")).toBeVisible();
  await expect(page.getByText("部署：部署中")).toBeVisible();
  await expect(page.getByRole("link", { name: /查看 Build/ })).toHaveAttribute(
    "href",
    /\/builds\/b-1$/,
  );
  await expect(
    page.getByRole("link", { name: /查看 Artifact/ }),
  ).toHaveAttribute("href", /\/builds\/b-1#artifact$/);
  await expect(
    page.getByRole("link", { name: /查看 Release/ }),
  ).toHaveAttribute("href", /\/releases\/r-1$/);
});

test("历史仅构建 Run 按当时的模式展示，不套用当前 Revision", async ({
  page,
}) => {
  const oldRun = {
    ...run,
    run: {
      ...run.run,
      pipelineRevision: 1,
      phase: "artifact_ready",
      releaseId: undefined,
    },
    mode: "build_only",
    status: "candidate_ready",
    activeStage: undefined,
  };
  await page.route("**/api/v1/delivery-pipelines/pl-1/runs?*", (route) =>
    route.fulfill({ json: { items: [oldRun] } }),
  );
  await page.route("**/api/v1/delivery-runs/dr-1", (route) =>
    route.fulfill({ json: oldRun }),
  );
  await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1");
  await expect(page.getByText("来源校验：不适用")).toBeVisible();
  await expect(page.getByText("部署：不自动部署")).toBeVisible();
  await page.getByRole("link", { name: /查看运行/ }).click();
  await expect(page.getByText("来源校验：不适用")).toBeVisible();
  await expect(page.getByText("部署：不自动部署")).toBeVisible();
});

test("Pipeline 运行历史可刷新并发现新 Webhook Run", async ({ page }) => {
  let visible = false;
  await page.route("**/api/v1/delivery-pipelines/pl-1/runs?*", (route) =>
    route.fulfill({ json: { items: visible ? [run] : [] } }),
  );
  await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1");
  await expect(page.getByText("暂无交付运行")).toBeVisible();
  visible = true;
  await page.getByRole("button", { name: "刷新运行" }).click();
  await expect(page.getByRole("link", { name: /查看运行/ })).toBeVisible();
});

test("阻塞 Run 的重新对账只是接纳后台推进请求", async ({ page }) => {
  await page.clock.install();
  const blocked = {
    ...run,
    run: {
      ...run.run,
      phase: "blocked",
      releaseId: undefined,
      reasonCode: "source_changed",
    },
    status: "blocked",
    activeStage: "source_verification",
  };
  let submitted = false;
  let current = blocked;
  let reads = 0;
  await page.route("**/api/v1/delivery-runs/dr-1", (route) => {
    reads++;
    return route.fulfill({ json: current });
  });
  await page.route("**/api/v1/delivery-runs/dr-1/reconcile", (route) => {
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    submitted = true;
    return route.fulfill({ status: 202, json: blocked });
  });
  await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1/runs/dr-1");
  await expect(page.getByText("原因：source_changed")).toBeVisible();
  await page.clock.fastForward(5 * 60_000 + 1_000);
  await page.getByRole("button", { name: "重新对账", exact: true }).click();
  expect(submitted).toBe(false);
  await page.getByRole("button", { name: "确认重新对账" }).click();
  await expect(
    page.getByText("推进请求已接纳，请稍后刷新运行状态。"),
  ).toBeVisible();
  await expect(page.getByText("已阻塞", { exact: true })).toBeVisible();
  expect(submitted).toBe(true);
  const acceptedAt = reads;
  await page.clock.fastForward(31_000);
  await expect.poll(() => reads).toBeGreaterThan(acceptedAt);
  current = { ...blocked, status: "succeeded" };
  await page.clock.fastForward(31_000);
  await expect(page.getByText("交付成功", { exact: true })).toBeVisible();
  const finishedAt = reads;
  await page.clock.fastForward(60_000);
  expect(reads).toBe(finishedAt);
});

test("Run 详情轮询有界，刷新及切换资源重新开启窗口", async ({ page }) => {
  await page.clock.install();
  let firstReads = 0;
  let secondReads = 0;
  await page.route("**/api/v1/delivery-runs/dr-1", (route) => {
    firstReads++;
    return route.fulfill({ json: run });
  });
  await page.route("**/api/v1/delivery-runs/dr-2", (route) => {
    secondReads++;
    return route.fulfill({ json: { ...run, run: { ...run.run, id: "dr-2" } } });
  });
  await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1/runs/dr-1");
  await expect(page.getByText("Run dr-1", { exact: true })).toBeVisible();
  await page.clock.fastForward(31_000);
  await expect.poll(() => firstReads).toBeGreaterThan(1);
  await page.clock.fastForward(5 * 60_000);
  const stoppedAt = firstReads;
  await page.clock.fastForward(60_000);
  expect(firstReads).toBe(stoppedAt);
  await page.getByRole("button", { name: "刷新运行" }).click();
  await expect.poll(() => firstReads).toBeGreaterThan(stoppedAt);
  await expect(page.getByRole("button", { name: "刷新运行" })).toBeEnabled();
  const refreshedAt = firstReads;
  await page.clock.fastForward(31_000);
  await expect.poll(() => firstReads).toBeGreaterThan(refreshedAt);

  await page.clock.fastForward(5 * 60_000);
  // 保持同一个 Router，验证详情间导航不会沿用前一个资源已到期的本地状态。
  await page.evaluate(() => {
    history.pushState(
      {},
      "",
      "/projects/p-1/applications/a-1/pipelines/pl-1/runs/dr-2",
    );
    window.dispatchEvent(new PopStateEvent("popstate"));
  });
  await expect(page.getByText("Run dr-2", { exact: true })).toBeVisible();
  const switchedAt = secondReads;
  await page.clock.fastForward(31_000);
  await expect.poll(() => secondReads).toBeGreaterThan(switchedAt);
});

test("Run 查询失败后暂停，手动重试恢复观察", async ({ page }) => {
  await page.clock.install();
  let unavailable = false;
  let reads = 0;
  await page.route("**/api/v1/delivery-runs/dr-1", (route) => {
    reads++;
    return unavailable
      ? route.fulfill({
          status: 503,
          json: { code: "unavailable", message: "运行读取失败" },
        })
      : route.fulfill({ json: run });
  });
  await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1/runs/dr-1");
  await expect(page.getByText("Run dr-1", { exact: true })).toBeVisible();
  unavailable = true;
  await page.clock.fastForward(31_000);
  await expect(page.getByText("运行读取失败")).toBeVisible();
  const stoppedAt = reads;
  await page.clock.fastForward(60_000);
  expect(reads).toBe(stoppedAt);
  unavailable = false;
  await page.getByRole("button", { name: "重试", exact: true }).click();
  await expect(page.getByText("Run dr-1", { exact: true })).toBeVisible();
  const resumedAt = reads;
  await page.clock.fastForward(31_000);
  await expect.poll(() => reads).toBeGreaterThan(resumedAt);
});

for (const status of [
  "build_failed",
  "release_canceled",
  "attention_required",
] as const) {
  test(`${status} 显示真实阶段，观察者不能重新对账`, async ({ page }) => {
    await page.route("**/api/v1/projects/p-1/permissions", (route) =>
      route.fulfill({
        json: { projectId: "p-1", role: "viewer", allowed: ["read"] },
      }),
    );
    await page.route("**/api/v1/delivery-runs/dr-1", (route) =>
      route.fulfill({
        json: {
          ...run,
          status,
          activeStage: status === "build_failed" ? "build" : "release",
          run: {
            ...run.run,
            imageArtifactId: status === "build_failed" ? undefined : "i-1",
            releaseId: status === "build_failed" ? undefined : "r-1",
            reasonCode: "operation_stopped",
          },
        },
      }),
    );
    await page.goto("/projects/p-1/applications/a-1/pipelines/pl-1/runs/dr-1");
    await expect(page.getByText("原因：operation_stopped")).toBeVisible();
    await expect(
      page.getByRole("button", { name: "重新对账", exact: true }),
    ).toHaveCount(0);
  });
}
