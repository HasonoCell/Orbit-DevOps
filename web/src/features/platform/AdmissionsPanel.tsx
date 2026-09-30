import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ApiError, errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { RecentAuthPrompt } from "@/features/auth/RecentAuthPrompt";
import { QueryNotice, Timestamp } from "@/shared/OverviewUI";
import { decideAdmission, getAdmission, listAdmissions } from "./api";

type Status = "pending" | "rejected" | "linked";
type Decision = "approve" | "reject" | "reopen";
const statusNames: Record<Status, string> = {
  pending: "待准入",
  rejected: "已拒绝",
  linked: "已关联",
};
const decisionNames: Record<Decision, string> = {
  approve: "批准准入",
  reject: "拒绝准入",
  reopen: "重新打开",
};

export function AdmissionsPanel() {
  const [params] = useSearchParams();
  const rawStatus = params.get("admissionStatus");
  const status: Status =
    rawStatus === "rejected" || rawStatus === "linked" ? rawStatus : "pending";
  return <AdmissionsContent key={status} status={status} />;
}

function AdmissionsContent({ status }: { status: Status }) {
  const [params, setParams] = useSearchParams();
  const identityId = params.get("identityId") ?? "";
  const [cursors, setCursors] = useState([""]);
  const [pageIndex, setPageIndex] = useState(0);
  const [confirming, setConfirming] = useState<Decision | null>(null);
  const [notice, setNotice] = useState("");
  const queryClient = useQueryClient();
  const cursor = cursors[pageIndex] || undefined;
  const list = useQuery({
    queryKey: ["platform-admissions", status, cursor],
    queryFn: () => listAdmissions(status, cursor),
    retry: false,
  });
  const detail = useQuery({
    queryKey: ["platform-admission", identityId],
    queryFn: () => getAdmission(identityId),
    enabled: !!identityId,
    retry: false,
  });
  const decision = useMutation({
    mutationFn: async (action: Decision) => {
      const latest = await getAdmission(identityId);
      if (latest.status !== detail.data?.status) {
        queryClient.setQueryData(["platform-admission", identityId], latest);
        throw new ApiError(
          "admission state changed",
          409,
          "admission_state_changed",
        );
      }
      return decideAdmission(identityId, action);
    },
    onSuccess(result, action) {
      setConfirming(null);
      setNotice(
        action === "approve"
          ? `批准已接纳，正式用户 ID：${"id" in result ? result.id : "—"}`
          : `${decisionNames[action]}已接纳。`,
      );
      void queryClient.invalidateQueries({ queryKey: ["platform-admissions"] });
      void queryClient.invalidateQueries({
        queryKey: ["platform-admission", identityId],
      });
    },
    onError() {
      void detail.refetch();
    },
  });
  function selectStatus(next: Status) {
    setParams((previous) => {
      const updated = new URLSearchParams(previous);
      updated.set("view", "admissions");
      updated.set("admissionStatus", next);
      updated.delete("identityId");
      return updated;
    });
  }
  function selectIdentity(id: string) {
    setConfirming(null);
    setNotice("");
    setParams((previous) => {
      const updated = new URLSearchParams(previous);
      updated.set("identityId", id);
      return updated;
    });
  }
  const candidate = detail.data;
  return (
    <div className="space-y-5">
      <section className="workbench-panel">
        <header className="panel-heading">
          <div>
            <h2>OIDC 准入</h2>
            <p className="mt-1 text-xs text-muted-foreground">
              按状态查看外部身份；邮箱只用于展示，不合并用户
            </p>
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void list.refetch()}
          >
            刷新
          </Button>
        </header>
        <nav
          className="flex flex-wrap gap-2 border-b px-5 py-3"
          aria-label="准入状态"
        >
          {(["pending", "rejected", "linked"] as const).map((item) => (
            <Button
              key={item}
              size="sm"
              variant={status === item ? "secondary" : "ghost"}
              aria-pressed={status === item}
              onClick={() => selectStatus(item)}
            >
              {statusNames[item]}
            </Button>
          ))}
        </nav>
        {list.isPending || list.error ? (
          <div className="p-5">
            <QueryNotice error={list.error} retry={() => void list.refetch()} />
          </div>
        ) : (
          <>
            {list.data.items.length ? (
              <div className="divide-y">
                {list.data.items.map((item) => (
                  <article
                    key={item.id}
                    className="flex flex-wrap items-center justify-between gap-3 p-5 text-sm"
                  >
                    <div>
                      <p className="font-medium">
                        {item.displayName}{" "}
                        <span className="ml-2 text-xs text-muted-foreground">
                          {statusNames[item.status]}
                        </span>
                      </p>
                      <p className="mt-1 text-xs text-muted-foreground">
                        {item.providerId} ·{" "}
                        {item.emailVerified && item.email
                          ? item.email
                          : "邮箱未验证"}
                      </p>
                      <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
                        {item.id}
                      </p>
                    </div>
                    <div className="flex items-center gap-3">
                      <Timestamp value={item.updatedAt} />
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => selectIdentity(item.id)}
                      >
                        详情
                      </Button>
                    </div>
                  </article>
                ))}
              </div>
            ) : (
              <p className="p-5 text-sm text-muted-foreground">
                此状态下暂无记录
              </p>
            )}
            {(pageIndex > 0 || list.data.nextCursor) && (
              <div className="flex items-center justify-end gap-2 border-t p-4 text-sm">
                <span>第 {pageIndex + 1} 页</span>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={pageIndex === 0}
                  onClick={() => setPageIndex(pageIndex - 1)}
                >
                  上一页
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={!list.data.nextCursor}
                  onClick={() => {
                    setCursors((previous) => [
                      ...previous.slice(0, pageIndex + 1),
                      list.data.nextCursor!,
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
      </section>
      {identityId && (
        <section className="workbench-panel" aria-label="准入详情">
          <header className="panel-heading">
            <h2>准入详情</h2>
            <div className="flex gap-2">
              <Button
                variant="outline"
                size="sm"
                onClick={() => void detail.refetch()}
              >
                刷新详情
              </Button>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => {
                  setConfirming(null);
                  setParams((previous) => {
                    const updated = new URLSearchParams(previous);
                    updated.delete("identityId");
                    return updated;
                  });
                }}
              >
                关闭
              </Button>
            </div>
          </header>
          {detail.isPending || detail.error ? (
            <div className="p-5">
              <QueryNotice
                error={detail.error}
                retry={() => void detail.refetch()}
              />
            </div>
          ) : (
            <div className="space-y-4 p-5 text-sm">
              <div className="grid gap-3 sm:grid-cols-2">
                <p>显示名称：{candidate!.displayName}</p>
                <p>状态：{statusNames[candidate!.status]}</p>
                <p>Provider：{candidate!.providerId}</p>
                <p>
                  已验证邮箱：
                  {candidate!.emailVerified && candidate!.email
                    ? candidate!.email
                    : "无"}
                </p>
                <p className="break-all sm:col-span-2">
                  Identity ID：{candidate!.id}
                </p>
                <p className="break-all sm:col-span-2">
                  关联 User ID：{candidate!.userId ?? "尚未关联"}
                </p>
              </div>
              {notice && (
                <p role="status" className="rounded border bg-accent p-3">
                  {notice}
                </p>
              )}
              <div className="flex flex-wrap gap-2">
                {candidate!.status === "pending" && (
                  <>
                    <Button
                      onClick={() => {
                        setConfirming("approve");
                        decision.reset();
                      }}
                    >
                      批准
                    </Button>
                    <Button
                      variant="outline"
                      onClick={() => {
                        setConfirming("reject");
                        decision.reset();
                      }}
                    >
                      拒绝
                    </Button>
                  </>
                )}
                {candidate!.status === "rejected" && (
                  <Button
                    variant="outline"
                    onClick={() => {
                      setConfirming("reopen");
                      decision.reset();
                    }}
                  >
                    重新打开
                  </Button>
                )}
              </div>
              {confirming && (
                <div className="flex flex-wrap items-center gap-3 rounded border bg-muted/40 p-3">
                  <p className="grow">
                    确认{decisionNames[confirming]} {candidate!.displayName}？
                  </p>
                  <Button
                    variant="destructive"
                    disabled={decision.isPending}
                    onClick={() => decision.mutate(confirming)}
                  >
                    确认提交
                  </Button>
                  <Button variant="ghost" onClick={() => setConfirming(null)}>
                    取消
                  </Button>
                </div>
              )}
              {decision.error &&
                !(
                  decision.error instanceof ApiError &&
                  decision.error.code === "recent_authentication_required"
                ) && (
                  <p role="alert" className="text-destructive">
                    {decision.error instanceof ApiError &&
                    (decision.error.code === "admission_state_conflict" ||
                      decision.error.code === "admission_state_changed")
                      ? "准入状态已变化，请刷新详情后重新确认。"
                      : errorText(decision.error)}
                    {!(decision.error instanceof ApiError) &&
                      " 结果未确认，请先刷新详情。"}
                  </p>
                )}
              <RecentAuthPrompt error={decision.error} />
            </div>
          )}
        </section>
      )}
    </div>
  );
}
