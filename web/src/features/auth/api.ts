import {
  client,
  requireData,
  requireSuccess,
  type AuthProvider,
  type CurrentPrincipal,
} from "@/api/http";

export const principalQueryKey = ["principal"] as const;

export async function getCurrentPrincipal(): Promise<CurrentPrincipal> {
  return requireData(await client.GET("/api/v1/users/me"), "查询当前身份");
}

export async function listAuthProviders(): Promise<AuthProvider[]> {
  return requireData(
    await client.GET("/api/v1/auth/providers"),
    "查询登录方式",
  );
}

export async function loginLocal(
  loginName: string,
  password: string,
): Promise<CurrentPrincipal> {
  return requireData(
    await client.POST("/api/v1/auth/login", { body: { loginName, password } }),
    "登录",
  );
}

export async function startOIDCLogin(providerId: string): Promise<string> {
  const result = await client.POST("/api/v1/auth/oidc/{providerId}/start", {
    params: { path: { providerId } },
  });
  return requireData(result, "开始 OIDC 登录").authorizationUrl;
}

export type PasswordCommand =
  | { currentPassword: string; newPassword: string }
  | { loginName: string; newPassword: string };

export async function changePassword(command: PasswordCommand): Promise<void> {
  const result = await client.PUT("/api/v1/users/me/password", {
    body: command,
  });
  requireSuccess(result, "修改密码");
}

export async function logout(): Promise<void> {
  requireSuccess(await client.POST("/api/v1/auth/logout"), "退出登录");
}
