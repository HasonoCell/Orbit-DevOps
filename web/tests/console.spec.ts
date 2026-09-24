import { expect, test, type Page } from "@playwright/test";

const user = {
  id: "u-1",
  displayName: "Alice",
  status: "active",
  platformRole: "user",
  createdAt: "2026-01-01T00:00:00Z",
};
const project = { id: "p-1", name: "Yuuki", slug: "yuuki", createdBy: "u-1", createdAt: "2026-01-01T00:00:00Z" };
const application = { id: "a-1", projectId: "p-1", name: "Payment Service", slug: "payment-service", createdBy: "u-1", createdAt: "2026-01-01T00:00:00Z" };

async function catalog(page: Page, role: "owner" | "viewer" = "owner") {
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ json: { kind: "user", user, mustChangePassword: false } }));
  await page.route("**/api/v1/projects?*", (route) => route.fulfill({ json: { items: [project] } }));
  await page.route("**/api/v1/projects/p-1", (route) => route.fulfill({ json: project }));
  await page.route("**/api/v1/projects/p-1/permissions", (route) => route.fulfill({ json: { projectId: "p-1", role, allowed: role === "owner" ? ["read", "develop"] : ["read"] } }));
  await page.route("**/api/v1/projects/p-1/applications?*", (route) => route.fulfill({ json: { items: [application] } }));
  await page.route("**/api/v1/applications/a-1", (route) => route.fulfill({ json: application }));
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) => route.fulfill({ json: { items: [{ id: "t-1", applicationId: "a-1", stage: "production", clusterRef: "demo", namespace: "yuuki", replicas: 2, containerPort: 8080, createdBy: "u-1", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z" }] } }));
}

test("未登录时保留目标地址，登录后返回应用", async ({ page }) => {
  let authenticated = false;
  await page.route("**/api/v1/users/me", (route) => route.fulfill(authenticated ? { json: { kind: "user", user, mustChangePassword: false } } : { status: 401, json: { code: "unauthorized", message: "未登录" } }));
  await page.route("**/api/v1/auth/providers", (route) => route.fulfill({ json: [{ id: "local", type: "local", displayName: "本地账号", available: true }] }));
  await page.route("**/api/v1/auth/login", (route) => { authenticated = true; return route.fulfill({ json: { kind: "user", user, mustChangePassword: false } }); });
  await page.route("**/api/v1/projects/p-1", (route) => route.fulfill({ json: project }));
  await page.route("**/api/v1/applications/a-1", (route) => route.fulfill({ json: application }));
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) => route.fulfill({ json: { items: [] } }));
  await page.goto("/projects/p-1/applications/a-1");
  await expect(page).toHaveURL(/\/login\?next=/);
  await page.getByLabel("登录名").fill("alice");
  await page.getByLabel("密码", { exact: true }).fill("secret");
  await page.getByRole("button", { name: "使用本地账号登录" }).click();
  await expect(page).toHaveURL(/\/projects\/p-1\/applications\/a-1$/);
  await expect(page.getByRole("heading", { name: "Payment Service" })).toBeVisible();
});

test("待准入与临时密码分别阻断工作区", async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ json: { kind: "pending", externalIdentity: { displayName: "Bob" }, mustChangePassword: false } }));
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/admission$/);
  await expect(page.getByRole("heading", { name: "等待管理员准入" })).toBeVisible();
  await page.unroute("**/api/v1/users/me");
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ json: { kind: "user", user, mustChangePassword: true } }));
  await page.reload();
  await expect(page).toHaveURL(/\/account\/password$/);
  await expect(page.getByRole("heading", { name: "设置新密码" })).toBeVisible();
});

test("项目与应用深链可刷新，观察者看不到可用的创建操作", async ({ page }) => {
  await catalog(page, "viewer");
  await page.goto("/projects/p-1");
  await expect(page.getByRole("heading", { name: "Yuuki" })).toBeVisible();
  await expect(page.getByRole("button", { name: "创建应用" }).first()).toBeDisabled();
  await page.getByRole("link", { name: /Payment Service/ }).click();
  await expect(page.getByText("生产阶段")).toBeVisible();
  await page.reload();
  await expect(page.getByRole("heading", { name: "Payment Service" })).toBeVisible();
});

test("创建项目使用幂等键，并进入新项目", async ({ page }) => {
  await catalog(page);
  let submittedKey = "";
  await page.route("**/api/v1/projects", (route) => {
    submittedKey = route.request().headers()["idempotency-key"] ?? "";
    return route.fulfill({ json: { ...project, id: "p-2", name: "Demo", slug: "demo" } });
  });
  await page.route("**/api/v1/projects/p-2", (route) => route.fulfill({ json: { ...project, id: "p-2", name: "Demo", slug: "demo" } }));
  await page.route("**/api/v1/projects/p-2/permissions", (route) => route.fulfill({ json: { projectId: "p-2", role: "owner", allowed: ["read", "develop"] } }));
  await page.route("**/api/v1/projects/p-2/applications?*", (route) => route.fulfill({ json: { items: [] } }));
  await page.goto("/projects");
  await page.getByRole("button", { name: "创建项目" }).first().click();
  await page.getByLabel("项目名称").fill("Demo");
  await page.getByLabel("项目标识").fill("demo");
  await page.getByRole("dialog").getByRole("button", { name: "创建项目" }).click();
  await expect(page).toHaveURL(/\/projects\/p-2$/);
  expect(submittedKey).not.toBe("");
});

test("项目 owner 可以创建应用，移动端可打开导航", async ({ page }) => {
  await catalog(page);
  await page.route("**/api/v1/projects/p-1/applications", (route) => route.fulfill({ json: { ...application, id: "a-2", name: "Catalog", slug: "catalog" } }));
  await page.route("**/api/v1/applications/a-2", (route) => route.fulfill({ json: { ...application, id: "a-2", name: "Catalog", slug: "catalog" } }));
  await page.route("**/api/v1/applications/a-2/deployment-targets?*", (route) => route.fulfill({ json: { items: [] } }));
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/projects/p-1");
  await page.getByRole("button", { name: "打开导航" }).click();
  await expect(page.getByRole("navigation", { name: "主导航" })).toBeVisible();
  await page.getByRole("button", { name: "Close" }).click();
  await page.getByRole("button", { name: "创建应用" }).first().click();
  await page.getByLabel("应用名称").fill("Catalog");
  await page.getByLabel("应用标识").fill("catalog");
  await page.getByRole("dialog").getByRole("button", { name: "创建应用" }).click();
  await expect(page).toHaveURL(/\/applications\/a-2$/);
});

test("业务请求 401 会重新核验会话并返回登录", async ({ page }) => {
  let sessionValid = true;
  await page.route("**/api/v1/users/me", (route) => route.fulfill(sessionValid ? { json: { kind: "user", user, mustChangePassword: false } } : { status: 401, json: { message: "会话已失效" } }));
  await page.route("**/api/v1/projects?*", (route) => { sessionValid = false; return route.fulfill({ status: 401, json: { message: "会话已失效" } }); });
  await page.route("**/api/v1/auth/providers", (route) => route.fulfill({ json: [{ id: "local", type: "local", displayName: "本地账号", available: true }] }));
  await page.goto("/projects");
  await expect(page).toHaveURL(/\/login\?next=/);
  await expect(page.getByText("登录工作区")).toBeVisible();
});
