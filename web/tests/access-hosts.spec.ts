import { chooseOption } from "./helpers/select";
import { expect, test } from "@playwright/test";
import {
  application,
  principal,
  project,
  target,
  timestamp,
} from "./fixtures/overview";

const host = {
  id: "h-1",
  projectId: "p-1",
  clusterRef: "demo-cluster",
  namespace: "yuuki",
  hostname: "payment.example.com",
  tlsMode: "http_only",
  lifecycle: "active",
  createdAt: timestamp,
  updatedAt: timestamp,
};
const status = {
  host,
  sync: { desiredRevision: 1, appliedRevision: 0, state: "pending" },
  controller: {
    gatewayState: "ready",
    listenerState: "ready",
    routes: [],
    certificateState: "not_applicable",
    secretState: "not_applicable",
    addresses: ["203.0.113.1"],
    observedAt: timestamp,
  },
  dns: { state: "mismatch", answers: ["203.0.113.2"], observedAt: timestamp },
};
const routeRecord = {
  id: "ar-1",
  hostId: "h-1",
  deploymentTargetId: "t-1",
  pathPrefix: "/pay",
  lifecycle: "active",
  createdAt: timestamp,
  updatedAt: timestamp,
};

test.beforeEach(async ({ page }) => {
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: principal }),
  );
  await page.route("**/api/v1/projects/p-1", (route) =>
    route.fulfill({ json: project }),
  );
  await page.route("**/api/v1/projects/p-1/permissions", (route) =>
    route.fulfill({
      json: {
        projectId: "p-1",
        role: "owner",
        allowed: [
          "read",
          "develop",
          "manage_access_hosts",
          "manage_access_routes",
        ],
      },
    }),
  );
  await page.route("**/api/v1/applications/a-1", (route) =>
    route.fulfill({ json: application }),
  );
  await page.route("**/api/v1/deployment-targets/t-1", (route) =>
    route.fulfill({ json: target }),
  );
  await page.route("**/api/v1/projects/p-1/access-host-options", (route) =>
    route.fulfill({
      json: {
        clusterRef: host.clusterRef,
        namespace: host.namespace,
        issuerPolicies: [
          { key: "demo-issuer", kind: "ClusterIssuer", name: "demo" },
        ],
      },
    }),
  );
});

test("项目创建 Host、选择同边界 Target 建 Route，并分开呈现入口和 DNS 状态", async ({
  page,
}) => {
  let currentHost = host;
  let hosts: (typeof host)[] = [];
  let routes: (typeof routeRecord)[] = [];
  await page.route("**/api/v1/projects/p-1/access-hosts?*", (route) =>
    route.fulfill({ json: hosts }),
  );
  await page.route("**/api/v1/projects/p-1/access-hosts", (route) => {
    expect(route.request().postDataJSON()).toEqual({
      hostname: host.hostname,
      tlsMode: "http_only",
    });
    hosts = [host];
    return route.fulfill({ status: 201, json: host });
  });
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1", (route) => {
    if (route.request().method() === "GET")
      return route.fulfill({ json: currentHost });
    currentHost = { ...host, lifecycle: "deleting" };
    return route.fulfill({ status: 202, json: currentHost });
  });
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1/status", (route) =>
    route.fulfill({ json: status }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/eligible-targets?*",
    (route) =>
      route.fulfill({
        json: [
          {
            id: "t-1",
            applicationId: "a-1",
            applicationName: "Payment Service",
            stage: "production",
            clusterRef: host.clusterRef,
            namespace: host.namespace,
          },
        ],
      }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/routes?*",
    (route) => route.fulfill({ json: routes }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/routes",
    (route) => {
      expect(route.request().postDataJSON()).toEqual({
        pathPrefix: "/pay",
        deploymentTargetId: "t-1",
      });
      routes = [routeRecord];
      return route.fulfill({ status: 201, json: routeRecord });
    },
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/routes/ar-1",
    (route) => {
      routes = [{ ...routeRecord, lifecycle: "deleting" }];
      return route.fulfill({ status: 202, json: routes[0] });
    },
  );
  await page.goto("/projects/p-1?view=access");
  await expect(page.getByText("暂无访问域名")).toBeVisible();
  await page.getByRole("link", { name: "添加域名" }).click();
  await page.getByLabel("域名").fill(host.hostname);
  await page.getByRole("button", { name: "创建域名" }).click();
  await expect(page).toHaveURL(/\/access-hosts\/h-1$/);
  await expect(page.getByText("待调和", { exact: false })).toBeVisible();
  await expect(page.getByText("不匹配")).toBeVisible();
  await expect(page.getByText(/未主动验证公网 HTTP\/HTTPS/)).toBeVisible();
  await page.getByLabel("PathPrefix").fill("/pay");
  await chooseOption(page.getByLabel("部署目标"), "t-1");
  await page.getByRole("button", { name: "创建路由" }).click();
  await expect(page.getByText("/pay", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "删除", exact: true }).click();
  await page.getByRole("button", { name: "确认删除路由" }).click();
  await expect(page.getByText(/清理已接纳，等待控制器完成/)).toBeVisible();
  await page.getByRole("button", { name: "删除域名" }).click();
  await page.getByRole("button", { name: "确认删除", exact: true }).click();
  await expect(page.getByText(/清理已接纳。请继续观察/)).toBeVisible();
});

