import { randomUUID } from "node:crypto";
import type { components } from "../src/api/schema.d.ts";
import {
  application,
  build,
  diagnostic,
  pipeline,
  project,
  release,
  run,
  target,
} from "../tests/fixtures/overview.ts";

export type Schema = components["schemas"];
export type Outcome = "success" | "failed" | "unknown" | "hold";
export type Operation = Schema["BuildOperation"] | Schema["ReleaseOperation"];
export type DemoSession = {
  id: string;
  userId?: string;
  identityId?: string;
  method: "password" | "oidc";
  recentAt: number;
  createdAt: string;
};
export type Job = {
  kind: "build" | "release";
  operationId: string;
  outcome: Outcome;
  started: number;
};
export const now = () => new Date().toISOString();
export const id = () => randomUUID();
export const copy = <T>(value: T): T => structuredClone(value);
// 这是公开的虚构演示口令，不是任何真实系统的凭据。
export const demoPassword = "Orbit-Demo-2026!";

export class MockError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}
export function fail(status: number, code: string, message: string): never {
  throw new MockError(status, code, message);
}
export function find<T extends { id: string }>(items: T[], key: string): T {
  return (
    items.find((item) => item.id === key) ??
    fail(404, "not_found", "资源不存在")
  );
}
export function pageOf<T>(items: T[], url: URL) {
  const cursor = url.searchParams.get("cursor");
  const offset = cursor
    ? Number(cursor)
    : Number(url.searchParams.get("offset") ?? 0);
  const limit = Math.min(
    100,
    Math.max(1, Number(url.searchParams.get("limit") ?? 20)),
  );
  if (!Number.isInteger(offset) || offset < 0 || !Number.isFinite(limit))
    fail(400, "invalid_page", "分页参数无效");
  return {
    items: items.slice(offset, offset + limit),
    ...(offset + limit < items.length
      ? { nextCursor: String(offset + limit) }
      : {}),
  };
}

