import { chooseOption } from "./helpers/select";
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

for (const width of [1440, 375]) {
  test(`组件下拉菜单在触发器下方展开并保留选择与焦点（${width}px）`, async ({
    page,
  }) => {
    await page.setViewportSize({ width, height: 900 });
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
    await page.goto("/projects/p-1?view=members");
    const select = page.getByRole("combobox", { name: "查找方式" });
    const trigger = await select.boundingBox();
    expect(trigger).not.toBeNull();
    await select.click();
    const menu = page.getByRole("listbox");
    await expect(menu).toBeVisible();
    await expect
      .poll(async () => {
        const content = await menu.boundingBox();
        return trigger && content
          ? content.y - (trigger.y + trigger.height)
          : -1;
      })
      .toBeGreaterThanOrEqual(4);
    const content = await menu.boundingBox();
    expect(Math.abs((trigger?.x ?? 0) - (content?.x ?? 0))).toBeLessThanOrEqual(
      1,
    );
    await page.keyboard.press("Escape");
    await expect(select).toBeFocused();
    await chooseOption(select, "verified_email");
    await expect(select).toHaveAttribute("data-value", "verified_email");
    await select.focus();
    await expect(select).toBeFocused();

    // 高对比度模式继续使用组件交互，不依赖系统菜单或背景 SVG。
    await page.emulateMedia({ forcedColors: "active" });
    await chooseOption(select, "user_id");
    await expect(select).toHaveAttribute("data-value", "user_id");
  });
}

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
  await chooseOption(page.getByLabel(/修改 新同事 的角色/), "viewer");
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
  await chooseOption(page.getByLabel("查找方式"), "verified_email");
  await page.getByLabel("精确查找用户").fill("shared@example.com");
  await page.getByRole("button", { name: "查找" }).click();
  await expect(page.getByText(/邮箱对应多个用户/)).toBeVisible();
  await expect(page.getByRole("button", { name: "添加成员" })).toHaveCount(0);
  await page.getByRole("button", { name: "修改角色" }).click();
  await chooseOption(page.getByLabel(/修改 项目所有者 的角色/), "developer");
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

test("降低自己的项目角色后回读权限并清除已查找的候选人", async ({ page }) => {
  let role = "owner";
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role,
        allowed:
          role === "owner"
            ? ["read", "develop", "manage_members", "manage_owners"]
            : ["read", "develop"],
      },
    }),
  );
  await page.route("**/api/v1/projects/p-1/members?*", (route) =>
    route.fulfill({
      json: {
        items: [
          { ...owner, role },
          { ...owner, userId: "u-3", displayName: "另一位所有者" },
        ],
      },
    }),
  );
  await page.route("**/api/v1/projects/p-1/member-candidate:resolve", (route) =>
    route.fulfill({ json: { status: "found", candidate } }),
  );
  await page.route("**/api/v1/projects/p-1/members/u-1", (route) => {
    expect(route.request().postDataJSON()).toEqual({ role: "developer" });
    role = "developer";
    return route.fulfill({ json: { ...owner, role } });
  });
  await page.goto("/projects/p-1?view=members");
  await page.getByLabel("精确查找用户").fill("candidate");
  await page.getByRole("button", { name: "查找", exact: true }).click();
  await expect(page.getByText("新同事", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "修改角色" }).first().click();
  await chooseOption(page.getByLabel(/修改 项目所有者 的角色/), "developer");
  await page.getByRole("button", { name: "保存角色" }).click();
  await expect(
    page.locator(".workbench-heading").getByText("开发者", { exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("精确查找用户")).toHaveCount(0);
  await expect(page.getByText("新同事", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "添加成员" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "修改角色" })).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "移除", exact: true }),
  ).toHaveCount(0);
});

test("移除自己后停止展示项目管理入口并刷新可见项目列表", async ({ page }) => {
  let removed = false;
  const invisible = {
    status: 404,
    json: { code: "project_not_found", message: "project not found" },
  };
  await page.route("**/api/v1/projects?*", (route) =>
    route.fulfill({ json: { items: removed ? [] : [project] } }),
  );
  await page.route("**/api/v1/projects/p-1", (route) =>
    removed ? route.fulfill(invisible) : route.fulfill({ json: project }),
  );
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    removed
      ? route.fulfill(invisible)
      : route.fulfill({
          json: {
            projectId: "p-1",
            role: "owner",
            allowed: ["read", "manage_members", "manage_owners"],
          },
        }),
  );
  await page.route("**/api/v1/projects/p-1/members?*", (route) =>
    removed
      ? route.fulfill(invisible)
      : route.fulfill({
          json: {
            items: [
              owner,
              { ...owner, userId: "u-3", displayName: "另一位所有者" },
            ],
          },
        }),
  );
  await page.route("**/api/v1/projects/p-1/application-workbench?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.route("**/api/v1/projects/p-1/members/u-1", (route) => {
    expect(route.request().method()).toBe("DELETE");
    removed = true;
    return route.fulfill({ json: owner });
  });
  // 先缓存项目列表，再验证退出项目后不会继续使用旧可见性。
  await page.goto("/projects");
  await page.getByRole("link", { name: /^Yuuki / }).click();
  await page.getByRole("button", { name: "项目成员", exact: true }).click();
  await page.getByRole("button", { name: "移除", exact: true }).first().click();
  await page.getByRole("button", { name: "确认移除" }).click();
  await expect(
    page.getByRole("heading", { name: "无法加载项目" }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "修改角色" })).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: "项目工作台", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("link", { name: "所有项目", exact: true }).click();
  await expect(page.getByRole("heading", { name: "还没有项目" })).toBeVisible();
});
