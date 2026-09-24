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

test.beforeEach(async ({ page }) => {
  for (const path of ["applications/*/builds?*", "applications/*/delivery-pipelines?*", "deployment-targets/*/releases?*"]) {
    await page.route("**/api/v1/" + path, (route) => route.fulfill({ json: { items: [] } }));
  }
});

async function catalog(page: Page, role: "owner" | "viewer" = "owner") {
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ json: { kind: "user", user, mustChangePassword: false } }));
  await page.route("**/api/v1/projects?*", (route) => route.fulfill({ json: { items: [project] } }));
  await page.route("**/api/v1/projects/p-1", (route) => route.fulfill({ json: project }));
  await page.route("**/api/v1/projects/p-1/permissions", (route) => route.fulfill({ json: { projectId: "p-1", role, allowed: role === "owner" ? ["read", "develop"] : ["read"] } }));
  await page.route("**/api/v1/projects/p-1/applications?*", (route) => route.fulfill({ json: { items: [application] } }));
  await page.route("**/api/v1/applications/a-1", (route) => route.fulfill({ json: application }));
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) => route.fulfill({ json: { items: [{ id: "t-1", applicationId: "a-1", stage: "production", clusterRef: "demo", namespace: "yuuki", replicas: 2, containerPort: 8080, createdBy: "u-1", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z" }] } }));
}

test("登录页在桌面与窄屏保持单列，并只展示可用登录方式", async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ status: 401, json: { message: "未登录" } }));
  await page.route("**/api/v1/auth/providers", (route) => route.fulfill({ json: [
    { id: "local", type: "local", displayName: "本地账号", available: true },
    { id: "oidc", type: "oidc", displayName: "企业登录", available: true },
    { id: "disabled", type: "oidc", displayName: "未启用的入口", available: false },
  ] }));
  await page.goto("/login");
  await expect(page.getByRole("heading", { name: "登录 Orbit DevOps" })).toBeVisible();
  await expect(page.getByLabel("登录名")).toBeVisible();
  await expect(page.getByLabel("密码", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "登录", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "企业登录" })).toBeVisible();
  await expect(page.getByText("未启用的入口")).toHaveCount(0);
  await expect(page.getByText("从代码到运行状态")).toHaveCount(0);

  const desktopForm = await page.getByLabel("登录名").boundingBox();
  expect(desktopForm).not.toBeNull();
  expect(Math.abs(desktopForm!.x + desktopForm!.width / 2 - (page.viewportSize()?.width ?? 0) / 2)).toBeLessThan(2);

  await page.setViewportSize({ width: 375, height: 812 });
  await expect(page.getByRole("heading", { name: "登录 Orbit DevOps" })).toBeVisible();
  await expect(page.getByRole("button", { name: "企业登录" })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(375);
});

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
  await page.getByRole("button", { name: "登录", exact: true }).click();
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
  await expect(page.getByText("生产环境")).toBeVisible();
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

test("创建项目的标识已被占用时提示更换标识", async ({ page }) => {
  await catalog(page);
  await page.route("**/api/v1/projects", (route) => route.fulfill({ status: 409, json: { code: "slug_conflict", message: "project slug is already in use" } }));
  await page.goto("/projects");
  await page.getByRole("button", { name: "创建项目" }).first().click();
  await page.getByLabel("项目名称").fill("Another Yuuki");
  await page.getByLabel("项目标识").fill("yuuki");
  await page.getByRole("dialog").getByRole("button", { name: "创建项目" }).click();
  await expect(page.getByText("标识已被占用，请更换标识。")).toBeVisible();
  await expect(page.getByRole("dialog")).toBeVisible();
});

