import { expect, test, type Page, type Route } from "@playwright/test";

const ids = {
  project: "11111111-1111-4111-8111-111111111111",
  application: "22222222-2222-4222-8222-222222222222",
  target: "33333333-3333-4333-8333-333333333333",
  release: "44444444-4444-4444-8444-444444444444",
  releaseOperation: "55555555-5555-4555-8555-555555555555",
  releaseAttempt: "66666666-6666-4666-8666-666666666666",
};
const now = "2026-08-30T12:00:00Z";

test("成功发布显示 ReleaseOperation 终态与 Kubernetes 就绪状态", async ({ page }) => {
  await mockControlPlane(page, "succeeded");
  await page.goto("/");

  await page.getByRole("button", { name: "创建并发布" }).click();

  await expect(page.getByTestId("release-operation-status")).toHaveText(/已成功/);
  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).toContainText("Ready");
  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).toContainText("完整");
  await expect(page.getByText(ids.release, { exact: true })).toBeVisible();
});

test("镜像拉取失败显示结构化失败与 Pod 原因", async ({ page }) => {
  await mockControlPlane(page, "failed");
  await page.goto("/");

  await page.getByRole("button", { name: "模拟拉取失败" }).click();
  await page.getByRole("button", { name: "创建并发布" }).click();

  await expect(page.getByTestId("release-operation-status")).toHaveText(/已失败/);
  await expect(page.getByRole("region", { name: "发布操作" })).toContainText("image_pull_failed");
  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).toContainText("ImagePullBackOff");
  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).toContainText("pod_waiting");
  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).toContainText("部分可用");
  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).not.toContainText("不可用");
});

test("运行中的 Release 不匹配时显示明确提示", async ({ page }) => {
  await mockControlPlane(page, "failed", "different");
  await page.goto("/");

  await page.getByRole("button", { name: "创建并发布" }).click();

  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).toContainText("runtime_release_different");
  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).toContainText("集群当前运行的是另一个 Release");
  await expect(page.getByRole("region", { name: "Kubernetes 实况" })).toContainText("其他 Release");
});

test("取消后的 ReleaseOperation 停止自动轮询", async ({ page }) => {
  const traffic = await mockControlPlane(page, "canceled");
  await page.goto("/");

  await page.getByRole("button", { name: "创建并发布" }).click();
  await expect(page.getByTestId("release-operation-status")).toHaveText("已取消");

  const settledPolls = traffic.releaseOperationPolls();
  await page.waitForTimeout(1_200);
  expect(traffic.releaseOperationPolls()).toBe(settledPolls);
});

test("移动端可以打开和关闭产品导航", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");

  const navigation = page.getByRole("complementary", { name: "产品导航" });
  await expect(navigation).toBeHidden();

  await page.getByRole("button", { name: "打开导航" }).click();
  await expect(navigation).toBeVisible();

  await navigation.getByRole("button", { name: "关闭导航" }).click();
  await expect(navigation).toBeHidden();
});

async function mockControlPlane(
  page: Page,
  terminal: "succeeded" | "failed" | "canceled",
  relation: "matches" | "different" = "matches",
) {
  let releaseOperationPolls = 0;
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (request.method() === "POST") {
      const idempotencyKey = request.headers()["idempotency-key"];
      if (idempotencyKey === undefined || idempotencyKey.length < 8) {
        await json(route, 400, { code: "invalid_idempotency_key", message: "missing key" });
        return;
      }
    }

    if (request.method() === "POST" && pathname === "/api/v1/projects") {
      await json(route, 201, {
        id: ids.project,
        name: "Orbit 示例项目",
        slug: "orbit-demo",
        createdBy: "local-developer",
        createdAt: now,
      });
      return;
    }
    if (request.method() === "POST" && pathname === `/api/v1/projects/${ids.project}/applications`) {
      await json(route, 201, {
        id: ids.application,
        projectId: ids.project,
        name: "演示应用",
        slug: "demo-app",
        createdBy: "local-developer",
        createdAt: now,
      });
      return;
    }
    if (request.method() === "POST" && pathname === `/api/v1/applications/${ids.application}/deployment-targets`) {
      await json(route, 201, targetDocument());
      return;
    }
    if (request.method() === "POST" && pathname === `/api/v1/deployment-targets/${ids.target}/releases`) {
      await json(route, 201, {
        release: releaseDocument(),
        releaseOperation: releaseOperationDocument("pending"),
      });
      return;
    }
    if (request.method() === "GET" && pathname === `/api/v1/release-operations/${ids.releaseOperation}`) {
      releaseOperationPolls += 1;
      await json(route, 200, releaseOperationDocument(releaseOperationPolls < 2 ? "running" : terminal));
      return;
    }
    if (request.method() === "GET" && pathname === `/api/v1/releases/${ids.release}/diagnostics`) {
      await json(route, 200, diagnosticDocument(terminal, relation));
      return;
    }
    await json(route, 404, { code: "not_found", message: pathname });
  });
  return { releaseOperationPolls: () => releaseOperationPolls };
}

