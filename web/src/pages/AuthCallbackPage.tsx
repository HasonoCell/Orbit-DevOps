import { useQuery } from "@tanstack/react-query";
import { Navigate } from "react-router-dom";
import { getCurrentPrincipal, principalQueryKey } from "@/api/auth";
import { ErrorPanel, LoadingPage } from "@/components/PageState";

// OIDC code/state 只由后端 callback 消费；前端回跳仅重新读取 Cookie 会话。
export function AuthCallbackPage() {
  const principal = useQuery({ queryKey: principalQueryKey, queryFn: getCurrentPrincipal, retry: false, refetchOnMount: "always" });
  if (principal.isPending) return <LoadingPage label="正在完成登录" />;
  if (principal.error) return <div className="mx-auto max-w-xl p-8"><ErrorPanel title="登录未完成" error={principal.error} /></div>;
  return <Navigate to={principal.data.kind === "pending" ? "/admission" : principal.data.mustChangePassword ? "/account/password" : "/projects"} replace />;
}