test("创建项目关闭后清理草稿、错误和旧命令键", async ({ page }) => {
  await catalog(page);
  const keys: string[] = [];
  await page.route("**/api/v1/projects", (route) => {
    keys.push(route.request().headers()["idempotency-key"] ?? "");
    return route.fulfill({ status: 409, json: { code: "slug_conflict", message: "project slug is already in use" } });
  });
  await page.goto("/projects");
  await page.getByRole("button", { name: "创建项目" }).first().click();
  await page.getByLabel("项目名称").fill("Discarded");
  await page.getByLabel("项目标识").fill("discarded");
  await page.getByRole("dialog").getByRole("button", { name: "取消" }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page.getByRole("button", { name: "创建项目" }).first().click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByLabel("项目名称")).toHaveValue("");
  await expect(page.getByLabel("项目标识")).toHaveValue("");

  await page.getByLabel("项目名称").fill("Demo");
  await page.getByLabel("项目标识").fill("demo");
  await page.getByRole("dialog").getByRole("button", { name: "创建项目" }).click();
  await expect(page.getByText("标识已被占用，请更换标识。")).toBeVisible();
  await page.getByRole("dialog").getByRole("button", { name: "创建项目" }).click();
  await expect.poll(() => keys.length).toBe(2);
  expect(keys[1]).toBe(keys[0]);
  await expect(page.getByRole("dialog").getByRole("button", { name: "创建项目" })).toBeEnabled();
  await page.getByRole("dialog").getByRole("button", { name: "Close" }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page.getByRole("button", { name: "创建项目" }).first().click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByLabel("项目名称")).toHaveValue("");
  await expect(page.getByText("标识已被占用，请更换标识。")).not.toBeVisible();
  await page.getByLabel("项目名称").fill("Demo");
  await page.getByLabel("项目标识").fill("demo");
  await page.getByRole("dialog").getByRole("button", { name: "创建项目" }).click();
  await expect.poll(() => keys.length).toBe(3);
  expect(keys[0]).not.toBe("");
  expect(keys[2]).not.toBe(keys[0]);
});

test("创建应用的标识已被占用时提示更换标识", async ({ page }) => {
  await catalog(page);
  await page.route("**/api/v1/projects/p-1/applications", (route) => route.fulfill({ status: 409, json: { code: "slug_conflict", message: "application slug is already in use in this project" } }));
  await page.goto("/projects/p-1");
  await page.getByRole("button", { name: "创建应用" }).first().click();
  await page.getByLabel("应用名称").fill("Another Payment Service");
  await page.getByLabel("应用标识").fill("payment-service");
  await page.getByRole("dialog").getByRole("button", { name: "创建应用" }).click();
  await expect(page.getByText("标识已被占用，请更换标识。")).toBeVisible();
  await expect(page.getByRole("dialog")).toBeVisible();
});

