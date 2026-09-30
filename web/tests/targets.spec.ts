import { expect, test } from "@playwright/test";

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
const target = {
  id: "t-1",
  applicationId: "a-1",
  stage: "development",
  clusterRef: "kind-local",
  namespace: "orbit-devops",
  replicas: 2,
  containerPort: 8080,
  createdBy: "u-1",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
};

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({
      json: {
        kind: "user",
        user: {
          id: "u-1",
          displayName: "Alice",
          status: "active",
          platformRole: "user",
          createdAt: "2026-01-01T00:00:00Z",
        },
        mustChangePassword: false,
      },
    }),
  );
  await page.route("**/api/v1/projects/p-1", (route) =>
    route.fulfill({ json: project }),
  );
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "owner",
        allowed: ["read", "develop"],
      },
    }),
  );
  await page.route("**/api/v1/applications/a-1", (route) =>
    route.fulfill({ json: application }),
  );
  await page.route("**/api/v1/applications/a-1/builds?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/applications/a-1/delivery-pipelines?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/deployment-targets/t-1/releases?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
});

test("从应用创建目标并修改期望配置，不声称已部署", async ({ page }) => {
  let current = target;
  let targets = [] as (typeof target)[];
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) =>
    route.fulfill({ json: { items: targets } }),
  );
  await page.route("**/api/v1/applications/a-1/deployment-targets", (route) => {
    expect(route.request().method()).toBe("POST");
    expect(route.request().postDataJSON()).toEqual({
      stage: "development",
      replicas: 2,
      containerPort: 8080,
    });
    targets = [target];
    return route.fulfill({ status: 201, json: target });
  });
  await page.route("**/api/v1/deployment-targets/t-1", (route) => {
    if (route.request().method() === "GET")
      return route.fulfill({ json: current });
    expect(route.request().postDataJSON()).toEqual({
      replicas: 3,
      containerPort: 9090,
    });
    current = { ...current, replicas: 3, containerPort: 9090 };
    return route.fulfill({ json: current });
  });
  await page.goto("/projects/p-1/applications/a-1");
  await page.getByRole("button", { name: "创建开发目标" }).click();
  await page.getByLabel("期望副本").fill("2");
  await page.getByLabel("容器端口").fill("8080");
  await page.getByRole("button", { name: "创建部署目标" }).click();
  await expect(page).toHaveURL(/\/targets\/t-1$/);
  await expect(page.getByText("kind-local")).toBeVisible();
  await expect(page.getByText("orbit-devops")).toBeVisible();
  await page.getByLabel("期望副本").fill("3");
  await page.getByLabel("容器端口").fill("9090");
  await page.getByRole("button", { name: "保存配置" }).click();
  await expect(
    page.getByText("配置已保存，将在下一次发布中应用"),
  ).toBeVisible();
});

test("已有阶段不能重复创建，且无权限时隐藏写操作", async ({ page }) => {
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) =>
    route.fulfill({ json: { items: [target] } }),
  );
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "viewer",
        allowed: ["read"],
      },
    }),
  );
  await page.goto("/projects/p-1/applications/a-1");
  await expect(
    page.getByRole("heading", { name: "Payment Service" }),
  ).toBeVisible();
  await expect(page.getByRole("link", { name: "开发目标配置" })).toBeVisible();
  await expect(page.getByRole("button", { name: "创建开发目标" })).toHaveCount(
    0,
  );
  await expect(page.getByRole("button", { name: "创建生产目标" })).toHaveCount(
    0,
  );
});

test("已有开发目标时只允许创建生产目标", async ({ page }) => {
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) =>
    route.fulfill({ json: { items: [target] } }),
  );
  await page.goto("/projects/p-1/applications/a-1");
  await expect(page.getByRole("link", { name: "开发目标配置" })).toBeVisible();
  await expect(page.getByRole("button", { name: "创建开发目标" })).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("button", { name: "创建生产目标" }),
  ).toBeVisible();
});

test("创建目标结果未知后关闭再打开，保留草稿并复用同一幂等键", async ({
  page,
}) => {
  const keys: string[] = [];
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/applications/a-1/deployment-targets", (route) => {
    keys.push(route.request().headers()["idempotency-key"]);
    expect(route.request().postDataJSON()).toEqual({
      stage: "development",
      replicas: 3,
      containerPort: 9090,
    });
    if (keys.length === 1)
      return route.fulfill({
        status: 503,
        json: { message: "结果暂时无法确认" },
      });
    return route.fulfill({ status: 201, json: target });
  });
  await page.route("**/api/v1/deployment-targets/t-1", (route) =>
    route.fulfill({ json: target }),
  );
  await page.route(
    "**/api/v1/deployment-targets/t-1/access-routes?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.goto("/projects/p-1/applications/a-1");
  await page.getByRole("button", { name: "创建开发目标" }).click();
  await page.getByLabel("期望副本").fill("3");
  await page.getByLabel("容器端口").fill("9090");
  await page.getByRole("button", { name: "创建部署目标" }).click();
  await expect(page.getByText("结果暂时无法确认")).toBeVisible();
  await page.getByRole("button", { name: "取消" }).click();
  await page.getByRole("button", { name: "创建开发目标" }).click();
  await expect(page.getByLabel("期望副本")).toHaveValue("3");
  await expect(page.getByLabel("容器端口")).toHaveValue("9090");
  await page.getByRole("button", { name: "创建部署目标" }).click();
  await expect(page).toHaveURL(/\/targets\/t-1$/);
  expect(keys).toHaveLength(2);
  expect(keys[0]).toBeTruthy();
  expect(keys[1]).toBe(keys[0]);
});
