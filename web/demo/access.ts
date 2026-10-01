import { choice, ok, text, type Handler } from "./http.ts";
import {
  fail,
  find,
  id,
  now,
  pageOf,
  type DemoStore,
  type Schema,
} from "./state.ts";

function touch(s: DemoStore, hostId: string) {
  s.hostChanges.set(hostId, {
    revision: (s.hostChanges.get(hostId)?.revision ?? 0) + 1,
    at: Date.now(),
  });
}
function hostView(s: DemoStore, host: Schema["AccessHost"]) {
  return {
    ...host,
    ...(host.secretBindingId
      ? { secretBindingState: find(s.bindings, host.secretBindingId).state }
      : {}),
  };
}
export function advanceAccess(s: DemoStore) {
  for (const [key, at] of s.deletions) {
    if (Date.now() - at < 3 * s.stepMs) continue;
    s.routes = s.routes.filter((r) => r.id !== key && r.hostId !== key);
    s.hosts = s.hosts.filter((h) => h.id !== key);
    s.deletions.delete(key);
  }
}
function evidence(
  s: DemoStore,
  host: Schema["AccessHost"],
): Schema["AccessHostStatus"] {
  const change = s.hostChanges.get(host.id)!;
  const elapsed = Date.now() - change.at;
  const applied = elapsed >= s.stepMs;
  const ready =
    elapsed >= 2 * s.stepMs &&
    s.evidence !== "controller_unavailable" &&
    host.lifecycle === "active";
  const revoked =
    host.secretBindingId &&
    find(s.bindings, host.secretBindingId).state === "revoked";
  const tlsReady = ready && !revoked && s.evidence !== "certificate_failed";
  return {
    host: hostView(s, host),
    sync: {
      desiredRevision: change.revision,
      appliedRevision: applied
        ? change.revision
        : Math.max(0, change.revision - 1),
      state: applied
        ? "applied"
        : host.lifecycle === "deleting"
          ? "deleting"
          : "pending",
    },
    controller: {
      gatewayState:
        s.evidence === "controller_unavailable"
          ? "unknown"
          : ready
            ? "ready"
            : "not_ready",
      listenerState: ready && !revoked ? "ready" : "not_ready",
      routes: s.routes
        .filter((r) => r.hostId === host.id)
        .map((r) => ({
          routeId: r.id,
          accepted: ready && r.lifecycle === "active" ? "ready" : "not_ready",
          resolvedRefs:
            ready && r.lifecycle === "active" ? "ready" : "not_ready",
        })),
      certificateState:
        host.tlsMode === "http_only" || host.tlsMode === "existing_secret"
          ? "not_applicable"
          : tlsReady
            ? "ready"
            : "not_ready",
      secretState:
        host.tlsMode === "http_only"
          ? "not_applicable"
          : tlsReady
            ? "ready"
            : "not_ready",
      ...(tlsReady && host.tlsMode !== "http_only"
        ? {
            certificateNotAfter: new Date(
              Date.now() + 90 * 86400000,
            ).toISOString(),
          }
        : {}),
      ...(revoked
        ? { errorCode: "secret_binding_revoked" }
        : s.evidence === "controller_unavailable"
          ? { errorCode: "mock_controller_unavailable" }
          : {}),
      addresses: ready ? ["203.0.113.10"] : [],
      observedAt: now(),
    },
    dns: {
      state: !ready
        ? "not_configured"
        : s.evidence === "dns_mismatch"
          ? "mismatch"
          : "verified",
      answers: ready
        ? [s.evidence === "dns_mismatch" ? "203.0.113.99" : "203.0.113.10"]
        : [],
      observedAt: now(),
    },
  };
}