test("创建应用按 Esc 关闭后不保留上次错误", async ({ page }) => {
  await catalog(page);
  await page.route("**/api/v1/projects/p-1/applications", (route) => route.fulfill({ status: 409, json: { code: "slug_conflict", message: "application slug is already in use in this project" } }));
  await page.goto("/projects/p-1");
  await page.getByRole("button", { name: "创建应用" }).first().click();
  await page.getByLabel("应用名称").fill("Catalog");
  await page.getByLabel("应用标识").fill("catalog");
  await page.getByRole("dialog").getByRole("button", { name: "创建应用" }).click();
  await expect(page.getByText("标识已被占用，请更换标识。")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page.getByRole("button", { name: "创建应用" }).first().click();
  await expect(page.getByLabel("应用名称")).toHaveValue("");
  await expect(page.getByText("标识已被占用，请更换标识。")).not.toBeVisible();
});

test("项目权限拒绝不会误导用户重新认证", async ({ page }) => {
  await catalog(page);
  await page.route("**/api/v1/projects/p-1/applications", (route) => route.fulfill({ status: 403, json: { code: "project_permission_denied", message: "current project role cannot create applications" } }));
  await page.goto("/projects/p-1");
  await page.getByRole("button", { name: "创建应用" }).first().click();
  await page.getByLabel("应用名称").fill("Catalog");
  await page.getByLabel("应用标识").fill("catalog");
  await page.getByRole("dialog").getByRole("button", { name: "创建应用" }).click();
  await expect(page.getByText("当前账号无权执行此操作，请联系管理员确认权限。")).toBeVisible();
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
  await expect(page.getByRole("heading", { name: "登录 Orbit DevOps" })).toBeVisible();
});

test("会话失效后换账号不会展示旧账号的项目缓存", async ({ page }) => {
  const bob = { ...user, id: "u-2", displayName: "Bob" };
  let identity: "alice" | "expired" | "bob" = "alice";
  let bobProjectsRequested = 0;
  let releaseBobProjects = () => {};
  const bobProjectsGate = new Promise<void>((resolve) => { releaseBobProjects = resolve; });
  await page.route("**/api/v1/users/me", (route) => route.fulfill(identity === "expired" ? { status: 401, json: { message: "会话已失效" } } : { json: { kind: "user", user: identity === "alice" ? user : bob, mustChangePassword: false } }));
  await page.route("**/api/v1/projects?*", async (route) => {
    if (identity === "bob") {
      bobProjectsRequested++;
      await bobProjectsGate;
      return route.fulfill({ json: { items: [{ ...project, id: "p-2", name: "Bob 的项目" }] } });
    }
    return route.fulfill({ json: { items: [project] } });
  });
  await page.route("**/api/v1/projects/p-1", (route) => {
    if (identity === "alice") {
      identity = "expired";
      return route.fulfill({ status: 401, json: { message: "会话已失效" } });
    }
    return route.fulfill({ status: 404, json: { message: "该项目不可见" } });
  });
  await page.route("**/api/v1/auth/providers", (route) => route.fulfill({ json: [{ id: "local", type: "local", displayName: "本地账号", available: true }] }));
  await page.route("**/api/v1/auth/login", (route) => { identity = "bob"; return route.fulfill({ json: { kind: "user", user: bob, mustChangePassword: false } }); });
  await page.route("**/api/v1/projects/p-1/permissions", (route) => route.fulfill({ status: 404, json: { message: "该项目不可见" } }));
  await page.route("**/api/v1/projects/p-1/applications?*", (route) => route.fulfill({ status: 404, json: { message: "该项目不可见" } }));
  await page.goto("/projects");
  await expect(page.getByRole("link", { name: /Yuuki/ })).toBeVisible();
  await page.getByRole("link", { name: /Yuuki/ }).click();
  await expect(page).toHaveURL(/\/login\?next=/);
  await page.getByLabel("登录名").fill("bob");
  await page.getByLabel("密码", { exact: true }).fill("secret");
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await page.getByRole("link", { name: /Orbit DevOps/ }).click();
  await expect(page).toHaveURL(/\/projects$/);
  await expect.poll(() => bobProjectsRequested).toBe(1);
  const oldProjectVisible = await page.getByRole("link", { name: /Yuuki/ }).isVisible();
  releaseBobProjects();
  expect(oldProjectVisible).toBe(false);
  await expect(page.getByRole("link", { name: /Bob 的项目/ })).toBeVisible();
});

test("OIDC 登录后返回原应用深链", async ({ page }) => {
  let authenticated = false;
  await page.route("**/api/v1/users/me", (route) => route.fulfill(authenticated ? { json: { kind: "user", user, mustChangePassword: false } } : { status: 401, json: { message: "未登录" } }));
  await page.route("**/api/v1/auth/providers", (route) => route.fulfill({ json: [{ id: "oidc", type: "oidc", displayName: "企业登录", available: true }] }));
  await page.route("**/api/v1/auth/oidc/oidc/start", (route) => { authenticated = true; return route.fulfill({ json: { authorizationUrl: new URL("/auth/callback", route.request().url()).href } }); });
  await page.route("**/api/v1/projects/p-1", (route) => route.fulfill({ json: project }));
  await page.route("**/api/v1/applications/a-1", (route) => route.fulfill({ json: application }));
  await page.route("**/api/v1/applications/a-1/deployment-targets?*", (route) => route.fulfill({ json: { items: [] } }));
  await page.goto("/projects/p-1/applications/a-1");
  await expect(page).toHaveURL(/\/login\?next=/);
  await page.getByRole("button", { name: "企业登录" }).click();
  await expect(page).toHaveURL(/\/projects\/p-1\/applications\/a-1$/);
});

test("OIDC 回跳没有有效会话时返回登录并保留目标地址", async ({ page }) => {
  await page.addInitScript(() => sessionStorage.setItem("orbit:oidc-return-path", "/projects/p-1"));
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ status: 401, json: { code: "authentication_required", message: "未登录" } }));
  await page.route("**/api/v1/auth/providers", (route) => route.fulfill({ json: [{ id: "local", type: "local", displayName: "本地账号", available: true }] }));
  await page.goto("/auth/callback");
  await expect(page).toHaveURL(/\/login\?next=/);
  await expect(page.getByRole("heading", { name: "登录 Orbit DevOps" })).toBeVisible();
  expect(new URL(page.url()).searchParams.get("next")).toBe("/projects/p-1");
});

