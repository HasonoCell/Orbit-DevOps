import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { QueryNotice, Timestamp } from "@/shared/OverviewUI";
import { listCurrentSessions, logoutAll } from "./api";

export function AccountSessions() {
  const [cursors, setCursors] = useState([""]);
  const [pageIndex, setPageIndex] = useState(0);
  const [confirming, setConfirming] = useState(false);
  const cursor = cursors[pageIndex] || undefined;
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const page = useQuery({
    queryKey: ["account-sessions", cursor],
    queryFn: () => listCurrentSessions(cursor),
  });
  const revoke = useMutation({
    mutationFn: logoutAll,
    onSuccess() {
      queryClient.clear();
      navigate("/login", { replace: true });
    },
  });
  return (
    <Card>
      <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-3">
        <CardTitle>有效会话</CardTitle>
        <Button variant="outline" onClick={() => setConfirming(true)}>
          退出全部会话
        </Button>
      </CardHeader>
      <CardContent className="space-y-4">
        {page.isPending || page.error ? (
          <QueryNotice error={page.error} retry={() => void page.refetch()} />
        ) : (
          <>
            {page.data.items.length ? (
              page.data.items.map((session) => (
                <div
                  key={session.id}
                  className="flex flex-wrap items-center justify-between gap-3 border-b pb-3 text-sm last:border-0 last:pb-0"
                >
                  <div>
                    <p className="font-medium">
                      {session.method === "password"
                        ? "本地密码"
                        : `OIDC · ${session.providerId ?? "—"}`}{" "}
                      {session.current && (
                        <span className="ml-2 rounded bg-emerald-100 px-2 py-0.5 text-xs text-emerald-800">
                          当前会话
                        </span>
                      )}
                    </p>
                    <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
                      {session.id}
                    </p>
                  </div>
                  <div className="space-y-1 text-xs text-muted-foreground">
                    <p>
                      创建：
                      <Timestamp value={session.createdAt} />
                    </p>
                    <p>
                      活动：
                      <Timestamp value={session.lastSeenAt} />
                    </p>
                    <p>
                      到期：
                      <Timestamp value={session.expiresAt} />
                    </p>
                  </div>
                </div>
              ))
            ) : (
              <p className="text-sm text-muted-foreground">暂无有效会话</p>
            )}
            {(pageIndex > 0 || page.data.nextCursor) && (
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
                  disabled={!page.data.nextCursor}
                  onClick={() => {
                    setCursors((previous) => [
                      ...previous.slice(0, pageIndex + 1),
                      page.data.nextCursor!,
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
        {confirming && (
          <div className="flex flex-wrap items-center gap-3 rounded-md border bg-muted/40 p-3 text-sm">
            <p className="grow">确认退出所有设备？当前浏览器也需要重新登录。</p>
            <Button
              variant="destructive"
              disabled={revoke.isPending}
              onClick={() => revoke.mutate()}
            >
              确认退出全部
            </Button>
            <Button variant="ghost" onClick={() => setConfirming(false)}>
              取消
            </Button>
          </div>
        )}
        {revoke.error && (
          <p role="alert" className="text-sm text-destructive">
            {errorText(revoke.error)}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
