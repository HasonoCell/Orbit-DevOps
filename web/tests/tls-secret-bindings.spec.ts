import { expect, test } from "@playwright/test";
import { principal, project, timestamp } from "./fixtures/overview";

const admin = {
  ...principal,
  user: { ...principal.user!, platformRole: "platform_admin" },
};
const binding = {
  id: "binding-1",
  projectId: "p-1",
  clusterRef: "kind-local",
  namespace: "orbit-e2e",
  hostname: "payment.example.com",
  secretName: "payment-tls",
  state: "active",
  createdAt: timestamp,
  updatedAt: timestamp,
};
const host = {
  id: "host-1",
  projectId: "p-1",
  clusterRef: "kind-local",
  namespace: "orbit-e2e",
  hostname: binding.hostname,
  tlsMode: "existing_secret",
  secretBindingId: binding.id,
  secretBindingState: "active",
  lifecycle: "active",
  createdAt: timestamp,
  updatedAt: timestamp,
};
const status = {
  host,
  sync: { desiredRevision: 2, appliedRevision: 1, state: "pending" },
  controller: {
    gatewayState: "ready",
    listenerState: "unknown",
    certificateState: "not_applicable",
    secretState: "unknown",
    routes: [],
    addresses: [],
    observedAt: timestamp,
  },
  dns: { state: "not_configured", answers: [], observedAt: timestamp },
};

test("平台管理员登记并撤销已有 TLS Secret 授权", async ({ page }) => {
  let registered = false;
  let revoked = false;
  await page.route("**/api/v1/users/me", (route) =>
    route.fulfill({ json: admin }),
  );
  await page.route("**/api/v1/platform/access-secret-bindings?*", (route) =>
    route.fulfill({
      json: registered
        ? [{ ...binding, state: revoked ? "revoked" : "active" }]
        : [],
    }),
  );
  await page.route("**/api/v1/platform/access-secret-bindings", (route) => {
    expect(route.request().postDataJSON()).toEqual({
      projectId: "p-1",
      hostname: binding.hostname,
      secretName: binding.secretName,
    });
    expect(route.request().headers()["idempotency-key"]).toBeTruthy();
    registered = true;
    return route.fulfill({ status: 201, json: binding });
  });
  await page.route(
    "**/api/v1/platform/access-secret-bindings/binding-1",
    (route) => {
      expect(route.request().headers()["idempotency-key"]).toBeTruthy();
      revoked = true;
      return route.fulfill({ json: { ...binding, state: "revoked" } });
    },
  );
  await page.goto("/platform?view=secrets");
  await page.getByLabel("Project ID").fill("p-1");
  await page.getByLabel("域名").fill(binding.hostname);
  await page.getByLabel("Secret 名称").fill(binding.secretName);
  await page.getByRole("button", { name: "登记授权" }).click();
  await expect(page.getByText(/已登记 payment.example.com/)).toBeVisible();
  await page.getByRole("button", { name: "撤销", exact: true }).click();
  expect(revoked).toBe(false);
  await page.getByRole("button", { name: "确认撤销" }).click();
  await expect(page.getByText(/已撤销 payment.example.com/)).toBeVisible();
  expect(revoked).toBe(true);
});

test("项目成员只选择当前域名的有效授权创建 Host", async ({ page }) => {
  let created = false;
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
        allowed: ["read", "manage_access_hosts"],
      },
    }),
  );
  await page.route("**/api/v1/projects/p-1/access-host-options", (route) =>
    route.fulfill({
      json: {
        clusterRef: "kind-local",
        namespace: "orbit-e2e",
        issuerPolicies: [],
      },
    }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-secret-binding-options?*",
    (route) => {
      expect(new URL(route.request().url()).searchParams.get("hostname")).toBe(
        binding.hostname,
      );
      return route.fulfill({
        json: [
          {
            id: binding.id,
            secretName: binding.secretName,
            clusterRef: binding.clusterRef,
            namespace: binding.namespace,
            hostname: binding.hostname,
          },
        ],
      });
    },
  );
  await page.route("**/api/v1/projects/p-1/access-hosts", (route) => {
    expect(route.request().postDataJSON()).toEqual({
      hostname: binding.hostname,
      tlsMode: "existing_secret",
      secretBindingId: binding.id,
    });
    created = true;
    return route.fulfill({ status: 201, json: host });
  });
  await page.route("**/api/v1/projects/p-1/access-hosts/host-1", (route) =>
    route.fulfill({ json: host }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/host-1/status",
    (route) => route.fulfill({ json: status }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/host-1/routes?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.goto("/projects/p-1/access-hosts/new");
  await page.getByLabel("域名").fill(binding.hostname);
  await page.getByLabel("TLS 模式").selectOption("existing_secret");
  await page.getByLabel("TLS Secret 授权").selectOption(binding.id);
  await page.getByRole("button", { name: "创建域名" }).click();
  await expect(page).toHaveURL(/\/access-hosts\/host-1$/);
  expect(created).toBe(true);
});

test("被撤销的绑定在 Host 详情标记失效且不能再次选择", async ({ page }) => {
  const revokedHost = { ...host, secretBindingState: "revoked" };
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
        allowed: ["read", "manage_access_hosts"],
      },
    }),
  );
  await page.route("**/api/v1/projects/p-1/access-hosts/host-1", (route) =>
    route.fulfill({ json: revokedHost }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/host-1/status",
    (route) => route.fulfill({ json: { ...status, host: revokedHost } }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-hosts/host-1/routes?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.route("**/api/v1/projects/p-1/access-host-options", (route) =>
    route.fulfill({
      json: {
        clusterRef: "kind-local",
        namespace: "orbit-e2e",
        issuerPolicies: [],
      },
    }),
  );
  await page.route(
    "**/api/v1/projects/p-1/access-secret-binding-options?*",
    (route) => route.fulfill({ json: [] }),
  );
  await page.goto("/projects/p-1/access-hosts/host-1");
  await expect(page.getByText(/当前 TLS Secret 授权已撤销/)).toBeVisible();
  await expect(
    page.getByRole("button", { name: "保存 TLS 配置" }),
  ).toBeDisabled();
});
