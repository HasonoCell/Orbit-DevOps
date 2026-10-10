import { chooseOption } from "../../tests/helpers/select";
import { expect, test, type Page } from "@playwright/test";
import { demoPassword } from "../state.ts";

const api = "http://127.0.0.1:18091";
const app = "/projects/p-1/applications/a-1";
async function login(page: Page, loginName = "demo") {
  await page.goto("/login");
  await page.getByLabel("登录名").fill(loginName);
  await page.getByLabel("密码", { exact: true }).fill(demoPassword);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page).toHaveURL(/\/projects$/);
}
async function scenario(page: Page, values: Record<string, string>) {
  const result = await page.request.post(api + "/__demo/scenario", {
    headers: { "X-Orbit-CSRF": "1" },
    data: values,
  });
  expect(result.ok()).toBe(true);
}
test.beforeEach(async ({ request }) => {
  const response = await request.post(api + "/__demo/reset", {
    headers: { "X-Orbit-CSRF": "1" },
    data: {},
  });
  expect(response.ok()).toBe(true);
});

test("真实页面完成项目创建、构建、产物发布、运行日志与回滚", async ({
  page,
}) => {
  await login(page);
  await page.getByRole("button", { name: "创建项目" }).first().click();
  await page.getByLabel("项目名称").fill("体验项目");
  await page.getByLabel("项目标识").fill("experience");
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "创建项目" })
    .click();
  await expect(page).toHaveURL(/\/projects\/[a-f\d-]+$/);
  await page.getByRole("button", { name: "创建应用" }).first().click();
  await page.getByLabel("应用名称").fill("体验服务");
  await page.getByLabel("应用标识").fill("experience-service");
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "创建应用" })
    .click();
  await expect(page).toHaveURL(/\/applications\/[a-f\d-]+$/);
  const applicationPath = new URL(page.url()).pathname;
  await page.getByRole("button", { name: "创建开发目标" }).click();
  await page.getByRole("button", { name: "创建部署目标" }).click();
  await expect(page).toHaveURL(/\/targets\/[a-f\d-]+$/);
  await page.goto(applicationPath + "?view=delivery");
  await page.getByRole("button", { name: "手动构建" }).click();
  await page
    .getByLabel("Git Clone URL")
    .fill("https://github.com/HasonoCell/Yuuki");
  await page.getByLabel("Commit SHA").fill("d".repeat(40));
  await page.getByRole("button", { name: "创建构建" }).click();
  await expect(page).toHaveURL(/\/builds\/[a-f\d-]+$/);
  const buildId = new URL(page.url()).pathname.split("/").at(-1)!;
  await expect
    .poll(
      async () =>
        (await (await page.request.get(`/api/v1/builds/${buildId}`)).json())
          .buildOperation.status,
    )
    .toBe("succeeded");
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(
    page.getByRole("link", { name: "使用此产物发布" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "读取日志摘录" }).first().click();
  await expect(page.getByText(/exporting OCI image/)).toBeVisible();
  await page.getByRole("link", { name: "使用此产物发布" }).click();
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "确认发布到开发环境" }).click();
  await expect(page).toHaveURL(/\/releases\/[a-f\d-]+$/);
  const releaseId = new URL(page.url()).pathname.split("/").at(-1)!;
  await expect
    .poll(
      async () =>
        (await (await page.request.get(`/api/v1/releases/${releaseId}`)).json())
          .releaseOperation.status,
    )
    .toBe("succeeded");
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(
    page.getByText("执行成功", { exact: true }).first(),
  ).toBeVisible();
  await page
    .getByRole("button", { name: /^读取 .* \/ app 日志$/ })
    .first()
    .click();
  await expect(page.getByText(/GET \/healthz 200/)).toBeVisible();
  await page.getByRole("button", { name: "回滚到此发布" }).click();
  await page.getByRole("button", { name: "确认回滚到此发布" }).click();
  await expect(page).not.toHaveURL(new RegExp(`/releases/${releaseId}$`));
  await expect(page.getByText("回滚来源")).toBeVisible();
});

