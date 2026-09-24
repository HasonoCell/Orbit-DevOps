import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Navigate } from "react-router-dom";
import { getCurrentPrincipal, principalQueryKey } from "@/api/auth";
import { ApiError } from "@/api/http";
import { ErrorPanel, LoadingPage } from "@/components/PageState";
import { oidcReturnPathKey, safeNextPath } from "@/lib/return-path";

// OIDC code/state 只由后端 callback 消费；前端回跳仅重新读取 Cookie 会话。
export function AuthCallbackPage() {
  const [next] = useState(() => safeNextPath(sessionStorage.getItem(oidcReturnPathKey)));
  useEffect(() => { sessionStorage.removeItem(oidcReturnPathKey); }, []);
  const principal = useQuery({ queryKey: principalQueryKey, queryFn: getCurrentPrincipal, retry: false, refetchOnMount: "always" });
  if (principal.isPending) return <LoadingPage label="正在完成登录" />;
  if (principal.error instanceof ApiError && principal.error.status === 401) {
    // 回跳未建立有效 Session 时重新登录，但保留原先要访问的站内路径。
    return <Navigate to={`/login?next=${encodeURIComponent(next)}`} replace />;
  }
  if (principal.error) return <div className="mx-auto max-w-xl p-8"><ErrorPanel title="登录未完成" error={principal.error} onRetry={() => void principal.refetch()} /></div>;
  return <Navigate to={principal.data.kind === "pending" ? "/admission" : principal.data.mustChangePassword ? "/account/password" : next} replace />;
}