test("OIDC 回跳的临时身份错误允许重试", async ({ page }) => {
  let attempts = 0;
  await page.route("**/api/v1/users/me", (route) => {
    attempts++;
    return route.fulfill(attempts === 1
      ? { status: 503, json: { code: "identity_unavailable", message: "身份服务暂不可用" } }
      : { json: { kind: "user", user, mustChangePassword: false } });
  });
  await page.route("**/api/v1/projects?*", (route) => route.fulfill({ json: { items: [project] } }));
  await page.goto("/auth/callback");
  await expect(page.getByRole("heading", { name: "登录未完成" })).toBeVisible();
  const attemptsBeforeRetry = attempts;
  await page.getByRole("button", { name: "重试" }).click();
  await expect(page).toHaveURL(/\/projects$/);
  expect(attempts).toBeGreaterThan(attemptsBeforeRetry);
});

test("创建表单按 OpenAPI 限制名称与标识长度", async ({ page }) => {
  await catalog(page);
  let submissions = 0;
  await page.route("**/api/v1/projects", (route) => { submissions++; return route.fulfill({ status: 400, json: { message: "长度不合法" } }); });
  await page.goto("/projects");
  await page.getByRole("button", { name: "创建项目" }).first().click();
  await page.getByLabel("项目名称").fill("A".repeat(101));
  await page.getByLabel("项目标识").fill("a".repeat(64));
  await page.getByRole("dialog").getByRole("button", { name: "创建项目" }).click();
  await expect(page.getByText("名称不能超过 100 个字符")).toBeVisible();
  await expect(page.getByText("标识不能超过 63 个字符")).toBeVisible();
  expect(submissions).toBe(0);
});

test("OIDC-only 用户可以首次设置本地登录密码", async ({ page }) => {
  let authenticated = true;
  await page.route("**/api/v1/users/me", (route) => route.fulfill(authenticated ? { json: { kind: "user", user, mustChangePassword: false } } : { status: 401, json: { message: "需要重新登录" } }));
  await page.route("**/api/v1/auth/providers", (route) => route.fulfill({ json: [{ id: "oidc", type: "oidc", displayName: "企业登录", available: true }] }));
  let body: Record<string, unknown> | undefined;
  await page.route("**/api/v1/users/me/password", (route) => { body = route.request().postDataJSON(); authenticated = false; return route.fulfill({ status: 204 }); });
  await page.goto("/account");
  await page.getByRole("link", { name: /密码/ }).click();
  await page.getByRole("button", { name: "首次设置本地密码" }).click();
  await page.getByLabel("登录名").fill("alice-local");
  await page.getByLabel("新密码", { exact: true }).fill("long-password-123");
  await page.getByLabel("确认新密码").fill("long-password-123");
  await page.getByRole("button", { name: "确认设置" }).click();
  await expect.poll(() => body).toEqual({ loginName: "alice-local", newPassword: "long-password-123" });
});

test("首次设置本地密码遇到登录名冲突时提示更换名称", async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ json: { kind: "user", user, mustChangePassword: false } }));
  await page.route("**/api/v1/users/me/password", (route) => route.fulfill({ status: 409, json: { code: "login_name_conflict", message: "login name is already in use" } }));
  await page.goto("/account/password");
  await page.getByRole("button", { name: "首次设置本地密码" }).click();
  await page.getByLabel("登录名").fill("alice-local");
  await page.getByLabel("新密码", { exact: true }).fill("long-password-123");
  await page.getByLabel("确认新密码").fill("long-password-123");
  await page.getByRole("button", { name: "确认设置" }).click();
  await expect(page.getByText("登录名已被占用，请更换登录名。")).toBeVisible();
  await expect(page).toHaveURL(/\/account\/password$/);
});

test("首次设置本地密码遇到近期认证过期时提示重新登录", async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ json: { kind: "user", user, mustChangePassword: false } }));
  await page.route("**/api/v1/users/me/password", (route) => route.fulfill({ status: 403, json: { code: "recent_authentication_required", message: "recent authentication required" } }));
  await page.goto("/account/password");
  await page.getByRole("button", { name: "首次设置本地密码" }).click();
  await page.getByLabel("登录名").fill("alice-local");
  await page.getByLabel("新密码", { exact: true }).fill("long-password-123");
  await page.getByLabel("确认新密码").fill("long-password-123");
  await page.getByRole("button", { name: "确认设置" }).click();
  await expect(page.getByText("身份验证已过期，请重新登录后再试。")).toBeVisible();
});