test("场景面板触发自动交付，入口与证书状态变化回到真实页面", async ({
  page,
}) => {
  await login(page);
  await page.goto(api + "/__demo");
  await page.getByRole("button", { name: "触发自动交付" }).click();
  const runLink = page.getByRole("link", { name: "查看交付运行" });
  await expect(runLink).toBeVisible();
  const href = await runLink.getAttribute("href");
  await page.goto(href!);
  const runId = new URL(page.url()).pathname.split("/").at(-1)!;
  await expect
    .poll(
      async () =>
        (
          await (
            await page.request.get(`/api/v1/delivery-runs/${runId}`)
          ).json()
        ).status,
    )
    .toBe("succeeded");
  await page.getByRole("button", { name: "刷新运行" }).click();
  await expect(page.getByText("交付成功", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: /^查看 Build / })).toBeVisible();
  await page.goto("/projects/p-1/access-hosts/h-1");
  await expect(page.getByText("已验证", { exact: true })).toBeVisible();
  await chooseOption(
    page.getByLabel("模式", { exact: true }),
    "existing_secret",
  );
  await chooseOption(page.getByLabel("TLS Secret 授权"), "binding-1");
  await page.getByRole("button", { name: "保存 TLS 配置" }).click();
  await expect(page.getByText("已保存，等待控制器更新。")).toBeVisible();
  await scenario(page, { evidence: "dns_mismatch" });
  await page.getByRole("button", { name: "刷新入口状态" }).click();
  await expect
    .poll(
      async () =>
        (
          await (
            await page.request.get(
              "/api/v1/projects/p-1/access-hosts/h-1/status",
            )
          ).json()
        ).dns.state,
    )
    .toBe("mismatch");
  await page.getByRole("button", { name: "刷新入口状态" }).click();
  await expect(page.getByText("不匹配", { exact: true })).toBeVisible();
  await page.getByLabel("PathPrefix").fill("/order");
  await chooseOption(page.getByLabel("部署目标"), "a-4-production");
  await page.getByRole("button", { name: "创建路由" }).click();
  await expect(page.getByText("/order", { exact: true })).toBeVisible();
});

test("平台管理、近期认证和 OIDC 绑定使用实际 Mock HTTP 跳转", async ({
  page,
}) => {
  await login(page);
  await page.goto("/platform?view=users");
  await page.getByLabel("登录名").fill("colleague");
  await page.getByLabel("显示名称").fill("体验同事");
  await page
    .getByLabel("临时密码", { exact: true })
    .fill("Temporary-Demo-2026!");
  await page.getByRole("button", { name: "创建用户" }).click();
  await expect(page.getByText(/已创建 体验同事/)).toBeVisible();
  await page.goto("/account");
  await expect(page.getByText("暂无外部身份")).toBeVisible();
  await page.getByRole("button", { name: "绑定 演示 OIDC" }).click();
  await expect(page).toHaveURL(/\/__demo\/oidc\?flow=/);
  await page.getByRole("button", { name: "绑定身份" }).click();
  await expect(page).toHaveURL(/\/account$/);
  await expect(
    page.getByText("演示管理员 OIDC", { exact: false }),
  ).toBeVisible();
  await page.request.post(api + "/__demo/expire", {
    headers: { "X-Orbit-CSRF": "1" },
    data: {},
  });
  await page.getByRole("button", { name: "解除绑定" }).click();
  await page.getByRole("button", { name: "确认解除" }).click();
  await expect(page.getByRole("group", { name: "近期认证" })).toBeVisible();
  await page.getByLabel("当前密码").fill(demoPassword);
  await page.getByRole("button", { name: "用密码验证" }).click();
  await expect(page.getByText("身份已验证，请重新提交。")).toBeVisible();
  await page.getByRole("button", { name: "确认解除" }).click();
  await expect(page).toHaveURL(/\/login$/);
  await login(page);
  await page.goto("/account");
  await expect(page.getByText("暂无外部身份")).toBeVisible();
});

test("开发者与只读账号看到受限操作，失败构建可从页面重试", async ({ page }) => {
  await login(page, "viewer");
  await page.goto(app + "?view=delivery");
  await expect(page.getByRole("button", { name: "手动构建" })).toHaveCount(0);
  await page.goto("/login");
  // 登录页面会自动跳过已有会话，所以通过公开退出接口切换身份。
  await page.request.post("/api/v1/auth/logout", {
    headers: { "X-Orbit-CSRF": "1" },
    data: {},
  });
  await login(page, "developer");
  await scenario(page, { nextBuild: "failed" });
  await page.goto(app + "?view=delivery");
  await page.getByRole("button", { name: "手动构建" }).click();
  await page
    .getByLabel("Git Clone URL")
    .fill("https://github.com/HasonoCell/Yuuki");
  await page.getByLabel("Commit SHA").fill("e".repeat(40));
  await page.getByRole("button", { name: "创建构建" }).click();
  await expect(page).toHaveURL(/\/builds\/[a-f\d-]+$/);
  const buildId = new URL(page.url()).pathname.split("/").at(-1)!;
  await expect
    .poll(
      async () =>
        (await (await page.request.get(`/api/v1/builds/${buildId}`)).json())
          .buildOperation.status,
    )
    .toBe("failed");
  await page.getByRole("button", { name: "刷新状态" }).click();
  await page.getByRole("button", { name: "重试构建" }).click();
  await page.getByRole("button", { name: "确认重试构建" }).click();
  await expect
    .poll(
      async () =>
        (await (await page.request.get(`/api/v1/builds/${buildId}`)).json())
          .buildOperation.status,
    )
    .toBe("succeeded");
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(
    page.getByRole("link", { name: "使用此产物发布" }),
  ).toBeVisible();
});
