import { expect, test } from "@playwright/test";
import { principal, timestamp } from "./fixtures/overview";

const admin = {
  ...principal,
  user: { ...principal.user!, platformRole: "platform_admin" },
};
const initial = {
  id: "identity-1",
  providerId: "enterprise",
  status: "pending",
  displayName: "申请人",
  email: "person@example.com",
  emailVerified: true,
  createdAt: timestamp,
  updatedAt: timestamp,
};

test("管理员批准准入后详情显示正式 User 关联，列表转入已关联", async ({
  page,
}) => {
  let record = initial;
  let approvals = 0;
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: admin }),
  );
  await page.route("**/api/v1/auth/admissions?*", (route) => {
    const status = new URL(route.request().url()).searchParams.get("status");
    return route.fulfill({
      json: { items: status === record.status ? [record] : [] },
    });
  });
  await page.route("**/api/v1/auth/admissions/identity-1", (route) =>
    route.fulfill({ json: record }),
  );
  await page.route("**/api/v1/auth/admissions/identity-1/approve", (route) => {
    approvals++;
    record = { ...record, status: "linked", userId: "u-2" } as typeof record;
    return route.fulfill({
      json: {
        id: "u-2",
        displayName: "申请人",
        status: "active",
        platformRole: "user",
        createdAt: timestamp,
      },
    });
  });
  await page.goto("/platform?view=admissions");
  await expect(page.getByText("申请人").first()).toBeVisible();
  await page.getByRole("button", { name: "详情" }).click();
  await expect(page).toHaveURL(/identityId=identity-1/);
  await page.getByRole("button", { name: "批准", exact: true }).click();
  await page.getByRole("button", { name: "确认提交" }).click();
  await expect(page.getByText(/正式用户 ID：u-2/)).toBeVisible();
  await expect(page.getByText(/关联 User ID：u-2/)).toBeVisible();
  await page.getByRole("button", { name: "已关联" }).click();
  await expect(page.getByText("申请人")).toBeVisible();
  expect(approvals).toBe(1);
});

test("拒绝与重新打开按服务端状态回读，冲突不误报成功", async ({ page }) => {
  let record = initial;
  let conflict = true;
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: admin }),
  );
  await page.route("**/api/v1/auth/admissions?*", (route) =>
    route.fulfill({ json: { items: [record] } }),
  );
  await page.route("**/api/v1/auth/admissions/identity-1", (route) =>
    route.fulfill({ json: record }),
  );
  await page.route("**/api/v1/auth/admissions/identity-1/reject", (route) => {
    if (conflict) {
      conflict = false;
      return route.fulfill({
        status: 409,
        json: { code: "admission_state_conflict", message: "changed" },
      });
    }
    record = { ...record, status: "rejected" };
    return route.fulfill({ json: record });
  });
  await page.route("**/api/v1/auth/admissions/identity-1/reopen", (route) => {
    record = { ...record, status: "pending" };
    return route.fulfill({ json: record });
  });
  await page.goto("/platform?view=admissions");
  await page.getByRole("button", { name: "详情" }).click();
  await page.getByRole("button", { name: "拒绝", exact: true }).click();
  await page.getByRole("button", { name: "确认提交" }).click();
  await expect(page.getByText(/准入状态已变化/)).toBeVisible();
  await page.getByRole("button", { name: "确认提交" }).click();
  await expect(page.getByRole("button", { name: "重新打开" })).toBeVisible();
  await page.getByRole("button", { name: "重新打开" }).click();
  await page.getByRole("button", { name: "确认提交" }).click();
  await expect(
    page.getByRole("button", { name: "批准", exact: true }),
  ).toBeVisible();
});

test("近期认证后批准不会自动重放", async ({ page }) => {
  let approvals = 0;
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
  await page.route("**/api/v1/auth/admissions?*", (route) =>
    route.fulfill({ json: { items: [initial] } }),
  );
  await page.route("**/api/v1/auth/admissions/identity-1", (route) =>
    route.fulfill({ json: initial }),
  );
  await page.route("**/api/v1/auth/admissions/identity-1/approve", (route) => {
    approvals++;
    return route.fulfill({
      status: 403,
      json: { code: "recent_authentication_required", message: "recent" },
    });
  });
  await page.route("**/api/v1/auth/reauth/local", (route) =>
    route.fulfill({ json: admin }),
  );
  await page.goto("/platform?view=admissions");
  await page.getByRole("button", { name: "详情" }).click();
  await page.getByRole("button", { name: "批准", exact: true }).click();
  await page.getByRole("button", { name: "确认提交" }).click();
  await page.getByLabel("当前密码").fill("fixture-only-recent-proof");
  await page.getByRole("button", { name: "用密码验证" }).click();
  await expect(page.getByText(/身份已验证，请检查原操作/)).toBeVisible();
  expect(approvals).toBe(1);
});

test("浏览器返回状态列表时不沿用另一状态的分页游标", async ({ page }) => {
  const requests: string[] = [];
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: admin }),
  );
  await page.route("**/api/v1/auth/admissions?*", (route) => {
    const url = new URL(route.request().url());
    const status = url.searchParams.get("status");
    const cursor = url.searchParams.get("cursor");
    requests.push(`${status}:${cursor ?? "first"}`);
    if (status === "pending" && cursor === "pending-next") {
      return route.fulfill({
        json: {
          items: [{ ...initial, id: "identity-2", displayName: "第二页申请" }],
        },
      });
    }
    if (status === "pending" && !cursor) {
      return route.fulfill({
        json: { items: [initial], nextCursor: "pending-next" },
      });
    }
    if (status === "rejected" && !cursor)
      return route.fulfill({ json: { items: [] } });
    return route.fulfill({
      status: 400,
      json: { code: "invalid_cursor", message: "游标与状态不匹配" },
    });
  });
  await page.goto("/platform?view=admissions&admissionStatus=pending");
  await page.getByRole("button", { name: "下一页" }).click();
  await expect(page.getByText("第二页申请")).toBeVisible();
  await page.getByRole("button", { name: "已拒绝" }).click();
  await expect(page.getByText("此状态下暂无记录")).toBeVisible();
  await page.goBack();
  await expect(page.getByText("申请人").first()).toBeVisible();
  await expect(page.getByText("第 1 页")).toBeVisible();
  expect(requests).not.toContain("rejected:pending-next");
  expect(requests.at(-1)).toBe("pending:first");
});

test("申请人的旧待准入会话失效后提示重新登录核验结果", async ({ page }) => {
  let reads = 0;
  await page.route("**/api/v1/users/me", (route) => {
    reads++;
    return route.fulfill(
      reads === 1
        ? {
            json: {
              kind: "pending",
              externalIdentity: initial,
              mustChangePassword: false,
            },
          }
        : {
            status: 401,
            json: {
              code: "authentication_required",
              message: "session expired",
            },
          },
    );
  });
  await page.route("**/api/v1/auth/providers", (route) =>
    route.fulfill({ json: [] }),
  );
  await page.goto("/admission");
  await page.getByRole("button", { name: "检查状态" }).click();
  await expect(page.getByText(/待准入会话已结束/)).toBeVisible();
  await page.getByRole("button", { name: "前往登录" }).click();
  await expect(page).toHaveURL(/\/login$/);
});
