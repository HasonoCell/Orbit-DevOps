import { expect, test } from "@playwright/test";
import { principal, project, timestamp } from "./fixtures/overview";

const owner = {
  projectId: "p-1",
  userId: "u-1",
  displayName: "项目所有者",
  role: "owner",
  createdBy: "u-1",
  createdAt: timestamp,
  updatedAt: timestamp,
};
const candidate = { userId: "u-2", displayName: "新同事" };

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: principal }),
  );
  await page.route("**/api/v1/projects/p-1", (route) =>
    route.fulfill({ json: project }),
  );
});

test("项目 owner 精确查找用户、添加成员并调整角色和移除", async ({ page }) => {
  let members = [owner];
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "owner",
        allowed: ["read", "manage_members", "manage_owners"],
      },
    }),
  );
  await page.route(
    "**/api/v1/projects/p-1/member-candidate:resolve",
    (route) => {
      expect(route.request().postDataJSON()).toEqual({
        kind: "login_name",
        value: "candidate",
      });
      return route.fulfill({ json: { status: "found", candidate } });
    },
  );
  await page.route("**/api/v1/projects/p-1/members**", (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (pathname.endsWith("/members")) {
      if (request.method() === "GET")
        return route.fulfill({ json: { items: members } });
      expect(request.headers()["idempotency-key"]).toBeTruthy();
      expect(request.postDataJSON()).toEqual({
        userId: "u-2",
        role: "developer",
      });
      members = [...members, { ...owner, ...candidate, role: "developer" }];
      return route.fulfill({ status: 201, json: members[1] });
    }
    expect(request.headers()["idempotency-key"]).toBeTruthy();
    if (request.method() === "PUT") {
      expect(request.postDataJSON()).toEqual({ role: "viewer" });
      members = [owner, { ...members[1], role: "viewer" }];
      return route.fulfill({ json: members[1] });
    }
    members = [owner];
    return route.fulfill({ json: { ...owner, ...candidate, role: "viewer" } });
  });
  await page.goto("/projects/p-1?view=members");
  await expect(page.getByText("项目所有者")).toBeVisible();
  await page.getByLabel("精确查找用户").fill("candidate");
  await page.getByRole("button", { name: "查找" }).click();
  await expect(page.getByText("新同事")).toBeVisible();
  await page.getByRole("button", { name: "添加成员" }).click();
  await expect(page.getByText("成员已添加。")).toBeVisible();
  await page.getByRole("button", { name: "修改角色" }).last().click();
  await page.getByLabel(/修改 新同事 的角色/).selectOption("viewer");
  await page.getByRole("button", { name: "保存角色" }).click();
  await expect(page.getByText("成员角色已更新。")).toBeVisible();
  await page.getByRole("button", { name: "移除" }).last().click();
  await page.getByRole("button", { name: "确认移除" }).click();
  await expect(page.getByText("成员已移除。")).toBeVisible();
  await expect(page.getByText("新同事")).toHaveCount(0);
});

test("邮箱歧义不暴露候选人，最后 owner 冲突有明确反馈", async ({ page }) => {
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "owner",
        allowed: ["read", "manage_members", "manage_owners"],
      },
    }),
  );
  await page.route("**/api/v1/projects/p-1/members?*", (route) =>
    route.fulfill({ json: { items: [owner] } }),
  );
  await page.route("**/api/v1/projects/p-1/member-candidate:resolve", (route) =>
    route.fulfill({ json: { status: "ambiguous" } }),
  );
  await page.route("**/api/v1/projects/p-1/members/u-1", (route) =>
    route.fulfill({
      status: 409,
      json: { code: "last_project_owner", message: "last owner" },
    }),
  );
  await page.goto("/projects/p-1?view=members");
  await page.getByLabel("查找方式").selectOption("verified_email");
  await page.getByLabel("精确查找用户").fill("shared@example.com");
  await page.getByRole("button", { name: "查找" }).click();
  await expect(page.getByText(/邮箱对应多个用户/)).toBeVisible();
  await expect(page.getByRole("button", { name: "添加成员" })).toHaveCount(0);
  await page.getByRole("button", { name: "修改角色" }).click();
  await page.getByLabel(/修改 项目所有者 的角色/).selectOption("developer");
  await page.getByRole("button", { name: "保存角色" }).click();
  await expect(page.getByText(/最后一位有效所有者/)).toBeVisible();
});

test("观察者仅能读取成员，不出现用户查找或管理操作", async ({ page }) => {
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: { projectId: "p-1", role: "viewer", allowed: ["read"] },
    }),
  );
  await page.route("**/api/v1/projects/p-1/members?*", (route) =>
    route.fulfill({ json: { items: [owner] } }),
  );
  await page.goto("/projects/p-1?view=members");
  await expect(page.getByText("项目所有者")).toBeVisible();
  await expect(page.getByLabel("精确查找用户")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "修改角色" })).toHaveCount(0);
});