test("托管 TLS 长页面中的 Target 下拉可见且可点击", async ({ page }) => {
  test.setTimeout(15_000);
  await page.setViewportSize({ width: 1280, height: 720 });
  const managed = {
    ...host,
    tlsMode: "managed",
    issuerPolicyKey: "demo-issuer",
  };
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1", (route) =>
    route.fulfill({ json: managed }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/status",
    async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 1000));
      return route.fulfill({
        json: {
          ...status,
          host: managed,
          controller: { ...status.controller, certificateNotAfter: timestamp },
          dns: { ...status.dns, errorCode: "dns_lookup_failed" },
        },
      });
    },
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/routes?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/eligible-targets?*",
    (route) =>
      route.fulfill({
        json: ["development", "production"].map((stage, n) => ({
          id: `target-${n}`,
          applicationId: "a-1",
          applicationName: "E2E Service",
          stage,
          clusterRef: host.clusterRef,
          namespace: host.namespace,
        })),
      }),
  );
  await page.goto("/projects/p-1/access-hosts/h-1");
  await page.getByLabel("PathPrefix").fill("/e2e");
  await page.getByLabel("部署目标").click();
  const menu = page.getByRole("listbox");
  await expect(menu.getByRole("option")).toHaveCount(3);
  await expect(
    page.getByText("dns_lookup_failed", { exact: true }),
  ).toBeVisible();
  await expect(menu).toBeInViewport({ ratio: 1 });
  await menu
    .locator('[data-slot="select-item"][data-value="target-0"]')
    .click();
  await expect(menu).toHaveCount(0);
  await expect(page.getByLabel("部署目标")).toHaveAttribute(
    "data-value",
    "target-0",
  );
});

test("Host 列表按 offset 分页，不扫描项目资源", async ({ page }) => {
  await page.route("**/api/v1/projects/p-1/access-hosts?*", (route) => {
    const offset = new URL(route.request().url()).searchParams.get("offset");
    return route.fulfill({
      json:
        offset === "20"
          ? [host]
          : Array.from({ length: 21 }, (_, n) => ({
              ...host,
              id: `h-${n}`,
              hostname: `host-${n}.example.com`,
            })),
    });
  });
  await page.goto("/projects/p-1?view=access");
  await page.getByRole("button", { name: "下一页" }).click();
  await expect(page).toHaveURL(/hostOffset=20/);
  await expect(page.getByRole("link", { name: host.hostname })).toBeVisible();
});