function targetDocument() {
  return {
    id: ids.target,
    applicationId: ids.application,
    stage: "development",
    clusterRef: "kind-orbit-devops-s1",
    namespace: "orbit-devops-s1",
    replicas: 1,
    containerPort: 8080,
    createdBy: "local-developer",
    createdAt: now,
    updatedAt: now,
  };
}

function releaseDocument() {
  return {
    id: ids.release,
    deploymentTargetId: ids.target,
    imageReference: "registry.k8s.io/pause@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a",
    targetSnapshot: {
      projectId: ids.project,
      applicationId: ids.application,
      stage: "development",
      clusterRef: "kind-orbit-devops-s1",
      namespace: "orbit-devops-s1",
      replicas: 1,
      containerPort: 8080,
    },
    createdBy: "local-developer",
    createdAt: now,
  };
}

function releaseOperationDocument(status: "pending" | "running" | "succeeded" | "failed" | "canceled") {
  const terminal = status === "succeeded" || status === "failed" || status === "canceled";
  const failed = status === "failed";
  return {
    id: ids.releaseOperation,
    type: "release.deploy",
    releaseId: ids.release,
    createdBy: "local-developer",
    idempotencyKey: "web-release-key",
    status,
    attemptCount: status === "pending" ? 0 : 1,
    ...(failed
      ? {
          errorCode: "image_pull_failed",
          errorSummary: "Kubernetes could not pull the immutable release image",
        }
      : {}),
    createdAt: now,
    updatedAt: now,
    ...(status === "pending" ? {} : { startedAt: now }),
    ...(terminal ? { finishedAt: now } : {}),
    attempts:
      status === "pending"
        ? []
        : [
            {
              id: ids.releaseAttempt,
              number: 1,
              workerId: "playwright-worker",
              status: terminal ? status : "running",
              ...(failed
                ? {
                    errorCode: "image_pull_failed",
                    errorSummary: "Kubernetes could not pull the immutable release image",
                  }
                : {}),
              startedAt: now,
              ...(terminal ? { finishedAt: now } : {}),
            },
          ],
  };
}

function diagnosticDocument(
  terminal: "succeeded" | "failed" | "canceled",
  relation: "matches" | "different",
) {
  const succeeded = terminal === "succeeded";
  return {
    release: releaseDocument(),
    releaseOperation: releaseOperationDocument(terminal),
    targetDifferences: [],
    runtimeReleaseRelation: relation,
    workloadObservation: {
      metadata: {
        source: "kubernetes",
        observedAt: now,
        status: succeeded ? "complete" : "partial",
        errorCategories: succeeded ? [] : ["events_unavailable"],
      },
      deployment: {
        name: "orbit-devops-33333333333343338333333333333333",
        uid: "deployment-uid",
        ownershipMatches: true,
        releaseId: relation === "matches" ? ids.release : "77777777-7777-4777-8777-777777777777",
        generation: 1,
        observedGeneration: 1,
        desiredReplicas: 1,
        updatedReplicas: 1,
        readyReplicas: succeeded ? 1 : 0,
        availableReplicas: succeeded ? 1 : 0,
        conditions: [],
      },
      service: {
        name: "orbit-devops-33333333333343338333333333333333",
        uid: "service-uid",
        ownershipMatches: true,
        ports: [{ name: "http", protocol: "TCP", port: 8080 }],
      },
      pods: [
        {
          name: "orbit-devops-demo-pod",
          uid: "pod-uid",
          createdAt: now,
          phase: succeeded ? "Running" : "Pending",
          ready: succeeded,
          reason: succeeded ? "Ready" : "ImagePullBackOff",
          containers: [
            {
              name: "app",
              ready: succeeded,
              restartCount: succeeded ? 0 : 2,
              state: succeeded ? "running" : "waiting",
              reason: succeeded ? "Ready" : "ImagePullBackOff",
              message: "",
            },
          ],
        },
      ],
    },
    eventObservation: {
      metadata: {
        source: "kubernetes",
        observedAt: now,
        status: succeeded ? "complete" : "unavailable",
        errorCategories: succeeded ? [] : ["events_unavailable"],
      },
      items: [],
    },
    signals: [
      ...(relation === "different"
        ? [
            {
              code: "runtime_release_different",
              severity: "warning",
              summary: "集群当前运行的是另一个 Release",
              evidenceRefs: [{ source: "kubernetes", kind: "Deployment", id: "deployment-uid" }],
            },
          ]
        : []),
      ...(succeeded
        ? []
        : [
          {
            code: "pod_waiting",
            severity: "error",
            summary: "Pod 正在等待镜像拉取。",
            evidenceRefs: [{ source: "kubernetes", kind: "Pod", id: "pod-uid" }],
          },
        ]),
    ],
    generatedAt: now,
  };
}

async function json(route: Route, status: number, body: unknown) {
  await route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
}
