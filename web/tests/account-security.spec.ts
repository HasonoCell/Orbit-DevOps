import { expect, test } from "@playwright/test";
import { principal, timestamp } from "./fixtures/overview";

const linked = {
  id: "identity-1",
  providerId: "oidc",
  status: "linked",
  userId: "u-1",
  displayName: "演示身份",
  email: "alice@example.com",
  emailVerified: true,
  createdAt: timestamp,
  updatedAt: timestamp,
};

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: principal }),
  );
  await page.route("**/api/v1/users/me/sessions?*", (route) =>
    route.fulfill({
      json: {
        items: [
          {
            id: "session-1",
            method: "password",
            createdAt: timestamp,
            lastSeenAt: timestamp,
            expiresAt: timestamp,
            current: true,
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/users/me/external-identities?*", (route) =>
    route.fulfill({ json: { items: [linked] } }),
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
        { id: "oidc", type: "oidc", displayName: "企业登录", available: true },
      ],
    }),
  );
});

test("账号页分页显示有效会话并确认退出全部", async ({ page }) => {
  let revoked = false;
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill(
      revoked
        ? {
            status: 401,
            json: { code: "authentication_required", message: "未登录" },
          }
        : { json: principal },
    ),
  );
  await page.route("**/api/v1/auth/logout-all", (route) => {
    revoked = true;
    return route.fulfill({ status: 204 });
  });
  await page.goto("/account");
  await expect(page.getByRole("heading", { name: "账号信息" })).toBeVisible();
  await expect(page.getByText("个人账号", { exact: true })).toHaveCount(0);
  await expect(page.getByText(/可以修改已有的本地密码/)).toHaveCount(0);
  await expect(page.getByText("当前会话")).toBeVisible();
  await expect(page.getByText("演示身份")).toBeVisible();
  await page.getByRole("button", { name: "退出全部会话" }).click();
  await expect(page.getByText(/当前浏览器也需要重新登录/)).toBeVisible();
  expect(revoked).toBe(false);
  await page.getByRole("button", { name: "确认退出全部" }).click();
  await expect(page).toHaveURL(/\/login$/);
  expect(revoked).toBe(true);
});

test("绑定 OIDC 后回到账号页读取服务端身份", async ({ page }) => {
  let bound = false;
  await page.route("**/api/v1/users/me/external-identities?*", (route) =>
    route.fulfill({ json: { items: bound ? [linked] : [] } }),
  );
  await page.route(
    "**/api/v1/users/me/external-identities/oidc/bind",
    (route) => {
      bound = true;
      return route.fulfill({
        json: {
          authorizationUrl: new URL("/auth/callback", route.request().url())
            .href,
          expiresAt: timestamp,
        },
      });
    },
  );
  await page.goto("/account");
  await expect(page.getByText("暂无外部身份")).toBeVisible();
  await page.getByRole("button", { name: "绑定 企业登录" }).click();
  await expect(page).toHaveURL(/\/account$/);
  await expect(page.getByText("演示身份")).toBeVisible();
  expect(bound).toBe(true);
});

test("最后一个登录方式拒绝解绑，近期认证成功后仍需手动重提", async ({
  page,
}) => {
  let attempts = 0;
  let proof = 0;
  await page.route(
    "**/api/v1/users/me/external-identities/identity-1",
    (route) => {
      attempts++;
      return route.fulfill({
        status: attempts === 1 ? 403 : 409,
        json:
          attempts === 1
            ? { code: "recent_authentication_required", message: "recent" }
            : { code: "last_login_method", message: "last login" },
      });
    },
  );
  await page.route("**/api/v1/auth/reauth/local", (route) => {
    proof++;
    expect(route.request().postDataJSON()).toEqual({
      password: "test-password",
    });
    return route.fulfill({ json: principal });
  });
  await page.goto("/account");
  await page.getByRole("button", { name: "解除绑定" }).click();
  await page.getByRole("button", { name: "确认解除" }).click();
  await expect(page.getByRole("group", { name: "近期认证" })).toBeVisible();
  await page.getByLabel("当前密码").fill("test-password");
  await page.getByRole("button", { name: "用密码验证" }).click();
  await expect(page.getByText("身份已验证，请重新提交。")).toBeVisible();
  expect(attempts).toBe(1);
  expect(proof).toBe(1);
  await page.getByRole("button", { name: "确认解除" }).click();
  await expect(page.getByText(/最后一个可用登录方式/)).toBeVisible();
  expect(attempts).toBe(2);
});

test("OIDC 近期认证在弹窗中完成，原命令保持待确认", async ({ page }) => {
  let attempts = 0;
  // page.route 不覆盖新窗口；模拟同源回跳窗口重新读取已轮换的会话。
  await page
    .context()
    .route("**/api/v1/users/me", (route) => route.fulfill({ json: principal }));
  await page.route(
    "**/api/v1/users/me/external-identities/identity-1",
    (route) => {
      attempts++;
      return route.fulfill({
        status: 403,
        json: { code: "recent_authentication_required", message: "recent" },
      });
    },
  );
  await page.route("**/api/v1/auth/oidc/oidc/reauth", (route) =>
    route.fulfill({
      json: {
        authorizationUrl: new URL("/auth/callback", route.request().url()).href,
        expiresAt: timestamp,
      },
    }),
  );
  await page.goto("/account");
  await page.getByRole("button", { name: "解除绑定" }).click();
  await page.getByRole("button", { name: "确认解除" }).click();
  const popupEvent = page.waitForEvent("popup");
  await page.getByRole("button", { name: "用 企业登录 验证" }).click();
  const popup = await popupEvent;
  await popup.waitForEvent("close");
  await expect(page.getByText("身份已验证，请重新提交。")).toBeVisible();
  expect(attempts).toBe(1);
});
