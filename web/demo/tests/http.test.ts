import assert from "node:assert/strict";
import { test, type TestContext } from "node:test";
import { createDemoServer } from "../server.ts";
import { demoPassword, type Schema } from "../state.ts";

async function environment(t: TestContext) {
  const server = createDemoServer({ stepMs: 30 });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(
    () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  );
  const base = `http://127.0.0.1:${(server.address() as { port: number }).port}`;
  let cookie = "";
  async function request<T = Record<string, unknown>>(
    path: string,
    method = "GET",
    body?: unknown,
    key?: string,
  ) {
    const response = await fetch(base + path, {
      method,
      headers: {
        "X-Orbit-CSRF": "1",
        ...(body ? { "Content-Type": "application/json" } : {}),
        ...(cookie ? { Cookie: cookie } : {}),
        ...(key ? { "Idempotency-Key": key } : {}),
      },
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    const setCookie = response.headers.get("set-cookie");
    if (setCookie) cookie = setCookie.split(";")[0];
    return {
      status: response.status,
      data:
        response.status === 204 ? undefined : ((await response.json()) as T),
    };
  }
  async function login(loginName = "demo", password = demoPassword) {
    const result = await request("/api/v1/auth/login", "POST", {
      loginName,
      password,
    });
    assert.equal(result.status, 200);
  }
  await login();
  return { request, login, base };
}
async function until<T>(
  read: () => Promise<T>,
  accepted: (value: T) => boolean,
) {
  const deadline = Date.now() + 3000;
  while (Date.now() < deadline) {
    const value = await read();
    if (accepted(value)) return value;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  throw new Error("Mock state did not advance");
}
const source = {
  repositoryUrl: "https://github.com/HasonoCell/Yuuki",
  sourceCommit: "b".repeat(40),
  dockerfilePath: "Dockerfile",
  contextPath: ".",
};
const image = {
  imageReference: "registry.example.test/payment@sha256:" + "c".repeat(64),
};

test("密码创建、重置和修改采用 12 字符下限", async (t) => {
  const { request, login } = await environment(t);
  const command = {
    loginName: "policy-fixture",
    displayName: "密码策略测试",
    temporaryPassword: "Fixture!123",
  };
  assert.equal((await request("/api/v1/users", "POST", command)).status, 400);
  const created = await request<Schema["User"]>("/api/v1/users", "POST", {
    ...command,
    temporaryPassword: "Fixture!1234",
  });
  assert.equal(created.status, 201);
  const resetPath = `/api/v1/users/${created.data!.id}/password/reset`;
  assert.equal(
    (await request(resetPath, "POST", { temporaryPassword: "Reset!12345" }))
      .status,
    400,
  );
  assert.equal(
    (await request(resetPath, "POST", { temporaryPassword: "Reset!123456" }))
      .status,
    204,
  );
  await login(command.loginName, "Reset!123456");
  const change = {
    currentPassword: "Reset!123456",
    newPassword: "Change!1234",
  };
  assert.equal(
    (await request("/api/v1/users/me/password", "PUT", change)).status,
    400,
  );
  assert.equal(
    (
      await request("/api/v1/users/me/password", "PUT", {
        ...change,
        newPassword: "Change!12345",
      })
    ).status,
    204,
  );
  await login(command.loginName, "Change!12345");
});

test("资源写入、分页、幂等冲突和角色限制通过同一 HTTP 状态库呈现", async (t) => {
  const { request, login } = await environment(t);
  const projects = await request<Schema["ProjectPage"]>(
    "/api/v1/projects?limit=1",
  );
  assert.equal(projects.data!.items.length, 1);
  assert.equal(projects.data!.nextCursor, "1");
  const command = { name: "Mock Project", slug: "mock-project" };
  const first = await request<Schema["Project"]>(
    "/api/v1/projects",
    "POST",
    command,
    "same-key",
  );
  const second = await request<Schema["Project"]>(
    "/api/v1/projects",
    "POST",
    command,
    "same-key",
  );
  assert.equal(first.status, 201);
  assert.equal(first.data!.id, second.data!.id);
  assert.equal(
    (
      await request(
        "/api/v1/projects",
        "POST",
        { ...command, slug: "changed" },
        "same-key",
      )
    ).status,
    409,
  );
  assert.equal(
    (
      await request(
        "/api/v1/projects/p-1/members/u-1",
        "DELETE",
        {},
        "last-owner",
      )
    ).status,
    409,
  );
  await login("viewer");
  assert.equal(
    (await request("/api/v1/projects/p-1/applications")).status,
    200,
  );
  assert.equal(
    (
      await request(
        "/api/v1/applications/a-1/builds",
        "POST",
        source,
        "no-write",
      )
    ).status,
    403,
  );
  assert.equal((await request("/api/v1/users")).status, 403);
});

test("构建产生 Digest，产物发布更新运行观测，历史快照与回滚保持连贯", async (t) => {
  const { request } = await environment(t);
  const created = (
    await request<Schema["BuildAcceptance"]>(
      "/api/v1/applications/a-1/builds",
      "POST",
      source,
      "build",
    )
  ).data!;
  const built = await until(
    async () =>
      (
        await request<Schema["BuildAcceptance"]>(
          `/api/v1/builds/${created.build.id}`,
        )
      ).data!,
    (b) => b.buildOperation.status === "succeeded",
  );
  assert.match(built.imageArtifact!.digest, /^sha256:[a-f\d]{64}$/);
  assert.equal(built.buildOperation.attempts.length, 1);
  const logs = await request<Schema["BuildAttemptLog"]>(
    `/api/v1/build-attempts/${built.buildOperation.attempts[0].id}/log`,
  );
  assert.match(logs.data!.excerpt, /exporting OCI image/);
  const release = (
    await request<Schema["ReleaseAcceptance"]>(
      "/api/v1/deployment-targets/t-1/releases",
      "POST",
      {
        imageArtifactId: built.imageArtifact!.id,
        imageReference: built.imageArtifact!.imageReference,
      },
      "release",
    )
  ).data!;
  await until(
    async () =>
      (
        await request<Schema["ReleaseOperation"]>(
          `/api/v1/release-operations/${release.releaseOperation.id}`,
        )
      ).data!,
    (op) => op.status === "succeeded",
  );
  const report = (
    await request<Schema["ReleaseDiagnosticReport"]>(
      `/api/v1/releases/${release.release.id}/diagnostics`,
    )
  ).data!;
  assert.equal(report.runtimeReleaseRelation, "matches");
  assert.equal(report.workloadObservation.pods.length, 2);
  await request(
    "/api/v1/deployment-targets/t-1",
    "PUT",
    { replicas: 3, containerPort: 9090 },
    "update-target",
  );
  const detail = (
    await request<Schema["ReleaseDetail"]>(
      `/api/v1/releases/${release.release.id}`,
    )
  ).data!;
  assert.equal(detail.release.targetSnapshot.replicas, 2);
  assert.equal(detail.snapshotDifferences.length, 2);
  const rollback = (
    await request<Schema["ReleaseAcceptance"]>(
      `/api/v1/releases/${release.release.id}/rollback`,
      "POST",
      {},
      "rollback",
    )
  ).data!;
  assert.notEqual(rollback.release.id, release.release.id);
  assert.equal(rollback.release.rollbackOfReleaseId, release.release.id);
  assert.equal(rollback.release.targetSnapshot.replicas, 2);
});

test("失败重试、结果未知对账和取消均改变操作记录而非固定成功响应", async (t) => {
  const { request } = await environment(t);
  await request("/__demo/scenario", "POST", {
    nextBuild: "failed",
    nextRelease: "unknown",
  });
  const built = (
    await request<Schema["BuildAcceptance"]>(
      "/api/v1/applications/a-1/builds",
      "POST",
      source,
      "failed-build",
    )
  ).data!;
  const buildPath = `/api/v1/build-operations/${built.buildOperation.id}`;
  await until(
    async () => (await request<Schema["BuildOperation"]>(buildPath)).data!,
    (op) => op.status === "failed",
  );
  assert.equal(
    (await request(buildPath + "/retry", "POST", {}, "retry")).status,
    202,
  );
  const retried = await until(
    async () => (await request<Schema["BuildOperation"]>(buildPath)).data!,
    (op) => op.status === "succeeded",
  );
  assert.deepEqual(
    retried.attempts.map((a) => a.status),
    ["failed", "succeeded"],
  );
  const release = (
    await request<Schema["ReleaseAcceptance"]>(
      "/api/v1/deployment-targets/t-1/releases",
      "POST",
      image,
      "unknown-release",
    )
  ).data!;
  const releasePath = `/api/v1/release-operations/${release.releaseOperation.id}`;
  await until(
    async () => (await request<Schema["ReleaseOperation"]>(releasePath)).data!,
    (op) => op.status === "attention_required",
  );
  assert.equal(
    (
      await request<Schema["ReleaseOperation"]>(
        releasePath + "/reconcile",
        "POST",
        {},
        "reconcile",
      )
    ).data!.status,
    "succeeded",
  );
  await request("/__demo/scenario", "POST", { nextBuild: "hold" });
  const held = (
    await request<Schema["BuildAcceptance"]>(
      "/api/v1/applications/a-1/builds",
      "POST",
      source,
      "hold",
    )
  ).data!;
  const heldPath = `/api/v1/build-operations/${held.buildOperation.id}`;
  await until(
    async () => (await request<Schema["BuildOperation"]>(heldPath)).data!,
    (op) => op.status === "running",
  );
  await request(heldPath + "/cancel", "POST", {}, "cancel");
  const canceled = await until(
    async () => (await request<Schema["BuildOperation"]>(heldPath)).data!,
    (op) => op.status === "canceled",
  );
  assert.equal(canceled.attempts[0].status, "canceled");
});

test("模拟 Push 串起 DeliveryRun、Build、Artifact 与开发 Release", async (t) => {
  const { request } = await environment(t);
  const pushed = (
    await request<{ run: Schema["DeliveryRunDetail"] }>(
      "/__demo/push",
      "POST",
      { pipelineId: "pl-1" },
    )
  ).data!;
  const completed = await until(
    async () =>
      (
        await request<Schema["DeliveryRunDetail"]>(
          `/api/v1/delivery-runs/${pushed.run.run.id}`,
        )
      ).data!,
    (r) => r.status === "succeeded",
  );
  assert.equal(completed.run.pipelineRevision, 1);
  assert.ok(completed.run.imageArtifactId);
  assert.ok(completed.run.releaseId);
  const release = (
    await request<Schema["ReleaseDetail"]>(
      `/api/v1/releases/${completed.run.releaseId}`,
    )
  ).data!;
  assert.equal(release.release.deploymentTargetId, "a-1-development");
  assert.equal(release.release.imageArtifactId, completed.run.imageArtifactId);
  await request(
    "/api/v1/delivery-pipelines/pl-1/disable",
    "POST",
    {},
    "disable",
  );
  assert.equal(
    (await request("/__demo/push", "POST", { pipelineId: "pl-1" })).status,
    409,
  );
});

test("TLS 与 DNS 独立观测，Secret 授权撤销和路由清理可以回读", async (t) => {
  const { request } = await environment(t);
  await request(
    "/api/v1/projects/p-1/access-hosts/h-1",
    "PATCH",
    {
      hostname: "yuuki.example.test",
      tlsMode: "existing_secret",
      secretBindingId: "binding-1",
    },
    "tls",
  );
  await until(
    async () =>
      (
        await request<Schema["AccessHostStatus"]>(
          "/api/v1/projects/p-1/access-hosts/h-1/status",
        )
      ).data!,
    (value) => value.controller.secretState === "ready",
  );
  await request("/__demo/scenario", "POST", { evidence: "dns_mismatch" });
  const mismatch = (
    await request<Schema["AccessHostStatus"]>(
      "/api/v1/projects/p-1/access-hosts/h-1/status",
    )
  ).data!;
  assert.equal(mismatch.controller.gatewayState, "ready");
  assert.equal(mismatch.dns.state, "mismatch");
  await request(
    "/api/v1/platform/access-secret-bindings/binding-1",
    "DELETE",
    {},
    "revoke",
  );
  const revoked = (
    await request<Schema["AccessHostStatus"]>(
      "/api/v1/projects/p-1/access-hosts/h-1/status",
    )
  ).data!;
  assert.equal(revoked.host.secretBindingState, "revoked");
  assert.equal(revoked.controller.secretState, "not_ready");
  await request(
    "/api/v1/projects/p-1/access-hosts/h-1/routes/route-1",
    "DELETE",
    {},
    "delete-route",
  );
  await until(
    async () =>
      (
        await request<Schema["AccessRoute"][]>(
          "/api/v1/projects/p-1/access-hosts/h-1/routes",
        )
      ).data!,
    (items) => items.length === 0,
  );
});

test("临时密码、近期认证和最后管理员保护完整影响账号操作", async (t) => {
  const { request, login } = await environment(t);
  assert.equal(
    (await request("/api/v1/users/u-1/disable", "POST", {})).status,
    409,
  );
  await request("/__demo/expire", "POST", {});
  assert.equal(
    (
      await request("/api/v1/users", "POST", {
        loginName: "new",
        displayName: "新同事",
        temporaryPassword: demoPassword,
      })
    ).status,
    403,
  );
  await request("/api/v1/auth/reauth/local", "POST", {
    password: demoPassword,
  });
  const created = (
    await request<Schema["User"]>("/api/v1/users", "POST", {
      loginName: "new",
      displayName: "新同事",
      temporaryPassword: demoPassword,
    })
  ).data!;
  await login("new");
  assert.equal(
    (await request<Schema["CurrentPrincipal"]>("/api/v1/users/me")).data!
      .mustChangePassword,
    true,
  );
  await request("/api/v1/users/me/password", "PUT", {
    currentPassword: demoPassword,
    newPassword: "Changed-Demo-2026!",
  });
  assert.equal((await request("/api/v1/users/me")).status, 401);
  await login("new", "Changed-Demo-2026!");
  assert.equal(
    (await request<Schema["CurrentPrincipal"]>("/api/v1/users/me")).data!
      .mustChangePassword,
    false,
  );
  await login();
  assert.equal(
    (await request<Schema["User"]>(`/api/v1/users/${created.id}`)).data!
      .displayName,
    "新同事",
  );
});

test("OIDC 本地跳转与准入批准转为正式 User，不接入外部 Provider", async (t) => {
  const { request, login } = await environment(t);
  const started = (
    await request<Schema["OIDCStart"]>(
      "/api/v1/auth/oidc/mock-oidc/start",
      "POST",
      {},
    )
  ).data!;
  const flow = new URL(started.authorizationUrl).searchParams.get("flow");
  await request("/__demo/oidc/complete", "POST", { flow, account: "newcomer" });
  assert.equal(
    (await request<Schema["CurrentPrincipal"]>("/api/v1/users/me")).data!.kind,
    "pending",
  );
  await login();
  await request("/api/v1/auth/admissions/identity-pending/approve", "POST", {});
  const second = (
    await request<Schema["OIDCStart"]>(
      "/api/v1/auth/oidc/mock-oidc/start",
      "POST",
      {},
    )
  ).data!;
  await request("/__demo/oidc/complete", "POST", {
    flow: new URL(second.authorizationUrl).searchParams.get("flow"),
    account: "newcomer",
  });
  const principal = (
    await request<Schema["CurrentPrincipal"]>("/api/v1/users/me")
  ).data!;
  assert.equal(principal.kind, "user");
  assert.equal(principal.user!.platformRole, "user");
});

test("缺少命令 Header 或外部 Origin 被拒绝，重置仅影响 Mock", async (t) => {
  const { request, base } = await environment(t);
  assert.equal(
    (await fetch(base + "/__demo/reset", { method: "POST" })).status,
    403,
  );
  assert.equal(
    (
      await fetch(base + "/__demo/reset", {
        method: "POST",
        headers: { Origin: "https://outside.example", "X-Orbit-CSRF": "1" },
      })
    ).status,
    403,
  );
  assert.equal((await request("/api/v1/unknown-resource")).status, 404);
  await request("/__demo/reset", "POST", {});
  assert.equal((await request("/api/v1/users/me")).status, 401);
  assert.equal((await request("/healthz")).status, 200);
});