test("已有本地密码账号提交旧密码和新密码", async ({ page }) => {
  let authenticated = true;
  await page.route("**/api/v1/users/me", (route) => route.fulfill(authenticated ? { json: { kind: "user", user, mustChangePassword: false } } : { status: 401, json: { message: "需要重新登录" } }));
  await page.route("**/api/v1/auth/providers", (route) => route.fulfill({ json: [{ id: "local", type: "local", displayName: "本地账号", available: true }] }));
  let body: Record<string, unknown> | undefined;
  await page.route("**/api/v1/users/me/password", (route) => { body = route.request().postDataJSON(); authenticated = false; return route.fulfill({ status: 204 }); });
  await page.goto("/account/password");
  await page.getByLabel("当前密码").fill("old-password");
  await page.getByLabel("新密码", { exact: true }).fill("new-password-123");
  await page.getByLabel("确认新密码").fill("new-password-123");
  await page.getByRole("button", { name: "确认修改" }).click();
  await expect.poll(() => body).toEqual({ currentPassword: "old-password", newPassword: "new-password-123" });
});

test("新密码表单按后端 Unicode 字符和弱口令规则校验", async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ json: { kind: "user", user, mustChangePassword: false } }));
  let submissions = 0;
  await page.route("**/api/v1/users/me/password", (route) => {
    submissions++;
    return route.fulfill({ status: 400, json: { code: "invalid_password", message: "密码无效" } });
  });
  await page.goto("/account/password");
  await page.getByLabel("当前密码").fill("old-password");
  const newPassword = page.getByLabel("新密码", { exact: true });
  const confirmPassword = page.getByLabel("确认新密码");
  const submit = page.getByRole("button", { name: "确认修改" });

  await newPassword.fill("Abcdef123456");
  await confirmPassword.fill("Abcdef123456");
  await submit.click();
  await expect(page.getByText("新密码需为 15–128 个字符")).toBeVisible();
  expect(submissions).toBe(0);

  await newPassword.fill("a".repeat(128) + "b");
  await confirmPassword.fill("a".repeat(128) + "b");
  await submit.click();
  await expect(page.getByText("新密码需为 15–128 个字符")).toBeVisible();
  expect(submissions).toBe(0);

  await newPassword.fill("passwordpassword");
  await confirmPassword.fill("passwordpassword");
  await submit.click();
  await expect(page.getByText("请避免常见弱密码或重复同一字符")).toBeVisible();
  expect(submissions).toBe(0);

  await page.getByLabel("当前密码").fill("Abcdefgh1234567");
  await newPassword.fill("Abcdefgh1234567");
  await confirmPassword.fill("Abcdefgh1234567");
  await submit.click();
  await expect(page.getByText("新密码不能与当前密码相同")).toBeVisible();
  expect(submissions).toBe(0);

  const unicodePassword = "🔒".repeat(14) + "A";
  await newPassword.fill(unicodePassword);
  await confirmPassword.fill(unicodePassword);
  await submit.click();
  await expect.poll(() => submissions).toBe(1);
});

test("项目列表游标可以翻页并返回第一页", async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) => route.fulfill({ json: { kind: "user", user, mustChangePassword: false } }));
  await page.route("**/api/v1/projects?*", (route) => {
    const cursor = new URL(route.request().url()).searchParams.get("cursor");
    return route.fulfill({ json: cursor === "page-2" ? { items: [{ ...project, id: "p-2", name: "第二页项目" }] } : { items: [project], nextCursor: "page-2" } });
  });
  await page.goto("/projects");
  await expect(page.getByRole("link", { name: /Yuuki/ })).toBeVisible();
  await page.getByRole("button", { name: "下一页" }).click();
  await expect(page).toHaveURL(/cursor=page-2/);
  await expect(page.getByRole("link", { name: /第二页项目/ })).toBeVisible();
  await page.getByRole("button", { name: "返回第一页" }).last().click();
  await expect(page.getByRole("link", { name: /Yuuki/ })).toBeVisible();
});
