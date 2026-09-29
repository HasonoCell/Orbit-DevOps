import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { ApiError, errorText } from "@/api/http";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { QueryNotice, Timestamp } from "@/shared/OverviewUI";
import {
  listAuthProviders,
  listCurrentExternalIdentities,
  startExternalIdentityBinding,
  unbindExternalIdentity,
} from "./api";
import { RecentAuthPrompt } from "./RecentAuthPrompt";
import { oidcReturnPathKey } from "./return-path";

type ExternalIdentity = components["schemas"]["ExternalIdentity"];

export function AccountIdentities() {
  const [cursors, setCursors] = useState([""]);
  const [pageIndex, setPageIndex] = useState(0);
  const [removing, setRemoving] = useState<ExternalIdentity | null>(null);
  const cursor = cursors[pageIndex] || undefined;
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const identities = useQuery({
    queryKey: ["account-identities", cursor],
    queryFn: () => listCurrentExternalIdentities(cursor),
  });
  const providers = useQuery({
    queryKey: ["auth-providers"],
    queryFn: listAuthProviders,
  });
  const bind = useMutation({
    mutationFn: startExternalIdentityBinding,
    onSuccess(url) {
      // 回跳只保存站内路径；OIDC code/state 与 Cookie 不进入前端存储。
      sessionStorage.setItem(oidcReturnPathKey, "/account");
      window.location.assign(url);
    },
  });
  const unbind = useMutation({
    mutationFn: unbindExternalIdentity,
    onSuccess() {
      // 后端撤销该 User 原会话，不能继续使用旧页面的身份缓存。
      queryClient.clear();
      navigate("/login", { replace: true });
    },
  });
  return (
    <Card>
      <CardHeader>
        <CardTitle>外部身份</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {identities.isPending || identities.error ? (
          <QueryNotice
            error={identities.error}
            retry={() => void identities.refetch()}
          />
        ) : (
          <>
            {identities.data.items.length ? (
              identities.data.items.map((item) => (
                <div
                  key={item.id}
                  className="flex flex-wrap items-center justify-between gap-3 border-b pb-3 text-sm last:border-0 last:pb-0"
                >
                  <div>
                    <p className="font-medium">
                      {item.displayName}{" "}
                      <span className="ml-2 text-xs text-muted-foreground">
                        {item.providerId}
                      </span>
                    </p>
                    <p className="mt-1 text-xs text-muted-foreground">
                      {item.emailVerified && item.email
                        ? item.email
                        : "邮箱未验证"}{" "}
                      ·{" "}
                      {item.status === "linked"
                        ? "已绑定"
                        : item.status === "pending"
                          ? "待准入"
                          : "已拒绝"}
                    </p>
                    <p className="mt-1 text-xs text-muted-foreground">
                      更新于 <Timestamp value={item.updatedAt} />
                    </p>
                  </div>
                  {item.status === "linked" && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => {
                        setRemoving(item);
                        unbind.reset();
                        bind.reset();
                      }}
                    >
                      解除绑定
                    </Button>
                  )}
                </div>
              ))
            ) : (
              <p className="text-sm text-muted-foreground">暂无外部身份</p>
            )}
            {(pageIndex > 0 || identities.data.nextCursor) && (
              <div className="flex items-center justify-end gap-2 text-sm">
                <span>第 {pageIndex + 1} 页</span>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={pageIndex === 0}
                  onClick={() => setPageIndex(pageIndex - 1)}
                >
                  上一页
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!identities.data.nextCursor}
                  onClick={() => {
                    setCursors((previous) => [
                      ...previous.slice(0, pageIndex + 1),
                      identities.data.nextCursor!,
                    ]);
                    setPageIndex(pageIndex + 1);
                  }}
                >
                  下一页
                </Button>
              </div>
            )}
          </>
        )}
        {providers.error && (
          <QueryNotice
            error={providers.error}
            retry={() => void providers.refetch()}
          />
        )}
        {(
          providers.data?.filter(
            (provider) => provider.type === "oidc" && provider.available,
          ) ?? []
        ).map((provider) => (
          <Button
            key={provider.id}
            variant="outline"
            disabled={bind.isPending}
            onClick={() => {
              unbind.reset();
              bind.mutate(provider.id);
            }}
          >
            绑定 {provider.displayName}
          </Button>
        ))}
        {bind.error &&
          !(
            bind.error instanceof ApiError &&
            bind.error.code === "recent_authentication_required"
          ) && (
            <p role="alert" className="text-sm text-destructive">
              {errorText(bind.error)}
            </p>
          )}
        <RecentAuthPrompt error={bind.error} />
        {removing && (
          <div className="flex flex-wrap items-center gap-3 rounded-md border bg-muted/40 p-3 text-sm">
            <p className="grow">
              确认解除 {removing.displayName}？所有现有会话将失效。
            </p>
            <Button
              variant="destructive"
              disabled={unbind.isPending}
              onClick={() => unbind.mutate(removing.id)}
            >
              确认解除
            </Button>
            <Button variant="ghost" onClick={() => setRemoving(null)}>
              取消
            </Button>
          </div>
        )}
        {unbind.error &&
          !(
            unbind.error instanceof ApiError &&
            unbind.error.code === "recent_authentication_required"
          ) && (
            <p role="alert" className="text-sm text-destructive">
              {unbind.error instanceof ApiError &&
              unbind.error.code === "last_login_method"
                ? "这是最后一个可用登录方式，请先设置本地密码或绑定其他身份。"
                : errorText(unbind.error)}
            </p>
          )}
        <RecentAuthPrompt error={unbind.error} />
      </CardContent>
    </Card>
  );
}
