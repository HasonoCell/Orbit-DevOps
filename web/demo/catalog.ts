import { choice, integer, ok, text, type Handler } from "./http.ts";
import { fail, find, id, now, pageOf, type Schema } from "./state.ts";

/** 模拟资源与成员的 HTTP 契约；项目边界、角色和最后一个 owner 的约束仍可在界面体验。 */
export const catalogHandler: Handler = (ctx) => {
  const { path, method, store: s, body, session, url } = ctx;
  if (path === "/api/v1/projects") {
    const user = s.user(session);
    if (method === "GET")
      return ok(
        pageOf(
          s.projects.filter((p) =>
            s.members.some((m) => m.projectId === p.id && m.userId === user.id),
          ),
          url,
        ),
      );
    if (method === "POST") {
      if (user.platformRole !== "platform_admin")
        fail(403, "forbidden", "需要平台管理员权限");
      const slug = text(body, "slug");
      if (s.projects.some((p) => p.slug === slug))
        fail(409, "slug_conflict", "项目标识已存在");
      const project = {
        id: id(),
        name: text(body, "name"),
        slug,
        createdBy: user.id,
        createdAt: now(),
      };
      s.projects.unshift(project);
      s.members.push({
        projectId: project.id,
        userId: user.id,
        displayName: user.displayName,
        role: "owner",
        createdBy: user.id,
        createdAt: now(),
        updatedAt: now(),
      });
      return ok(project, 201);
    }
  }
  const projectMatch = path.match(
    /^\/api\/v1\/projects\/([^/]+)(?:\/(permissions|applications|application-workbench|members|member-candidate:resolve)(?:\/([^/]+))?)?$/,
  );
  if (projectMatch) {
    const [, projectId, section, userId] = projectMatch;
    const project = find(s.projects, projectId);
    s.authorize(projectId, "read", session);
    if (!section && method === "GET") return ok(project);
    if (section === "permissions" && method === "GET")
      return ok(s.permissions(projectId, session));
    if (section === "applications") {
      if (method === "GET")
        return ok(
          pageOf(
            s.applications.filter((a) => a.projectId === projectId),
            url,
          ),
        );
      if (method === "POST") {
        s.authorize(projectId, "develop", session);
        const slug = text(body, "slug");
        if (
          s.applications.some(
            (a) => a.projectId === projectId && a.slug === slug,
          )
        )
          fail(409, "slug_conflict", "应用标识已存在");
        const app = {
          id: id(),
          projectId,
          name: text(body, "name"),
          slug,
          createdBy: s.user(session).id,
          createdAt: now(),
        };
        s.applications.unshift(app);
        return ok(app, 201);
      }
    }
    if (section === "application-workbench" && method === "GET") {
      const applications = pageOf(
        s.applications.filter((a) => a.projectId === projectId),
        url,
      );
      return ok({
        ...applications,
        items: applications.items.map(
          (app): Schema["ApplicationWorkbenchItem"] => {
            const built = s.builds.find(
              (b) => b.build.applicationId === app.id,
            );
            return {
              application: app,
              targets: s.targets
                .filter((t) => t.applicationId === app.id)
                .map((t) => {
                  const release = s.releases.find(
                    (r) => r.release.deploymentTargetId === t.id,
                  );
                  return {
                    id: t.id,
                    stage: t.stage,
                    ...(release
                      ? { releaseStatus: release.releaseOperation.status }
                      : {}),
                  };
                }),
              ...(built
                ? {
                    build: {
                      status: built.buildOperation.status,
                      createdAt: built.build.createdAt,
                    },
                  }
                : {}),
              pipelines: s.pipelines
                .filter((p) => p.pipeline.applicationId === app.id)
                .map((p) => {
                  const run = s.runs.find(
                    (r) => r.run.deliveryPipelineId === p.pipeline.id,
                  );
                  return {
                    id: p.pipeline.id,
                    name: p.pipeline.name,
                    ...(run
                      ? {
                          runStatus: run.status,
                          runCreatedAt: run.run.createdAt,
                        }
                      : {}),
                  };
                }),
            };
          },
        ),
      });
    }
    if (section === "member-candidate:resolve" && method === "POST") {
      s.authorize(projectId, "manage_members", session);
      const value = text(body, "value");
      const user =
        body.kind === "user_id"
          ? s.users.find((u) => u.id === value)
          : body.kind === "login_name"
            ? s.users.find((u) => s.credentials.get(u.id)?.loginName === value)
            : s.users.find((u) =>
                s.identities.some(
                  (i) =>
                    i.userId === u.id && i.email === value && i.emailVerified,
                ),
              );
      return ok(
        user && user.status === "active"
          ? {
              status: "found",
              candidate: { userId: user.id, displayName: user.displayName },
            }
          : { status: "not_found" },
      );
    }
    if (section === "members") {
      if (method === "GET")
        return ok(
          pageOf(
            s.members
              .filter((m) => m.projectId === projectId)
              .map((m) => ({
                ...m,
                displayName: find(s.users, m.userId).displayName,
              })),
            url,
          ),
        );
      s.authorize(projectId, "manage_members", session);
      const memberId = userId ?? text(body, "userId");
      const existing = s.members.find(
        (m) => m.projectId === projectId && m.userId === memberId,
      );
      const role =
        method === "DELETE"
          ? undefined
          : choice(body, "role", [
              "owner",
              "admin",
              "developer",
              "viewer",
            ] as const);
      if (existing?.role === "owner" || role === "owner")
        s.authorize(projectId, "manage_owners", session);
      if (
        existing?.role === "owner" &&
        role !== "owner" &&
        s.members.filter((m) => m.projectId === projectId && m.role === "owner")
          .length === 1
      )
        fail(409, "last_project_owner", "项目至少需要保留一个 owner");
      if (method === "POST") {
        if (existing) fail(409, "member_exists", "用户已是项目成员");
        const user = find(s.users, memberId);
        if (user.status !== "active") fail(409, "user_disabled", "用户已停用");
        const member: Schema["ProjectMember"] = {
          projectId,
          userId: memberId,
          displayName: user.displayName,
          role: role!,
          createdBy: s.user(session).id,
          createdAt: now(),
          updatedAt: now(),
        };
        s.members.push(member);
        return ok(member, 201);
      }
      if (!existing) fail(404, "member_not_found", "成员不存在");
      if (method === "PUT") {
        existing.role = role!;
        existing.updatedAt = now();
        return ok(existing);
      }
      if (method === "DELETE") {
        s.members = s.members.filter((m) => m !== existing);
        return ok(existing);
      }
    }
  }
  const appMatch = path.match(
    /^\/api\/v1\/applications\/([^/]+)(?:\/(deployment-targets))?$/,
  );
  if (appMatch) {
    const app = find(s.applications, appMatch[1]);
    s.authorize(app.projectId, "read", session);
    if (!appMatch[2] && method === "GET") return ok(app);
    if (appMatch[2] === "deployment-targets") {
      if (method === "GET")
        return ok(
          pageOf(
            s.targets.filter((t) => t.applicationId === app.id),
            url,
          ),
        );
      if (method === "POST") {
        s.authorize(app.projectId, "develop", session);
        const stage = choice(body, "stage", [
          "development",
          "production",
        ] as const);
        if (
          s.targets.some((t) => t.applicationId === app.id && t.stage === stage)
        )
          fail(409, "target_exists", "该 Stage 的 Target 已存在");
        const target: Schema["DeploymentTarget"] = {
          id: id(),
          applicationId: app.id,
          stage,
          clusterRef: "demo-cluster",
          namespace: find(s.projects, app.projectId).slug,
          replicas: integer(body, "replicas", 1, 0, 100),
          containerPort: integer(body, "containerPort", 8080, 1),
          createdBy: s.user(session).id,
          createdAt: now(),
          updatedAt: now(),
        };
        s.targets.push(target);
        return ok(target, 201);
      }
    }
  }
  const targetMatch = path.match(/^\/api\/v1\/deployment-targets\/([^/]+)$/);
  if (targetMatch) {
    const target = find(s.targets, targetMatch[1]);
    const app = find(s.applications, target.applicationId);
    s.authorize(app.projectId, "read", session);
    if (method === "GET") return ok(target);
    if (method === "PUT") {
      s.authorize(app.projectId, "develop", session);
      target.replicas = integer(body, "replicas", target.replicas, 0, 100);
      target.containerPort = integer(
        body,
        "containerPort",
        target.containerPort,
        1,
      );
      target.updatedAt = now();
      return ok(target);
    }
  }
};
