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
  await page.getByLabel("部署目标").selectOption("t-1");
  await page.getByRole("button", { name: "创建路由" }).click();
  await expect(page.getByText("/pay", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "删除", exact: true }).click();
  await page.getByRole("button", { name: "确认删除路由" }).click();
  await expect(page.getByText(/清理已接纳，等待控制器完成/)).toBeVisible();
  await page.getByRole("button", { name: "删除域名" }).click();
  await page.getByRole("button", { name: "确认删除", exact: true }).click();
  await expect(page.getByText(/清理已接纳。请继续观察/)).toBeVisible();
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
  await page.getByLabel("TLS 模式").selectOption("managed");
  await expect(page.getByRole("button", { name: "创建域名" })).toBeDisabled();
  await page.getByLabel("Issuer Policy").selectOption("demo-issuer");
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
  await expect(
    page.locator("#access-tls-mode option[value='managed']"),
  ).toBeDisabled();
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
