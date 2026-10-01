import { chooseOption } from "./helpers/select";
import { randomUUID } from "node:crypto";
import { execFileSync, spawn } from "node:child_process";
import { request as httpsRequest } from "node:https";
import { expect, request as requestContext, test } from "@playwright/test";

const loginName = process.env.ORBIT_DEVOPS_E2E_LOGIN;
const password = process.env.ORBIT_DEVOPS_E2E_PASSWORD;
const tlsHostname = process.env.ORBIT_DEVOPS_E2E_TLS_HOSTNAME;
const tlsSecretName = process.env.ORBIT_DEVOPS_E2E_TLS_SECRET_NAME;
const gatewayEnabled = process.env.ORBIT_DEVOPS_E2E_GATEWAY === "1";
const issuerPolicy = process.env.ORBIT_DEVOPS_E2E_ISSUER_POLICY;
const readyImage =
  process.env.ORBIT_DEVOPS_E2E_IMAGE ??
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
  let hostPath = "";
  const hostname = tlsHostname ?? `${projectSlug}.orbit.test`;
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
    await page.getByLabel("容器端口").fill(gatewayEnabled ? "80" : "8080");
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
    await page.getByLabel("域名").fill(hostname);
    if (tlsHostname && tlsSecretName) {
      await chooseOption(page.getByLabel("TLS 模式"), "existing_secret");
      await chooseOption(page.getByLabel("TLS Secret 授权"), { index: 1 });
    } else if (issuerPolicy) {
      await chooseOption(page.getByLabel("TLS 模式"), "managed");
      await chooseOption(page.getByLabel("Issuer Policy"), issuerPolicy);
    }
    await page.getByRole("button", { name: "创建域名" }).click();
    await expect(page).toHaveURL(/\/access-hosts\/[0-9a-f-]+$/);
    hostPath = new URL(page.url()).pathname;
    await page.getByLabel("PathPrefix").fill("/e2e");
    await chooseOption(page.getByLabel("部署目标"), targetId);
    await page.getByRole("button", { name: "创建路由" }).click();
    await expect(page.getByText("/e2e", { exact: true })).toBeVisible();
  });

  await test.step("创建发布并等候 Worker 在 Kind 中应用", async () => {
    await page.goto(applicationPath);
    await page.getByRole("button", { name: "新建发布" }).click();
    await chooseOption(page.getByLabel("镜像来源"), "reference");
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

  if (gatewayEnabled) {
    await test.step("真实控制器就绪、HTTP 跳转和 HTTPS 后端访问", async () => {
      expect(!!issuerPolicy || !!tlsSecretName).toBe(true);
      await expect
        .poll(
          async () => {
            const response = await page.request.get(
              `/api/v1${hostPath}/status`,
            );
            if (!response.ok()) return null;
            const status = await response.json();
            return {
              sync: status.sync.state,
              gateway: status.controller.gatewayState,
              listener: status.controller.listenerState,
              secret: status.controller.secretState,
              certificate: status.controller.certificateState,
              routes: status.controller.routes.map(
                (route: { accepted: string; resolvedRefs: string }) => [
                  route.accepted,
                  route.resolvedRefs,
                ],
              ),
            };
          },
          { timeout: 90_000 },
        )
        .toEqual({
          sync: "applied",
          gateway: "ready",
          listener: "ready",
          secret: "ready",
          certificate:
            issuerPolicy && !tlsSecretName ? "ready" : "not_applicable",
          routes: [["ready", "ready"]],
        });

      // 只转发到显式配置的本地 Kind；不使用开发者默认 context。
      const kubeconfig = process.env.ORBIT_DEVOPS_KUBECONFIG!;
      const context = process.env.ORBIT_DEVOPS_KUBERNETES_CONTEXT!;
      expect(kubeconfig).toBeTruthy();
      expect(context).toMatch(/^kind-/);
      const kubectlArgs = ["--kubeconfig", kubeconfig, "--context", context];
      const endpoint = execFileSync(
        "kubectl",
        [
          ...kubectlArgs,
          "config",
          "view",
          "--minify",
          "-o",
          "jsonpath={.clusters[0].cluster.server}",
        ],
        { encoding: "utf8" },
      );
      expect(["127.0.0.1", "localhost", "[::1]"]).toContain(
        new URL(endpoint).hostname,
      );
      const services = JSON.parse(
        execFileSync(
          "kubectl",
          [
            ...kubectlArgs,
            "-n",
            "envoy-gateway-system",
            "get",
            "services",
            "-l",
            `gateway.envoyproxy.io/owning-gateway-name=orbit-gw-${projectPath.split("/").at(-1)}`,
            "-o",
            "json",
          ],
          { encoding: "utf8" },
        ),
      );
      expect(services.items).toHaveLength(1);
      const forward = spawn(
        "kubectl",
        [
          ...kubectlArgs,
          "-n",
          "envoy-gateway-system",
          "port-forward",
          `svc/${services.items[0].metadata.name}`,
          "18080:80",
          "18443:443",
        ],
        { stdio: "ignore" },
      );
      // 流量探测使用空 Cookie 上下文，避免把 Orbit 登录会话发给业务入口。
      const probe = await requestContext.newContext();
      try {
        await expect
          .poll(async () => {
            try {
              const response = await probe.get("http://127.0.0.1:18080/e2e", {
                headers: { Host: hostname },
                maxRedirects: 0,
              });
              return [response.status(), response.headers().location];
            } catch {
              return null;
            }
          })
          .toEqual([301, `https://${hostname}/e2e`]);
        // 本地自签名证书不受浏览器信任，但必须用真实 SNI 完成 TLS 握手。
        await expect
          .poll(
            () =>
              new Promise<number>((resolve, reject) => {
                const request = httpsRequest(
                  {
                    hostname: "127.0.0.1",
                    port: 18443,
                    path: "/e2e",
                    servername: hostname,
                    headers: { Host: hostname },
                    rejectUnauthorized: false,
                    timeout: 3000,
                  },
                  (response) => {
                    let body = "";
                    response.on("data", (chunk) => {
                      body += chunk;
                    });
                    response.on("end", () =>
                      resolve(
                        response.statusCode === 200 &&
                          body.includes(
                            `Hostname: orbit-devops-${targetId.replaceAll("-", "")}-`,
                          )
                          ? 200
                          : 0,
                      ),
                    );
                  },
                );
                request.on("error", reject);
                request.on("timeout", () =>
                  request.destroy(new Error("HTTPS timeout")),
                );
                request.end();
              }).catch(() => 0),
            { timeout: 30_000 },
          )
          .toBe(200);
      } finally {
        await probe.dispose();
        forward.kill("SIGTERM");
      }
      await page.goto(hostPath);
      await expect(
        page.getByText("Gateway / Listener", { exact: true }),
      ).toBeVisible();
    });
  }
});