test("调和已应用后仍轮询 Gateway 与 DNS 观测，五分钟后暂停", async ({
  page,
}) => {
  await page.clock.install();
  let reads = 0;
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1", (route) =>
    route.fulfill({ json: host }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/status",
    (route) => {
      reads++;
      return route.fulfill({
        json:
          reads === 1
            ? {
                ...status,
                sync: { ...status.sync, appliedRevision: 1, state: "applied" },
                controller: { ...status.controller, gatewayState: "not_ready" },
              }
            : {
                ...status,
                sync: { ...status.sync, appliedRevision: 1, state: "applied" },
                dns: { ...status.dns, state: "verified" },
              },
      });
    },
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/routes?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/eligible-targets?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.goto("/projects/p-1/access-hosts/h-1");
  await expect(page.getByText("未就绪 / 就绪")).toBeVisible();
  await page.clock.fastForward(16_000);
  await expect(page.getByText("已验证")).toBeVisible();
  await page.clock.fastForward(5 * 60_000);
  const stoppedAt = reads;
  await page.clock.fastForward(16_000);
  expect(reads).toBe(stoppedAt);
});

for (const action of [
  "TLS",
  "创建路由",
  "修改路由",
  "删除路由",
  "删除域名",
] as const) {
  test(`五分钟后${action}，重新开启有界入口观测`, async ({ page }) => {
    await page.clock.install();
    let currentHost = { ...host, issuerPolicyKey: "" };
    let routes = action === "创建路由" ? [] : [routeRecord];
    let accepted = false;
    let ready = false;
    let reads = 0;
    await page.route("**/api/v1/projects/p-1/access-hosts/h-1", (route) => {
      const method = route.request().method();
      if (method !== "GET") {
        accepted = true;
        currentHost = {
          ...currentHost,
          ...(method === "PATCH"
            ? { tlsMode: "managed", issuerPolicyKey: "demo-issuer" }
            : { lifecycle: "deleting" }),
          updatedAt: "2026-09-30T10:00:00Z",
        };
      }
      return route.fulfill({ json: currentHost });
    });
    await page.route(
      "**/api/v1/projects/p-1/access-hosts/h-1/status",
      (route) => {
        reads++;
        return route.fulfill({
          json: {
            ...status,
            host: currentHost,
            sync: {
              desiredRevision: accepted ? 2 : 1,
              appliedRevision: ready ? 2 : 0,
              state: ready ? "applied" : "pending",
            },
            controller: {
              ...status.controller,
              gatewayState: ready ? "ready" : "not_ready",
              certificateState: ready ? "ready" : "not_ready",
              secretState: ready ? "ready" : "not_ready",
            },
          },
        });
      },
    );
    await page.route(
      "**/api/v1/projects/p-1/access-hosts/h-1/routes?*",
      (route) => route.fulfill({ json: routes }),
    );
    await page.route(
      "**/api/v1/projects/p-1/access-hosts/h-1/eligible-targets?*",
      (route) =>
        route.fulfill({
          json: [{ ...target, applicationName: application.name }],
        }),
    );
    await page.route(
      "**/api/v1/projects/p-1/access-hosts/h-1/routes",
      (route) => {
        accepted = true;
        routes = [routeRecord];
        return route.fulfill({ status: 201, json: routeRecord });
      },
    );
    await page.route(
      "**/api/v1/projects/p-1/access-hosts/h-1/routes/ar-1",
      (route) => {
        accepted = true;
        const updated =
          route.request().method() === "DELETE"
            ? { ...routeRecord, lifecycle: "deleting" }
            : { ...routeRecord, pathPrefix: "/pay-next" };
        routes = [updated];
        return route.fulfill({ json: updated });
      },
    );
    await page.goto("/projects/p-1/access-hosts/h-1");
    await expect(page.getByText("待调和 · 修订 0/1")).toBeVisible();
    await page.clock.fastForward(301_000);
    if (action === "TLS") {
      await chooseOption(page.getByLabel("模式", { exact: true }), "managed");
      await chooseOption(page.getByLabel("Issuer Policy"), "demo-issuer");
      await page.getByRole("button", { name: "保存 TLS 配置" }).click();
      await expect(page.getByText(/配置已保存/)).toBeVisible();
    } else if (action === "创建路由" || action === "修改路由") {
      if (action === "修改路由")
        await page.getByRole("button", { name: "修改", exact: true }).click();
      await page
        .getByLabel("PathPrefix")
        .fill(action === "创建路由" ? "/pay" : "/pay-next");
      await chooseOption(page.getByLabel("部署目标"), "t-1");
      await page
        .getByRole("button", {
          name: action === "创建路由" ? "创建路由" : "保存路由",
        })
        .click();
      await expect(
        page.getByText(action === "创建路由" ? "/pay" : "/pay-next", {
          exact: true,
        }),
      ).toBeVisible();
    } else if (action === "删除路由") {
      await page.getByRole("button", { name: "删除", exact: true }).click();
      await page.getByRole("button", { name: "确认删除路由" }).click();
      await expect(page.getByText(/清理已接纳，等待控制器完成/)).toBeVisible();
    } else {
      await page.getByRole("button", { name: "删除域名" }).click();
      await page.getByRole("button", { name: "确认删除", exact: true }).click();
      await expect(page.getByText(/清理已接纳。请继续观察/)).toBeVisible();
    }
    await expect(page.getByText("待调和 · 修订 0/2")).toBeVisible();
    await expect(
      page.getByRole("button", { name: "刷新入口状态" }),
    ).toBeEnabled();
    expect(accepted).toBe(true);
    const acceptedReads = reads;
    ready = true;
    await page.clock.fastForward(16_000);
    await expect(page.getByText("已应用 · 修订 2/2")).toBeVisible();
    expect(reads).toBeGreaterThan(acceptedReads);
    if (action === "TLS") {
      const certificate = page
        .getByRole("term")
        .filter({ hasText: "证书 / Secret" });
      await expect(certificate.locator("..")).toContainText("就绪 / 就绪");
    }
    await page.clock.fastForward(301_000);
    await expect(
      page.getByRole("button", { name: "刷新入口状态" }),
    ).toBeEnabled();
    const stoppedAt = reads;
    await page.clock.fastForward(16_000);
    expect(reads).toBe(stoppedAt);
  });
}

test("路由目标翻页后保留已选 Target", async ({ page }) => {
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1", (route) =>
    route.fulfill({ json: host }),
  );
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1/status", (route) =>
    route.fulfill({ json: status }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/routes?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/eligible-targets?*",
    (route) => {
      const offset = new URL(route.request().url()).searchParams.get("offset");
      const item = (index: number) => ({
        id: `t-${index}`,
        applicationId: "a-1",
        applicationName: "Payment Service",
        stage: "development",
        clusterRef: host.clusterRef,
        namespace: host.namespace,
      });
      return route.fulfill({
        json:
          offset === "20"
            ? [item(21)]
            : Array.from({ length: 21 }, (_, n) => item(n + 1)),
      });
    },
  );
  await page.goto("/projects/p-1/access-hosts/h-1");
  await chooseOption(page.getByLabel("部署目标"), "t-1");
  await page.getByRole("button", { name: "下一组 Target" }).click();
  await expect(page.getByLabel("部署目标")).toHaveAttribute(
    "data-value",
    "t-1",
  );
  await page.getByLabel("部署目标").click();
  await expect(
    page.getByRole("option", { name: "已选 Target t-1" }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await chooseOption(page.getByLabel("部署目标"), "t-21");
  await page.getByRole("button", { name: "上一组 Target" }).click();
  await expect(page.getByLabel("部署目标")).toHaveAttribute(
    "data-value",
    "t-21",
  );
  await page.getByLabel("部署目标").click();
  await expect(
    page.getByRole("option", { name: "已选 Target t-21" }),
  ).toBeVisible();
});

test("Target 只读取受限关联投影并可跳转到 Host", async ({ page }) => {
  await page.route(
    "**/api/v1/deployment-targets/t-1/access-routes?*",
    (route) =>
      route.fulfill({
        json: [
          {
            routeId: "ar-1",
            hostId: "h-1",
            hostname: host.hostname,
            pathPrefix: "/pay",
            routeLifecycle: "active",
            hostLifecycle: "active",
          },
        ],
      }),
  );
  await page.goto("/projects/p-1/applications/a-1/targets/t-1");
  await expect(
    page.getByRole("link", { name: "payment.example.com/pay" }),
  ).toHaveAttribute("href", "/projects/p-1/access-hosts/h-1");
});

test("创建托管 TLS Host 时只能选服务端返回的 Policy", async ({ page }) => {
  let submitted = false;
  const managed = {
    ...host,
    tlsMode: "managed",
    issuerPolicyKey: "demo-issuer",
  };
  await page.route("**/api/v1/projects/p-1/access-hosts", (route) => {
    submitted = true;
    expect(route.request().postDataJSON()).toEqual({
      hostname: host.hostname,
      tlsMode: "managed",
      issuerPolicyKey: "demo-issuer",
    });
    return route.fulfill({ status: 201, json: managed });
  });
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1", (route) =>
    route.fulfill({ json: managed }),
  );
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1/status", (route) =>
    route.fulfill({
      json: {
        ...status,
        host: managed,
        controller: {
          ...status.controller,
          certificateState: "not_ready",
          secretState: "unknown",
        },
      },
    }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/routes?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/eligible-targets?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.goto("/projects/p-1/access-hosts/new");
  await page.getByLabel("域名").fill(host.hostname);
  await chooseOption(page.getByLabel("TLS 模式"), "managed");
  await expect(page.getByRole("button", { name: "创建域名" })).toBeDisabled();
  await chooseOption(page.getByLabel("Issuer Policy"), "demo-issuer");
  await page.getByRole("button", { name: "创建域名" }).click();
  await expect(page).toHaveURL(/\/access-hosts\/h-1$/);
  await expect(
    page.getByRole("definition").filter({ hasText: "托管证书" }),
  ).toBeVisible();
  await expect(page.getByText("不匹配")).toBeVisible();
  expect(submitted).toBe(true);
});

test("无 Policy 时禁用托管模式，已有 Policy 失效时拒绝继续提交", async ({
  page,
}) => {
  await page.route("**/api/v1/projects/p-1/access-host-options", (route) =>
    route.fulfill({
      json: {
        clusterRef: host.clusterRef,
        namespace: host.namespace,
        issuerPolicies: [],
      },
    }),
  );
  await page.goto("/projects/p-1/access-hosts/new");
  await page.getByLabel("TLS 模式").click();
  await expect(page.getByRole("option", { name: "托管证书" })).toBeDisabled();
  await page.keyboard.press("Escape");
  await expect(page.getByText(/当前没有可用的托管证书 Policy/)).toBeVisible();

  const managed = {
    ...host,
    tlsMode: "managed",
    issuerPolicyKey: "removed-issuer",
  };
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1", (route) =>
    route.fulfill({ json: managed }),
  );
  await page.route("**/api/v1/projects/p-1/access-hosts/h-1/status", (route) =>
    route.fulfill({ json: { ...status, host: managed } }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/routes?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/h-1/eligible-targets?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.goto("/projects/p-1/access-hosts/h-1");
  await expect(
    page.getByText(/当前引用的 Issuer Policy 已不可用/),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "保存 TLS 配置" }),
  ).toBeDisabled();
});
