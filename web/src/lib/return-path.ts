export const oidcReturnPathKey = "orbit:oidc-return-path";

// 只允许站内路径，避免认证完成后跳转到外部地址或认证入口本身。
export function safeNextPath(next: string | null): string {
  if (!next || !next.startsWith("/") || next.startsWith("//") || next.includes("\\") || next.startsWith("/login") || next.startsWith("/auth/callback")) {
    return "/projects";
  }
  return next;
}
