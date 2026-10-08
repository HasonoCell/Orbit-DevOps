import { choice, cookie, ok, text, type Handler } from "./http.ts";
import {
  fail,
  find,
  id,
  now,
  pageOf,
  type DemoSession,
  type Schema,
} from "./state.ts";

/** Local 与 OIDC 在模拟边界也汇合为 Session；OIDC 只模拟跳转，不宣称执行真实协议。 */
export const identityHandler: Handler = (ctx) => {
  const { path, method, store: s, body, session, url } = ctx;
  const newSession = (value: Partial<DemoSession>) => {
    const created: DemoSession = {
      id: id(),
      method: "password",
      recentAt: Date.now(),
      createdAt: now(),
      ...value,
    };
    s.sessions.set(created.id, created);
    return ok(s.principal(created), 200, cookie(created.id));
  };
  if (path === "/api/v1/auth/providers" && method === "GET")
    return ok([
      { id: "local", type: "local", displayName: "本地账号", available: true },
      {
        id: "mock-oidc",
        type: "oidc",
        displayName: "演示 OIDC",
        available: true,
      },
    ]);
  if (path === "/api/v1/auth/login" && method === "POST") {
    const credential = [...s.credentials].find(
      ([, c]) => c.loginName === body.loginName && c.password === body.password,
    );
    if (!credential || find(s.users, credential[0]).status !== "active")
      fail(401, "invalid_credentials", "账号或密码错误");
    return newSession({ userId: credential[0] });
  }
  const start = path.match(
    /^\/api\/v1\/(?:auth\/oidc\/([^/]+)\/(start|reauth)|users\/me\/external-identities\/([^/]+)\/(bind))$/,
  );
  if (start && method === "POST") {
    if ((start[1] ?? start[3]) !== "mock-oidc")
      fail(404, "provider_not_found", "登录方式不存在");
    const kind =
      start[2] === "start"
        ? "login"
        : start[2] === "reauth"
          ? "reauth"
          : "bind";
    if (kind !== "login") s.user(session);
    if (kind === "bind") s.recent(session);
    const flow = id();
    s.oidcFlows.set(flow, { kind, sessionId: session?.id });
    return ok({
      authorizationUrl: `${ctx.apiOrigin}/__demo/oidc?flow=${flow}`,
      expiresAt: new Date(Date.now() + 5 * 60_000).toISOString(),
    });
  }
  if (path === "/__demo/oidc/complete" && method === "POST") {
    const flowId = text(body, "flow");
    const flow =
      s.oidcFlows.get(flowId) ??
      fail(409, "flow_expired", "模拟登录已失效，请重新发起");
    if (flow.kind !== "login" && (!session || session.id !== flow.sessionId))
      fail(403, "flow_conflict", "请在原会话完成模拟登录");
    s.oidcFlows.delete(flowId);
    let result;
    if (flow.kind === "reauth") {
      s.user(session);
      session!.recentAt = Date.now();
      result = ok(s.principal(session!));
    } else if (flow.kind === "bind") {
      const user = s.user(session);
      if (
        s.identities.some(
          (i) => i.userId === user.id && i.providerId === "mock-oidc",
        )
      )
        fail(409, "identity_already_linked", "当前用户已经绑定");
      s.identities.unshift({
        id: id(),
        providerId: "mock-oidc",
        status: "linked",
        userId: user.id,
        displayName: `${user.displayName} OIDC`,
        email: `${s.credentials.get(user.id)?.loginName ?? user.id}@example.test`,
        emailVerified: true,
        createdAt: now(),
        updatedAt: now(),
      });
      result = ok(s.principal(session!));
    } else if (body.account === "newcomer") {
      result = newSession({ identityId: "identity-pending", method: "oidc" });
    } else {
      const loginName = choice(
        body,
        "account",
        ["demo", "developer", "viewer"] as const,
        "demo",
      );
      const userId = [...s.credentials].find(
        ([, c]) => c.loginName === loginName,
      )![0];
      result = newSession({ userId, method: "oidc" });
    }
    return ok({ next: `${ctx.origin}/auth/callback` }, 200, result.headers);
  }
  if (path === "/api/v1/users/me" && method === "GET") {
    if (!session) fail(401, "authentication_required", "请先登录");
    return ok(s.principal(session));
  }
  if (path === "/api/v1/auth/logout" || path === "/api/v1/auth/logout-all") {
    if (method !== "POST") return;
    if (path.endsWith("logout-all") && session?.userId) {
      for (const [key, value] of s.sessions)
        if (value.userId === session.userId) s.sessions.delete(key);
    } else if (session) s.sessions.delete(session.id);
    return ok(undefined, 204, cookie());
  }
  if (path === "/api/v1/auth/reauth/local" && method === "POST") {
    const user = s.user(session);
    if (s.credentials.get(user.id)?.password !== body.password)
      fail(401, "invalid_credentials", "密码错误");
    session!.recentAt = Date.now();
    return ok(s.principal(session!));
  }
  if (path === "/api/v1/users/me/password" && method === "PUT") {
    const user = s.user(session);
    const credential = s.credentials.get(user.id);
    if (credential && credential.password !== body.currentPassword)
      fail(401, "invalid_credentials", "当前密码错误");
    const password = text(body, "newPassword");
    if ([...password].length < 12)
      fail(400, "invalid_password", "密码至少 12 个字符");
    const loginName = credential?.loginName ?? text(body, "loginName");
    if (
      [...s.credentials].some(
        ([key, c]) => key !== user.id && c.loginName === loginName,
      )
    )
      fail(409, "login_name_conflict", "登录名已占用");
    s.credentials.set(user.id, { loginName, password, temporary: false });
    // 与正式 API 的交互契约一致：密码变更后，包括当前浏览器在内的旧会话全部失效。
    for (const [key, value] of s.sessions)
      if (value.userId === user.id) s.sessions.delete(key);
    return ok(undefined, 204, cookie());
  }
  if (path === "/api/v1/users/me/sessions" && method === "GET") {
    const user = s.user(session);
    return ok(
      pageOf(
        [...s.sessions.values()]
          .filter((value) => value.userId === user.id)
          .map((value): Schema["SessionSummary"] => ({
            id: value.id,
            method: value.method,
            ...(value.method === "oidc" ? { providerId: "mock-oidc" } : {}),
            createdAt: value.createdAt,
            lastSeenAt: now(),
            expiresAt: new Date(Date.now() + 86400000).toISOString(),
            current: value.id === session?.id,
          })),
        url,
      ),
    );
  }
  if (path === "/api/v1/users/me/external-identities" && method === "GET") {
    const user = s.user(session);
    return ok(
      pageOf(
        s.identities.filter((i) => i.userId === user.id),
        url,
      ),
    );
  }
  const unlink = path.match(
    /^\/api\/v1\/users\/me\/external-identities\/([^/]+)$/,
  );
  if (unlink && method === "DELETE") {
    const user = s.user(session);
    s.recent(session);
    const identity = find(s.identities, unlink[1]);
    if (identity.userId !== user.id)
      fail(403, "forbidden", "身份不属于当前用户");
    if (
      !s.credentials.has(user.id) &&
      s.identities.filter((i) => i.userId === user.id).length <= 1
    )
      fail(409, "last_login_method", "需要保留一个可用登录方式");
    s.identities = s.identities.filter((i) => i !== identity);
    for (const [key, value] of s.sessions)
      if (value.userId === user.id) s.sessions.delete(key);
    return ok(undefined, 204, cookie());
  }
  const userMatch = path.match(
    /^\/api\/v1\/users(?:\/([^/]+)(?:\/(platform-role|disable|enable|password\/reset))?)?$/,
  );
  if (userMatch) {
    s.admin(session);
    const [, userId, action] = userMatch;
    if (method === "GET")
      return ok(userId ? find(s.users, userId) : pageOf(s.users, url));
    s.recent(session);
    if (!userId && method === "POST") {
      const loginName = text(body, "loginName");
      if ([...s.credentials.values()].some((c) => c.loginName === loginName))
        fail(409, "login_name_conflict", "登录名已占用");
      const password = text(body, "temporaryPassword");
      if ([...password].length < 12)
        fail(400, "invalid_password", "密码至少 12 个字符");
      const user: Schema["User"] = {
        id: id(),
        displayName: text(body, "displayName"),
        status: "active",
        platformRole: "user",
        createdAt: now(),
      };
      s.users.unshift(user);
      s.credentials.set(user.id, { loginName, password, temporary: true });
      return ok(user, 201);
    }
    const user = find(s.users, userId);
    const role =
      action === "platform-role"
        ? choice(body, "role", ["user", "platform_admin"] as const)
        : user.platformRole;
    if (
      user.platformRole === "platform_admin" &&
      (role === "user" || action === "disable") &&
      s.users.filter(
        (u) => u.platformRole === "platform_admin" && u.status === "active",
      ).length === 1
    )
      fail(409, "last_platform_administrator", "需要保留一个有效平台管理员");
    if (action === "platform-role" && method === "PUT")
      user.platformRole = role;
    else if (action === "disable" && method === "POST")
      user.status = "disabled";
    else if (action === "enable" && method === "POST") user.status = "active";
    else if (action === "password/reset" && method === "POST") {
      const password = text(body, "temporaryPassword");
      if ([...password].length < 12)
        fail(400, "invalid_password", "密码至少 12 个字符");
      const loginName =
        s.credentials.get(user.id)?.loginName ?? text(body, "loginName");
      s.credentials.set(user.id, { loginName, password, temporary: true });
    } else return;
    for (const [key, value] of s.sessions)
      if (value.userId === user.id) s.sessions.delete(key);
    return action === "password/reset" ? ok(undefined, 204) : ok(user);
  }
  const admissionMatch = path.match(
    /^\/api\/v1\/auth\/admissions(?:\/([^/]+)(?:\/(approve|reject|reopen))?)?$/,
  );
  if (admissionMatch) {
    s.admin(session);
    const [, identityId, action] = admissionMatch;
    if (method === "GET")
      return ok(
        identityId
          ? find(s.identities, identityId)
          : pageOf(
              s.identities.filter(
                (i) =>
                  i.status === (url.searchParams.get("status") ?? "pending"),
              ),
              url,
            ),
      );
    s.recent(session);
    const identity = find(s.identities, identityId);
    if (method !== "POST") return;
    if (action === "reopen" && identity.status === "rejected")
      identity.status = "pending";
    else if (action === "reject" && identity.status === "pending")
      identity.status = "rejected";
    else if (action === "approve" && identity.status === "pending") {
      const user: Schema["User"] = {
        id: id(),
        displayName: identity.displayName,
        platformRole: "user",
        status: "active",
        createdAt: now(),
      };
      identity.userId = user.id;
      identity.status = "linked";
      identity.updatedAt = now();
      s.users.unshift(user);
      return ok(user);
    } else fail(409, "admission_state_conflict", "准入记录状态已变化");
    identity.updatedAt = now();
    return ok(identity);
  }
};
