import { chooseOption } from "./helpers/select";
import { createHmac, randomUUID } from "node:crypto";
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
  page.setDefaultTimeout(15_000);
  // 即使不验收 Gateway，也必须从本地集群独立核验 Deployment/Service，不能只信 API succeeded。
  const kubeconfig = process.env.ORBIT_DEVOPS_KUBECONFIG;
  const context = process.env.ORBIT_DEVOPS_KUBERNETES_CONTEXT;
  const namespace = process.env.ORBIT_DEVOPS_NAMESPACE;
  if (!kubeconfig || !context?.startsWith("kind-") || !namespace) {
    throw new Error(
      "真实 E2E 需要显式指定本地 Kind kubeconfig、context 和任务 namespace",
    );
  }
  const kubectlArgs = ["--kubeconfig", kubeconfig, "--context", context];
  const kubectl = (...args: string[]) =>
    JSON.parse(
      execFileSync("kubectl", [...kubectlArgs, ...args], {
        encoding: "utf8",
        timeout: 10_000,
      }),
    );
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
    { encoding: "utf8", timeout: 10_000 },
  );
  expect(["127.0.0.1", "localhost", "[::1]"]).toContain(
    new URL(endpoint).hostname,
  );
  const nodes = kubectl("get", "nodes", "-o", "json");
  expect(nodes.items.length).toBeGreaterThan(0);
  for (const node of nodes.items)
    expect(node.metadata.name.startsWith(`${context.slice(5)}-`)).toBe(true);
  const managedNamespace = kubectl("get", "namespace", namespace, "-o", "json");
  expect(managedNamespace.metadata.labels["app.kubernetes.io/managed-by"]).toBe(
    "orbit-devops",
  );

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
    await page
      .getByLabel("容器端口")
      .fill(
        process.env.ORBIT_DEVOPS_E2E_SOURCE_BUILD === "1"
          ? "8080"
          : gatewayEnabled
            ? "80"
            : "8080",
      );
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

  if (process.env.ORBIT_DEVOPS_E2E_SOURCE_BUILD === "1") {
    await test.step("浏览器创建 Pipeline，签名 Push 经真实 BuildKit 交付并晋级生产", async () => {
      const secret = process.env.ORBIT_DEVOPS_E2E_WEBHOOK_SECRET;
      const buildNamespace = process.env.ORBIT_DEVOPS_BUILD_NAMESPACE;
      if (!secret || !buildNamespace)
        throw new Error("源码验收需要显式 Webhook 密钥和独立 Build namespace");
      await page.goto(applicationPath + "/pipelines/new");
      await page.getByLabel("名称", { exact: true }).fill("Source E2E");
      await page.getByLabel("Endpoint Key").fill("refactor-e2e");
      await page
        .getByLabel("GitHub Clone URL")
        .fill("https://github.com/nginxinc/NGINX-Demos.git");
      await page.getByLabel("分支", { exact: true }).fill("master");
      await page
        .getByLabel("Dockerfile 路径")
        .fill("nginx-hello-nonroot/plain-text-version/Dockerfile");
      await page
        .getByLabel("构建上下文")
        .fill("nginx-hello-nonroot/plain-text-version");
      await chooseOption(page.getByLabel("交付模式"), "auto_release");
      await chooseOption(page.getByLabel("开发 Target"), targetId);
      await page.getByRole("button", { name: "创建 Pipeline" }).click();
      await expect(page).toHaveURL(/\/pipelines\/[0-9a-f-]+$/);
      const pipelineId = new URL(page.url()).pathname.split("/").at(-1)!;
      await page
        .getByRole("button", { name: "启用 Pipeline", exact: true })
        .click();
      await page.getByRole("button", { name: "确认", exact: true }).click();
      await expect(
        page.getByText("Orbit 已启用", { exact: true }),
      ).toBeVisible();
      const configResponse = await page.request.get(
        `/api/v1/delivery-pipelines/${pipelineId}`,
      );
      expect(configResponse.ok()).toBe(true);
      const config = await configResponse.json();
      const sourceResponse = await page.request.get(
        "https://api.github.com/repos/nginxinc/NGINX-Demos/commits/master",
      );
      expect(sourceResponse.ok()).toBe(true);
      const source = await sourceResponse.json();
      const payload = JSON.stringify({
        ref: "refs/heads/master",
        before: "0".repeat(40),
        after: source.sha,
        forced: false,
        deleted: false,
        repository: {
          id: config.revision.repositoryId,
          full_name: config.revision.repositoryFullName,
          clone_url: config.revision.repositoryUrl,
          owner: { id: config.revision.repositoryOwnerId },
        },
      });
      const delivery = await page.request.post(
        "/api/v1/webhooks/github/refactor-e2e",
        {
          data: payload,
          headers: {
            "Content-Type": "application/json",
            "X-GitHub-Event": "push",
            "X-GitHub-Delivery": randomUUID(),
            "X-Hub-Signature-256":
              "sha256=" +
              createHmac("sha256", secret).update(payload).digest("hex"),
          },
        },
      );
      expect(delivery.status()).toBe(202);
      expect((await delivery.json()).state).toBe("pending");
      let completed: { buildId: string; releaseId: string } | undefined;
      await expect
        .poll(
          async () => {
            const response = await page.request.get(
              `/api/v1/delivery-pipelines/${pipelineId}/runs?limit=1`,
            );
            expect(response.ok()).toBe(true);
            const run = (await response.json()).items[0];
            if (run?.status === "succeeded") completed = run.run;
            return run?.status;
          },
          { timeout: 480_000, intervals: [1000, 3000, 5000] },
        )
        .toBe("succeeded");
      if (!completed) throw new Error("交付未产生 Build/Release");
      const buildResponse = await page.request.get(
        "/api/v1/builds/" + completed.buildId,
      );
      expect(buildResponse.ok()).toBe(true);
      const acceptedBuild = await buildResponse.json();
      expect(acceptedBuild.buildOperation.status).toBe("succeeded");
      expect(acceptedBuild.imageArtifact.digest).toMatch(
        /^sha256:[a-f0-9]{64}$/,
      );
      const jobs = kubectl(
        "-n",
        buildNamespace,
        "get",
        "jobs",
        "-l",
        "orbit-devops.dev/build-id=" + completed.buildId,
        "-o",
        "json",
      );
      expect(jobs.items).toHaveLength(1);
      expect(jobs.items[0].status.succeeded).toBe(1);
      const name = "orbit-devops-" + targetId.replaceAll("-", "");
      const deployed = kubectl(
        "-n",
        namespace,
        "get",
        "deployment",
        name,
        "-o",
        "json",
      );
      expect(deployed.spec.template.spec.containers[0].image).toBe(
        acceptedBuild.imageArtifact.imageReference,
      );
      expect(deployed.status.readyReplicas).toBe(2);
      await page.goto(applicationPath + "/builds/" + completed.buildId);
      await expect(
        page.getByText(acceptedBuild.imageArtifact.digest, { exact: true }),
      ).toBeVisible();
      await page.goto(applicationPath);
      await page.getByRole("button", { name: "创建生产目标" }).click();
      await page.getByLabel("期望副本").fill("1");
      await page.getByLabel("容器端口").fill("8080");
      await page.getByRole("button", { name: "创建部署目标" }).click();
      await expect(page).toHaveURL(/\/targets\/[0-9a-f-]+$/);
      const productionId = new URL(page.url()).pathname.split("/").at(-1)!;
      await page.goto(
        applicationPath +
          "?target=" +
          productionId +
          "&buildSource=" +
          completed.buildId +
          "&createRelease=1",
      );
      await page.getByRole("checkbox").check();
      await page.getByRole("button", { name: "确认发布到生产环境" }).click();
      await expect(page).toHaveURL(/\/releases\/[0-9a-f-]+$/);
      const productionReleaseId = new URL(page.url()).pathname
        .split("/")
        .at(-1)!;
      await expect
        .poll(
          async () => {
            const response = await page.request.get(
              "/api/v1/releases/" + productionReleaseId,
            );
            return (await response.json()).releaseOperation.status;
          },
          { timeout: 90_000 },
        )
        .toBe("succeeded");
      const production = kubectl(
        "-n",
        namespace,
        "get",
        "deployment",
        "orbit-devops-" + productionId.replaceAll("-", ""),
        "-o",
        "json",
      );
      expect(production.spec.template.spec.containers[0].image).toBe(
        acceptedBuild.imageArtifact.imageReference,
      );
      expect(production.status.readyReplicas).toBe(1);
      await page.getByRole("button", { name: "刷新状态", exact: true }).click();
      await expect(
        page.locator("#operation").getByText("执行成功", { exact: true }),
      ).toBeVisible();
      // 后续 Gateway 断言使用 whoami:80；这里只修订开发 Target，不改写上述不可变 Release。
      await page.goto(applicationPath + "/targets/" + targetId);
      await page.getByLabel("容器端口").fill("80");
      await page.getByRole("button", { name: "保存配置" }).click();
      await expect(
        page.getByText("配置已保存，将在下一次发布中应用"),
      ).toBeVisible();
    });
  }

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
    // 双阶段验收已经创建 production；手动发布必须显式选 development，不依赖默认目标。
    await page.goto(applicationPath + "?target=" + targetId);
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
    const resourceName = `orbit-devops-${targetId.replaceAll("-", "")}`;
    const deployment = kubectl(
      "-n",
      namespace,
      "get",
      "deployment",
      resourceName,
      "-o",
      "json",
    );
    const service = kubectl(
      "-n",
      namespace,
      "get",
      "service",
      resourceName,
      "-o",
      "json",
    );
    for (const resource of [deployment, service]) {
      expect(resource.metadata.uid).toBeTruthy();
      expect(resource.metadata.labels["orbit-devops.dev/release-id"]).toBe(
        releaseId,
      );
      expect(resource.metadata.labels["orbit-devops.dev/target-id"]).toBe(
        targetId,
      );
      expect(resource.metadata.labels["app.kubernetes.io/managed-by"]).toBe(
        "orbit-devops",
      );
    }
    expect(deployment.spec.template.spec.containers[0].image).toBe(readyImage);
    expect(deployment.status.observedGeneration).toBe(
      deployment.metadata.generation,
    );
    expect(deployment.status.readyReplicas).toBe(2);
    expect(service.spec.selector["orbit-devops.dev/target-id"]).toBe(targetId);
    expect(service.spec.ports).toContainEqual(
      expect.objectContaining({ port: 80, targetPort: "http" }),
    );
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

      // 复用进入业务流程前已核验的本地连接；不使用开发者默认 context。
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
