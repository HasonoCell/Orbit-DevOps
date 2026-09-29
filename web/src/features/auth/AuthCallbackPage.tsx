import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Navigate } from "react-router-dom";
import { getCurrentPrincipal, principalQueryKey } from "@/features/auth/api";
import { ApiError } from "@/api/http";
import { ErrorPanel, LoadingPage } from "@/shared/PageState";
import { oidcReturnPathKey, safeNextPath } from "@/features/auth/return-path";
import {
  reauthCompleteMessage,
  reauthPopupName,
} from "@/features/auth/RecentAuthPrompt";

// OIDC code/state 只由后端 callback 消费；前端回跳仅重新读取 Cookie 会话。
export function AuthCallbackPage() {
  const [next] = useState(() =>
    safeNextPath(sessionStorage.getItem(oidcReturnPathKey)),
  );
  useEffect(() => {
    sessionStorage.removeItem(oidcReturnPathKey);
  }, []);
  const principal = useQuery({
    queryKey: principalQueryKey,
    queryFn: getCurrentPrincipal,
    retry: false,
    refetchOnMount: "always",
  });
  const reauthPopup = window.name === reauthPopupName && Boolean(window.opener);
  useEffect(() => {
    if (reauthPopup && principal.isSuccess) {
      window.opener.postMessage(reauthCompleteMessage, window.location.origin);
      window.name = "";
      window.close();
    }
  }, [reauthPopup, principal.isSuccess]);
  if (principal.isPending) return <LoadingPage label="正在完成登录" />;
  if (principal.error instanceof ApiError && principal.error.status === 401) {
    // 回跳未建立有效 Session 时重新登录，但保留原先要访问的站内路径。
    return <Navigate to={`/login?next=${encodeURIComponent(next)}`} replace />;
  }
  if (principal.error)
    return (
      <div className="mx-auto max-w-xl p-8">
        <ErrorPanel
          title="登录未完成"
          error={principal.error}
          onRetry={() => void principal.refetch()}
        />
      </div>
    );
  if (reauthPopup) return <LoadingPage label="认证已完成，请返回原窗口" />;
  return (
    <Navigate
      to={
        principal.data.kind === "pending"
          ? "/admission"
          : principal.data.mustChangePassword
            ? "/account/password"
            : next
      }
      replace
    />
  );
}