/** 入口调和、控制器和 DNS 分别模拟；IP、证书与 Secret 均为虚构值，不发送外部探测。 */
export const accessHandler: Handler = (ctx) => {
  const { path, method, store: s, body, session, url } = ctx;
  const binding = path.match(
    /^\/api\/v1\/platform\/access-secret-bindings(?:\/([^/]+))?$/,
  );
  if (binding) {
    s.admin(session);
    if (method === "GET") return ok(pageOf(s.bindings, url).items);
    s.recent(session);
    if (method === "POST" && !binding[1]) {
      const projectId = text(body, "projectId");
      const p = find(s.projects, projectId);
      const hostname = text(body, "hostname").toLowerCase();
      const secretName = text(body, "secretName");
      if (
        s.bindings.some(
          (b) =>
            b.projectId === projectId &&
            b.hostname === hostname &&
            b.secretName === secretName &&
            b.state === "active",
        )
      )
        fail(409, "binding_exists", "该授权已经存在");
      const record: Schema["AccessSecretBinding"] = {
        id: id(),
        projectId,
        hostname,
        secretName,
        clusterRef: "demo-cluster",
        namespace: p.id === "p-1" ? "yuuki" : p.id === "p-2" ? "orbit" : p.slug,
        state: "active",
        createdAt: now(),
        updatedAt: now(),
      };
      s.bindings.unshift(record);
      return ok(record, 201);
    }
    if (method === "DELETE" && binding[1]) {
      const record = find(s.bindings, binding[1]);
      record.state = "revoked";
      record.updatedAt = now();
      for (const host of s.hosts)
        if (host.secretBindingId === record.id) touch(s, host.id);
      return ok(record);
    }
  }
  const project = path.match(
    /^\/api\/v1\/projects\/([^/]+)\/(access-host-options|access-secret-binding-options|access-hosts)(?:\/([^/]+)(?:\/(status|routes|eligible-targets)(?:\/([^/]+))?)?)?$/,
  );
  if (project) {
    const [, projectId, section, hostId, action, routeId] = project;
    const p = find(s.projects, projectId);
    s.authorize(projectId, "read", session);
    const namespace =
      p.id === "p-1" ? "yuuki" : p.id === "p-2" ? "orbit" : p.slug;
    if (section === "access-host-options" && method === "GET")
      return ok({
        clusterRef: "demo-cluster",
        namespace,
        issuerPolicies: [
          { key: "demo-issuer", kind: "ClusterIssuer", name: "demo-ca" },
        ],
      });
    if (section === "access-secret-binding-options" && method === "GET") {
      s.authorize(projectId, "manage_access_hosts", session);
      return ok(
        pageOf(
          s.bindings.filter(
            (b) =>
              b.projectId === projectId &&
              b.hostname === url.searchParams.get("hostname") &&
              b.state === "active",
          ),
          url,
        ).items,
      );
    }
    if (section !== "access-hosts") return;
    const host = hostId ? find(s.hosts, hostId) : undefined;
    if (host && host.projectId !== projectId)
      fail(404, "not_found", "域名不属于当前项目");
    if (method === "GET") {
      if (action === "status") return ok(evidence(s, host!));
      if (action === "routes")
        return ok(
          pageOf(
            s.routes.filter((r) => r.hostId === hostId),
            url,
          ).items,
        );
      if (action === "eligible-targets") {
        s.authorize(projectId, "manage_access_routes", session);
        return ok(
          pageOf(
            s.targets
              .filter(
                (t) =>
                  t.clusterRef === host!.clusterRef &&
                  t.namespace === host!.namespace &&
                  find(s.applications, t.applicationId).projectId === projectId,
              )
              .map((t) => ({
                ...t,
                applicationName: find(s.applications, t.applicationId).name,
              })),
            url,
          ).items,
        );
      }
      return ok(
        host
          ? hostView(s, host)
          : pageOf(
              s.hosts
                .filter((h) => h.projectId === projectId)
                .map((h) => hostView(s, h)),
              url,
            ).items,
      );
    }
    s.authorize(
      projectId,
      action === "routes" ? "manage_access_routes" : "manage_access_hosts",
      session,
    );
    if (host?.lifecycle === "deleting")
      fail(409, "host_deleting", "域名正在清理");
    if (!action && (method === "POST" || method === "PATCH")) {
      const hostname = text(body, "hostname").toLowerCase();
      if (!/^[a-z\d](?:[a-z\d.-]*[a-z\d])?\.[a-z]{2,}$/i.test(hostname))
        fail(400, "invalid_hostname", "请输入有效域名");
      if (s.hosts.some((h) => h.hostname === hostname && h.id !== hostId))
        fail(409, "hostname_conflict", "域名已经被使用");
      if (host && host.hostname !== hostname)
        fail(409, "hostname_immutable", "域名不可修改");
      const tlsMode = choice(body, "tlsMode", [
        "http_only",
        "managed",
        "existing_secret",
      ] as const);
      if (tlsMode === "managed" && body.issuerPolicyKey !== "demo-issuer")
        fail(409, "issuer_not_allowed", "Issuer Policy 不可用");
      const secretBindingId =
        tlsMode === "existing_secret"
          ? text(body, "secretBindingId")
          : undefined;
      if (secretBindingId) {
        const b = find(s.bindings, secretBindingId);
        if (
          b.projectId !== projectId ||
          b.hostname !== hostname ||
          b.state !== "active"
        )
          fail(
            409,
            "secret_binding_unavailable",
            "TLS Secret 授权不适用于此域名",
          );
      }
      const record: Schema["AccessHost"] = {
        id: hostId ?? id(),
        projectId,
        hostname,
        clusterRef: "demo-cluster",
        namespace,
        tlsMode,
        ...(tlsMode === "managed" ? { issuerPolicyKey: "demo-issuer" } : {}),
        ...(secretBindingId
          ? { secretBindingId, secretBindingState: "active" }
          : {}),
        lifecycle: "active",
        createdAt: host?.createdAt ?? now(),
        updatedAt: now(),
      };
      if (host) s.hosts[s.hosts.indexOf(host)] = record;
      else s.hosts.unshift(record);
      touch(s, record.id);
      return ok(record, host ? 200 : 201);
    }
    if (!action && host && method === "DELETE") {
      host.lifecycle = "deleting";
      host.updatedAt = now();
      s.deletions.set(host.id, Date.now());
      touch(s, host.id);
      return ok(host, 202);
    }
    if (action === "routes") {
      const route = routeId ? find(s.routes, routeId) : undefined;
      if (route && route.hostId !== hostId)
        fail(404, "not_found", "路由不属于当前域名");
      if (route?.lifecycle === "deleting")
        fail(409, "route_deleting", "路由正在清理");
      if (method === "DELETE" && route) {
        route.lifecycle = "deleting";
        route.updatedAt = now();
        s.deletions.set(route.id, Date.now());
        touch(s, hostId);
        return ok(route, 202);
      }
      if (method === "POST" || method === "PATCH") {
        const pathPrefix = text(body, "pathPrefix");
        const targetId = text(body, "deploymentTargetId");
        const t = find(s.targets, targetId);
        if (!pathPrefix.startsWith("/") || /[?#]|\/\//.test(pathPrefix))
          fail(400, "invalid_path", "PathPrefix 无效");
        if (
          find(s.applications, t.applicationId).projectId !== projectId ||
          t.namespace !== host!.namespace ||
          t.clusterRef !== host!.clusterRef
        )
          fail(409, "target_boundary_conflict", "Target 与域名边界不匹配");
        if (
          s.routes.some(
            (r) =>
              r.hostId === hostId &&
              r.pathPrefix === pathPrefix &&
              r.id !== routeId,
          )
        )
          fail(409, "route_conflict", "该路径已经存在");
        const record: Schema["AccessRoute"] = {
          id: routeId ?? id(),
          hostId,
          deploymentTargetId: targetId,
          pathPrefix,
          lifecycle: "active",
          createdAt: route?.createdAt ?? now(),
          updatedAt: now(),
        };
        if (route) s.routes[s.routes.indexOf(route)] = record;
        else s.routes.unshift(record);
        touch(s, hostId);
        return ok(record, route ? 200 : 201);
      }
    }
  }
  const target = path.match(
    /^\/api\/v1\/deployment-targets\/([^/]+)\/access-routes$/,
  );
  if (target && method === "GET") {
    const t = find(s.targets, target[1]);
    s.authorize(
      find(s.applications, t.applicationId).projectId,
      "read",
      session,
    );
    return ok(
      pageOf(
        s.routes
          .filter((r) => r.deploymentTargetId === t.id)
          .map((r) => {
            const h = find(s.hosts, r.hostId);
            return {
              routeId: r.id,
              hostId: h.id,
              hostname: h.hostname,
              pathPrefix: r.pathPrefix,
              routeLifecycle: r.lifecycle,
              hostLifecycle: h.lifecycle,
            };
          }),
        url,
      ).items,
    );
  }
};