/** 只持有虚构的进程内数据。页面刷新可回读，重启或显式重置恢复种子，不接触用户数据。 */
export class DemoStore {
  users: Schema["User"][] = [];
  credentials = new Map<
    string,
    { loginName: string; password: string; temporary: boolean }
  >();
  sessions = new Map<string, DemoSession>();
  identities: Schema["ExternalIdentity"][] = [];
  projects: Schema["Project"][] = [];
  applications: Schema["Application"][] = [];
  targets: Schema["DeploymentTarget"][] = [];
  members: Schema["ProjectMember"][] = [];
  builds: Schema["BuildAcceptance"][] = [];
  releases: Schema["ReleaseDetail"][] = [];
  pipelines: Schema["DeliveryPipelineDetail"][] = [];
  runs: Schema["DeliveryRunDetail"][] = [];
  runSpecs = new Map<string, Schema["DeliveryPipelineRevision"]>();
  hosts: Schema["AccessHost"][] = [];
  routes: Schema["AccessRoute"][] = [];
  bindings: Schema["AccessSecretBinding"][] = [];
  hostChanges = new Map<string, { revision: number; at: number }>();
  deletions = new Map<string, number>();
  runtime = new Map<string, string>();
  jobs = new Map<string, Job>();
  dedup = new Map<
    string,
    { payload: string; result: unknown; status: number }
  >();
  oidcFlows = new Map<
    string,
    { kind: "login" | "bind" | "reauth"; sessionId?: string }
  >();
  nextBuild: Outcome = "success";
  nextRelease: Outcome = "success";
  evidence:
    | "normal"
    | "dns_mismatch"
    | "controller_unavailable"
    | "certificate_failed"
    | "pod_crash" = "normal";
  stepMs: number;
  constructor(stepMs = 5000) {
    this.stepMs = stepMs;
    this.reset();
  }
  reset() {
    this.sessions.clear();
    this.credentials.clear();
    this.jobs.clear();
    this.dedup.clear();
    this.oidcFlows.clear();
    this.runtime.clear();
    this.hostChanges.clear();
    this.runSpecs.clear();
    this.deletions.clear();
    this.nextBuild = "success";
    this.nextRelease = "success";
    this.evidence = "normal";
    this.users = [
      {
        id: "u-1",
        displayName: "演示管理员",
        platformRole: "platform_admin",
        status: "active",
        createdAt: now(),
      },
      {
        id: "u-2",
        displayName: "演示开发者",
        platformRole: "user",
        status: "active",
        createdAt: now(),
      },
      {
        id: "u-3",
        displayName: "演示观察者",
        platformRole: "user",
        status: "active",
        createdAt: now(),
      },
    ];
    ["demo", "developer", "viewer"].forEach((loginName, index) =>
      this.credentials.set(`u-${index + 1}`, {
        loginName,
        password: demoPassword,
        temporary: false,
      }),
    );
    this.identities = [
      {
        id: "identity-pending",
        providerId: "mock-oidc",
        status: "pending",
        displayName: "OIDC 申请人",
        email: "newcomer@example.test",
        emailVerified: true,
        createdAt: now(),
        updatedAt: now(),
      },
    ];
    this.projects = [
      copy(project),
      {
        ...copy(project),
        id: "p-2",
        name: "Orbit DevOps",
        slug: "orbit-devops",
      },
    ];
    this.applications = [
      copy(application),
      ...[
        "User",
        "Catalog",
        "Order",
        "Cart",
        "Inventory",
        "Notification",
        "Gateway",
        "Search",
        "Shipping",
      ].map((name, index) => ({
        ...copy(application),
        id: `a-${index + 2}`,
        name: `${name} Service`,
        slug: `${name.toLowerCase()}-service`,
      })),
      {
        ...copy(application),
        id: "a-orbit",
        projectId: "p-2",
        name: "Orbit API",
        slug: "orbit-api",
      },
    ];
    this.targets = this.applications.flatMap((app) =>
      (["production", "development"] as const).map((stage) => ({
        ...copy(target),
        id:
          app.id === "a-1" && stage === "production"
            ? "t-1"
            : `${app.id}-${stage}`,
        applicationId: app.id,
        stage,
        namespace: app.projectId === "p-1" ? "yuuki" : "orbit",
        replicas: stage === "production" ? 2 : 1,
      })),
    );
    this.members = this.projects.flatMap((p) =>
      this.users.map((user, index) => ({
        projectId: p.id,
        userId: user.id,
        displayName: user.displayName,
        role: (["owner", "developer", "viewer"] as const)[index],
        createdBy: "u-1",
        createdAt: now(),
        updatedAt: now(),
      })),
    );
    this.builds = [];
    this.releases = [];
    this.pipelines = [];
    this.runs = [];
    for (const app of this.applications) {
      const built = copy(build);
      built.build = {
        ...built.build,
        id: `${app.id}-build`,
        applicationId: app.id,
        projectId: app.projectId,
        contextPath: `services/${app.slug}`,
        destinationRepository: `registry.example.test/${app.slug}`,
      };
      built.buildOperation = {
        ...built.buildOperation,
        id: `${app.id}-build-op`,
        buildId: built.build.id,
        attempts: [
          {
            id: `${app.id}-build-attempt`,
            number: 1,
            workerId: "mock-build-worker",
            status: "succeeded",
            startedAt: now(),
            finishedAt: now(),
          },
        ],
      };
      built.imageArtifact = {
        ...built.imageArtifact!,
        id: `${app.id}-artifact`,
        buildId: built.build.id,
        applicationId: app.id,
        projectId: app.projectId,
        repository: built.build.destinationRepository,
        imageReference: `${built.build.destinationRepository}@${built.imageArtifact!.digest}`,
      };
      this.builds.push(built);
      for (const t of this.targets.filter(
        (item) => item.applicationId === app.id,
      )) {
        const d = diagnostic();
        d.release = {
          ...copy(release.release),
          id: `${t.id}-release`,
          deploymentTargetId: t.id,
          imageArtifactId: built.imageArtifact.id,
          imageReference: built.imageArtifact.imageReference,
          targetSnapshot: {
            projectId: app.projectId,
            applicationId: app.id,
            stage: t.stage,
            clusterRef: t.clusterRef,
            namespace: t.namespace,
            replicas: t.replicas,
            containerPort: t.containerPort,
          },
        };
        d.releaseOperation = {
          ...d.releaseOperation,
          id: `${t.id}-release-op`,
          releaseId: d.release.id,
          deploymentTargetId: t.id,
          attempts: [
            {
              id: `${t.id}-release-attempt`,
              number: 1,
              workerId: "mock-release-worker",
              status: "succeeded",
              startedAt: now(),
              finishedAt: now(),
            },
          ],
        };
        this.releases.push({
          release: d.release,
          releaseOperation: d.releaseOperation,
          snapshotDifferences: [],
          auditTimeline: [],
        });
        this.runtime.set(t.id, d.release.id);
      }
    }
    const pl = copy(pipeline);
    pl.pipeline.name = "Main → Development";
    pl.revision = {
      ...pl.revision,
      endpointKey: "yuuki-demo",
      deploymentTargetId: "a-1-development",
    };
    this.pipelines.push(pl);
    this.runs.push({
      ...copy(run),
      run: {
        ...copy(run.run),
        buildId: "a-1-build",
        imageArtifactId: "a-1-artifact",
        releaseId: "a-1-development-release",
      },
    });
    this.runSpecs.set("run-1", copy(pl.revision));
    this.hosts = [
      {
        id: "h-1",
        projectId: "p-1",
        clusterRef: "demo-cluster",
        namespace: "yuuki",
        hostname: "yuuki.example.test",
        tlsMode: "managed",
        issuerPolicyKey: "demo-issuer",
        lifecycle: "active",
        createdAt: now(),
        updatedAt: now(),
      },
    ];
    this.routes = [
      {
        id: "route-1",
        hostId: "h-1",
        deploymentTargetId: "t-1",
        pathPrefix: "/payment",
        lifecycle: "active",
        createdAt: now(),
        updatedAt: now(),
      },
    ];
    this.bindings = [
      {
        id: "binding-1",
        projectId: "p-1",
        hostname: "yuuki.example.test",
        clusterRef: "demo-cluster",
        namespace: "yuuki",
        secretName: "demo-tls",
        state: "active",
        createdAt: now(),
        updatedAt: now(),
      },
    ];
    this.hostChanges.set("h-1", { revision: 1, at: Date.now() - 60_000 });
  }
  user(session?: DemoSession) {
    const user =
      session?.userId && this.users.find((item) => item.id === session.userId);
    if (!user || user.status !== "active")
      fail(401, "authentication_required", "请先登录演示账号");
    return user;
  }
  principal(session: DemoSession): Schema["CurrentPrincipal"] {
    if (session.identityId) {
      const identity = find(this.identities, session.identityId);
      if (identity.status !== "linked")
        return {
          kind: "pending",
          externalIdentity: identity,
          mustChangePassword: false,
        };
      session.userId = identity.userId;
      delete session.identityId;
    }
    const user = this.user(session);
    return {
      kind: "user",
      user,
      mustChangePassword: this.credentials.get(user.id)?.temporary ?? false,
    };
  }
  permissions(
    projectId: string,
    session?: DemoSession,
  ): Schema["ProjectPermissions"] {
    find(this.projects, projectId);
    const user = this.user(session);
    const member = this.members.find(
      (m) => m.projectId === projectId && m.userId === user.id,
    );
    if (!member) fail(403, "forbidden", "当前账号不是该项目成员");
    const allowed: Record<
      Schema["ProjectRole"],
      Schema["ProjectPermission"][]
    > = {
      viewer: ["read"],
      developer: ["read", "read_logs", "develop", "manage_access_routes"],
      admin: [
        "read",
        "read_logs",
        "develop",
        "manage_members",
        "manage_access_hosts",
        "manage_access_routes",
      ],
      owner: [
        "read",
        "read_logs",
        "develop",
        "manage_members",
        "manage_owners",
        "resolve_unknown",
        "manage_access_hosts",
        "manage_access_routes",
      ],
    };
    return { projectId, role: member.role, allowed: allowed[member.role] };
  }
  authorize(
    projectId: string,
    permission: Schema["ProjectPermission"],
    session?: DemoSession,
  ) {
    if (!this.permissions(projectId, session).allowed.includes(permission))
      fail(403, "forbidden", "当前账号无权执行此操作");
    if (
      permission !== "read" &&
      this.credentials.get(this.user(session).id)?.temporary
    )
      fail(403, "password_change_required", "请先修改临时密码");
  }
  admin(session?: DemoSession) {
    if (this.user(session).platformRole !== "platform_admin")
      fail(403, "forbidden", "需要平台管理员权限");
  }
  recent(session?: DemoSession) {
    if (!session || Date.now() - session.recentAt > 5 * 60_000)
      fail(403, "recent_authentication_required", "请重新验证身份");
  }
}
