import { randomUUID } from "node:crypto";
import { expect, test } from "@playwright/test";

const loginName = process.env.ORBIT_DEVOPS_E2E_LOGIN;
const password = process.env.ORBIT_DEVOPS_E2E_PASSWORD;
const tlsHostname = process.env.ORBIT_DEVOPS_E2E_TLS_HOSTNAME;
const tlsSecretName = process.env.ORBIT_DEVOPS_E2E_TLS_SECRET_NAME;
const readyImage =
  "registry.k8s.io/pause@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a";

test.skip(!loginName || !password, "需要显式提供本地验收管理员凭据");

test("浏览器经真实 API 管理资源并发布到本地 Kind", async ({ page }) => {
  const seed = randomUUID().slice(0, 8);
  const memberLogin = `e2e-member-${seed}`;
  const projectSlug = `e2e-${seed}`;
  const memberPassword = `Orbit-E2E!${randomUUID()}`;

  await test.step("登录并创建本地用户", async () => {
    await page.goto("/login");
    await page.getByLabel("登录名").fill(loginName!);
    await page.getByLabel("密码", { exact: true }).fill(password!);
    await page.getByRole("button", { name: "登录", exact: true }).click();
    await expect(page).toHaveURL(/\/projects(?:\?.*)?$/);
    await page.goto("/platform?view=users");
    await page.getByLabel("登录名").fill(memberLogin);
    await page.getByLabel("显示名称").fill("E2E 项目成员");
    await page.getByLabel("临时密码", { exact: true }).fill(memberPassword);
    await page.getByRole("button", { name: "创建用户" }).click();
    await expect(page.getByText(/已创建 E2E 项目成员/)).toBeVisible();
  });

  let projectPath = "";
  let applicationPath = "";
  let targetId = "";
  await test.step("创建项目、应用和部署目标", async () => {
    await page.goto("/projects");
    await page.getByRole("button", { name: "创建项目" }).first().click();
    await page.getByLabel("项目名称").fill(`E2E ${seed}`);
    await page.getByLabel("项目标识").fill(projectSlug);
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "创建项目" })
      .click();
    await expect(page).toHaveURL(/\/projects\/[0-9a-f-]+$/);
    projectPath = new URL(page.url()).pathname;

    await page.getByRole("button", { name: "创建应用" }).first().click();
    await page.getByLabel("应用名称").fill("E2E Service");
    await page.getByLabel("应用标识").fill("e2e-service");
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "创建应用" })
      .click();
    await expect(page).toHaveURL(/\/applications\/[0-9a-f-]+$/);
    applicationPath = new URL(page.url()).pathname;

    await page.getByRole("button", { name: "创建开发目标" }).click();
    await page.getByLabel("期望副本").fill("1");
    await page.getByLabel("容器端口").fill("8080");
    await page.getByRole("button", { name: "创建部署目标" }).click();
    await expect(page).toHaveURL(/\/targets\/[0-9a-f-]+$/);
    targetId = new URL(page.url()).pathname.split("/").at(-1)!;
    await page.getByLabel("期望副本").fill("2");
    await page.getByRole("button", { name: "保存配置" }).click();
    await expect(
      page.getByText("配置已保存，将在下一次发布中应用"),
    ).toBeVisible();
  });

  await test.step("项目成员的精确查找与授权", async () => {
    await page.goto(`${projectPath}?view=members`);
    await page.getByLabel("精确查找用户").fill(memberLogin);
    await page.getByRole("button", { name: "查找" }).click();
    await expect(page.getByText("E2E 项目成员")).toBeVisible();
    await page.getByRole("button", { name: "添加成员" }).click();
    await expect(page.getByText("成员已添加。")).toBeVisible();
  });

  await test.step("创建访问域名和基础路由", async () => {
    if (tlsHostname && tlsSecretName) {
      await page.goto("/platform?view=secrets");
      await page.getByLabel("Project ID").fill(projectPath.split("/").at(-1)!);
      await page.getByLabel("域名").fill(tlsHostname);
      await page.getByLabel("Secret 名称").fill(tlsSecretName);
      await page.getByRole("button", { name: "登记授权" }).click();
      await expect(
        page.getByText(new RegExp(`已登记 ${tlsHostname}`)),
      ).toBeVisible();
    }
    await page.goto(`${projectPath}?view=access`);
    await page.getByRole("link", { name: "添加域名" }).click();
    await page
      .getByLabel("域名")
      .fill(tlsHostname ?? `${projectSlug}.orbit.test`);
    if (tlsHostname && tlsSecretName) {
      await page.getByLabel("TLS 模式").selectOption("existing_secret");
      await page.getByLabel("TLS Secret 授权").selectOption({ index: 1 });
    }
    await page.getByRole("button", { name: "创建域名" }).click();
    await expect(page).toHaveURL(/\/access-hosts\/[0-9a-f-]+$/);
    await page.getByLabel("PathPrefix").fill("/e2e");
    await page.getByLabel("部署目标").selectOption(targetId);
    await page.getByRole("button", { name: "创建路由" }).click();
    await expect(page.getByText("/e2e", { exact: true })).toBeVisible();
  });

  await test.step("创建发布并等候 Worker 在 Kind 中应用", async () => {
    await page.goto(applicationPath);
    await page.getByRole("button", { name: "新建发布" }).click();
    await page.getByLabel("镜像来源").selectOption("reference");
    await page.getByLabel("镜像引用", { exact: true }).fill(readyImage);
    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "确认发布到开发环境" }).click();
    await expect(page).toHaveURL(/\/releases\/[0-9a-f-]+$/);
    const releaseId = new URL(page.url()).pathname.split("/").at(-1)!;
    await expect
      .poll(
        async () => {
          const response = await page.request.get(
            `/api/v1/releases/${releaseId}`,
          );
          if (!response.ok()) return `HTTP ${response.status()}`;
          const detail = await response.json();
          return detail.releaseOperation.status as string;
        },
        { timeout: 90_000 },
      )
      .toBe("succeeded");
    await page.getByRole("button", { name: "刷新状态" }).click();
    await expect(
      page.getByText("执行成功", { exact: true }).first(),
    ).toBeVisible({ timeout: 15_000 });
  });
});
