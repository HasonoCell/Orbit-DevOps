import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  getCurrentPrincipal,
  listAuthProviders,
  principalQueryKey,
  reauthenticateLocal,
  startOIDCReauthentication,
} from "./api";

export const reauthPopupName = "orbit-reauth";
export const reauthCompleteMessage = "orbit:reauth-complete";

/** 只恢复近期认证，不重放原命令；调用方保留表单并要求用户再次提交。 */
export function RecentAuthPrompt({ error }: { error: unknown }) {
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [failure, setFailure] = useState("");
  const popupRef = useRef<Window | null>(null);
  const queryClient = useQueryClient();
  const providers = useQuery({
    queryKey: ["auth-providers"],
    queryFn: listAuthProviders,
    enabled:
      error instanceof ApiError &&
      error.code === "recent_authentication_required",
  });
  useEffect(() => {
    async function onMessage(event: MessageEvent) {
      if (
        event.origin !== window.location.origin ||
        event.source !== popupRef.current ||
        event.data !== reauthCompleteMessage
      )
        return;
      popupRef.current = null;
      try {
        queryClient.setQueryData(
          principalQueryKey,
          await getCurrentPrincipal(),
        );
        setMessage("身份已验证，请重新提交。");
        setFailure("");
      } catch (requestError) {
        setFailure(errorText(requestError));
      }
    }
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, [queryClient]);
  if (
    !(error instanceof ApiError) ||
    error.code !== "recent_authentication_required"
  )
    return null;
  async function local(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setFailure("");
    try {
      const result = await reauthenticateLocal(password);
      setPassword("");
      queryClient.setQueryData(principalQueryKey, result);
      setMessage("身份已验证，请重新提交。");
    } catch (requestError) {
      setPassword("");
      setFailure(
        requestError instanceof ApiError && requestError.status === 401
          ? "密码不正确。"
          : errorText(requestError),
      );
    } finally {
      setBusy(false);
    }
  }
  async function oidc(providerId: string) {
    // 点击时同步创建窗口，避免异步网络请求后被浏览器拦截；原表单留在当前页内存中。
    const popup = window.open(
      "about:blank",
      reauthPopupName,
      "width=600,height=720",
    );
    if (!popup) {
      setFailure("浏览器阻止了认证窗口，请允许弹出窗口后重试。");
      return;
    }
    popupRef.current = popup;
    setBusy(true);
    setFailure("");
    try {
      popup.location.replace(await startOIDCReauthentication(providerId));
    } catch (requestError) {
      popup.close();
      popupRef.current = null;
      setFailure(errorText(requestError));
    } finally {
      setBusy(false);
    }
  }
  const localAvailable = providers.data?.some(
    (provider) => provider.type === "local" && provider.available,
  );
  const oidcProviders =
    providers.data?.filter(
      (provider) => provider.type === "oidc" && provider.available,
    ) ?? [];
  return (
    <div
      className="mt-4 space-y-3 rounded-md border border-amber-300 bg-amber-50 p-4 text-sm"
      role="group"
      aria-label="近期认证"
    >
      <p className="font-medium text-amber-950">此操作需要重新验证身份</p>
      {localAvailable && (
        <form onSubmit={local} className="flex flex-wrap items-end gap-2">
          <div className="space-y-1">
            <Label htmlFor="recent-password">当前密码</Label>
            <Input
              id="recent-password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
            />
          </div>
          <Button type="submit" variant="outline" disabled={busy || !password}>
            用密码验证
          </Button>
        </form>
      )}
      {oidcProviders.map((provider) => (
        <Button
          key={provider.id}
          type="button"
          variant="outline"
          disabled={busy}
          onClick={() => void oidc(provider.id)}
        >
          用 {provider.displayName} 验证
        </Button>
      ))}
      {providers.error && (
        <p role="alert" className="text-destructive">
          {errorText(providers.error)}
        </p>
      )}
      {failure && (
        <p role="alert" className="text-destructive">
          {failure}
        </p>
      )}
      {message && (
        <p role="status" className="text-emerald-800">
          {message}
        </p>
      )}
    </div>
  );
}
