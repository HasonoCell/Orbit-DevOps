import { chooseOption } from "./helpers/select";
import { expect, test } from "@playwright/test";
import { principal, timestamp } from "./fixtures/overview";

const admin = {
  ...principal,
  user: { ...principal.user!, platformRole: "platform_admin" },
};
const owner = {
  id: "u-1",
  displayName: "管理员",
  platformRole: "platform_admin",
  status: "active",
  createdAt: timestamp,
};
const newcomer = {
  id: "u-2",
  displayName: "新同事",
  platformRole: "user",
  status: "active",
  createdAt: timestamp,
};
const fixturePassword = "fixture-only-Temporary-2026!";

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: admin }),
  );
  await page.route("**/api/v1/auth/providers", (route) =>
    route.fulfill({
      json: [
        {
          id: "local",
          type: "local",
          displayName: "本地账号",
          available: true,
        },
      ],
    }),
  );
});

test("管理员创建本地用户、任命角色、停用并重置临时密码", async ({ page }) => {
  let users = [owner];
  let current = newcomer;
  let resetAccepted = false;
  await page.route("**/api/v1/users?*", (route) =>
    route.fulfill({ json: { items: users } }),
  );
  await page.route("**/api/v1/users", (route) => {
    const body = route.request().postDataJSON();
    expect(body.loginName).toBe("colleague");
    expect(body.displayName).toBe("新同事");
    expect(body.temporaryPassword.length).toBeGreaterThanOrEqual(15);
    users = [...users, newcomer];
    return route.fulfill({ status: 201, json: newcomer });
  });
  await page.route("**/api/v1/users/u-2", (route) =>
    route.fulfill({ json: current }),
  );
  await page.route("**/api/v1/users/u-2/platform-role", (route) => {
    expect(route.request().postDataJSON()).toEqual({ role: "platform_admin" });
    current = { ...current, platformRole: "platform_admin" };
    users = [owner, current];
    return route.fulfill({ json: current });
  });
  await page.route("**/api/v1/users/u-2/disable", (route) => {
    current = { ...current, status: "disabled" };
    users = [owner, current];
    return route.fulfill({ json: current });
  });
  await page.route("**/api/v1/users/u-2/password/reset", (route) => {
    expect(
      route.request().postDataJSON().temporaryPassword.length,
    ).toBeGreaterThanOrEqual(15);
    resetAccepted = true;
    return route.fulfill({ status: 204 });
  });
  await page.goto("/platform?view=users");
  await expect(page.getByText("新用户默认没有平台或项目管理权限")).toHaveCount(
    0,
  );
  await page.getByLabel("登录名").fill("colleague");
  await page.getByLabel("显示名称").fill("新同事");
  await page.getByLabel("临时密码", { exact: true }).fill(fixturePassword);
  await page.getByRole("button", { name: "创建用户" }).click();
  await expect(page.getByText(/已创建 新同事/)).toBeVisible();
  await expect(page.getByLabel("临时密码", { exact: true })).toHaveValue("");
  await page.getByRole("button", { name: "管理" }).last().click();
  await chooseOption(page.getByLabel("平台角色"), "platform_admin");
  await page.getByRole("button", { name: "修改角色" }).click();
  await page.getByRole("button", { name: "确认提交" }).click();
  await expect(page.getByText(/已接纳：新同事/)).toBeVisible();
  await page.getByRole("button", { name: "停用账号" }).click();
  await page.getByRole("button", { name: "确认提交" }).click();
  await expect(page.getByText(/当前为停用账号/)).toBeVisible();
  await page
    .getByLabel("临时密码", { exact: false })
    .last()
    .fill(fixturePassword);
  await page.getByRole("button", { name: "重置密码" }).click();
  await expect(page.getByText(/密码重置已接纳/)).toBeVisible();
  expect(resetAccepted).toBe(true);
});

test("最后管理员保护与并发变化不会被误报为成功", async ({ page }) => {
  let latest = owner;
  let writes = 0;
  await page.route("**/api/v1/users?*", (route) =>
    route.fulfill({ json: { items: [owner] } }),
  );
  await page.route("**/api/v1/users/u-1", (route) =>
    route.fulfill({ json: latest }),
  );
  await page.route("**/api/v1/users/u-1/platform-role", (route) => {
    writes++;
    return route.fulfill({
      status: 409,
      json: { code: "last_platform_administrator", message: "last admin" },
    });
  });
  await page.goto("/platform?view=users");
  await page.getByRole("button", { name: "管理" }).click();
  await chooseOption(page.getByLabel("平台角色"), "user");
  await page.getByRole("button", { name: "修改角色" }).click();
  await page.getByRole("button", { name: "确认提交" }).click();
  await expect(page.getByText(/至少需要保留一位有效平台管理员/)).toBeVisible();
  expect(writes).toBe(1);
  latest = { ...owner, status: "disabled" };
  await page.getByRole("button", { name: "确认提交" }).click();
  await expect(page.getByText(/用户状态已变化/)).toBeVisible();
  expect(writes).toBe(1);
});

test("近期认证保留创建草稿且不会自动重提", async ({ page }) => {
  let attempts = 0;
  await page.route("**/api/v1/users?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/users", (route) => {
    attempts++;
    return route.fulfill({
      status: 403,
      json: { code: "recent_authentication_required", message: "recent" },
    });
  });
  await page.route("**/api/v1/auth/reauth/local", (route) =>
    route.fulfill({ json: admin }),
  );
  await page.goto("/platform?view=users");
  await page.getByLabel("登录名").fill("colleague");
  await page.getByLabel("显示名称").fill("新同事");
  await page.getByLabel("临时密码", { exact: true }).fill(fixturePassword);
  await page.getByRole("button", { name: "创建用户" }).click();
  await page.getByLabel("当前密码").fill("fixture-only-recent-proof");
  await page.getByRole("button", { name: "用密码验证" }).click();
  await expect(page.getByText(/身份已验证，请检查原操作/)).toBeVisible();
  await expect(page.getByLabel("临时密码", { exact: true })).toHaveValue(
    fixturePassword,
  );
  expect(attempts).toBe(1);
});

test("普通用户不进入平台管理页", async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: principal }),
  );
  await page.goto("/platform");
  await expect(page.getByText("无平台管理权限")).toBeVisible();
  await expect(page.getByRole("button", { name: "创建用户" })).toHaveCount(0);
});
