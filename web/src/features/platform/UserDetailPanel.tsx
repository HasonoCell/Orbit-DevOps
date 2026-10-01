import { Select, SelectItem } from "@/components/ui/select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiError, errorText } from "@/api/http";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RecentAuthPrompt } from "@/features/auth/RecentAuthPrompt";
import { QueryNotice } from "@/shared/OverviewUI";
import {
  changePlatformRole,
  getUser,
  resetUserPassword,
  setUserEnabled,
} from "./api";
import { temporaryPasswordValid } from "./password-policy";

type User = components["schemas"]["User"];
type Role = components["schemas"]["PlatformRole"];
type Action = { kind: "role"; role: Role } | { kind: "enable" | "disable" };

function securityError(error: unknown) {
  if (error instanceof ApiError) {
    if (error.code === "last_platform_administrator")
      return "至少需要保留一位有效平台管理员。";
    if (error.code === "identity_invariant")
      return "该用户是最后一位有效管理员或项目所有者，暂不能停用。";
    if (error.code === "user_state_changed")
      return "用户状态已变化，请使用服务端最新状态重新确认。";
    if (error.code === "login_name_conflict") return "登录名已被占用。";
    if (error.code === "invalid_password") return "临时密码不符合服务端策略。";
  }
  return errorText(error);
}

export function UserDetailPanel({
  initialUser,
  onClose,
}: {
  initialUser: User;
  onClose: () => void;
}) {
  const [snapshot, setSnapshot] = useState(initialUser);
  const [role, setRole] = useState<Role>(initialUser.platformRole);
  const [confirming, setConfirming] = useState<Action | null>(null);
  const [temporaryPassword, setTemporaryPassword] = useState("");
  const [newLoginName, setNewLoginName] = useState("");
  const [validation, setValidation] = useState("");
  const [notice, setNotice] = useState("");
  const queryClient = useQueryClient();
  const detail = useQuery({
    queryKey: ["platform-user", initialUser.id],
    queryFn: () => getUser(initialUser.id),
    initialData: initialUser,
    refetchOnMount: "always",
    retry: false,
  });
  const changed =
    detail.data &&
    (detail.data.status !== snapshot.status ||
      detail.data.platformRole !== snapshot.platformRole);
  const action = useMutation({
    mutationFn: async (command: Action) => {
      // 接口没有版本条件；提交前再读取一次事实，检测已知的并发改动，最终仍以响应和回读为准。
      const latest = await getUser(initialUser.id);
      if (
        latest.status !== snapshot.status ||
        latest.platformRole !== snapshot.platformRole
      ) {
        queryClient.setQueryData(["platform-user", initialUser.id], latest);
        throw new ApiError("user state changed", 409, "user_state_changed");
      }
      return command.kind === "role"
        ? changePlatformRole(initialUser.id, command.role)
        : setUserEnabled(initialUser.id, command.kind === "enable");
    },
    onSuccess(updated) {
      queryClient.setQueryData(["platform-user", initialUser.id], updated);
      setSnapshot(updated);
      setRole(updated.platformRole);
      setConfirming(null);
      setNotice(
        `已接纳：${updated.displayName} 当前为${updated.status === "active" ? "正常" : "停用"}账号，平台角色为${updated.platformRole === "platform_admin" ? "管理员" : "普通用户"}。`,
      );
      void queryClient.invalidateQueries({ queryKey: ["platform-users"] });
      void detail.refetch();
    },
    onError() {
      void detail.refetch();
    },
  });
  const reset = useMutation({
    mutationFn: () =>
      resetUserPassword(initialUser.id, {
        temporaryPassword,
        ...(newLoginName.trim()
          ? { loginName: newLoginName.trim().toLowerCase() }
          : {}),
      }),
    onSuccess() {
      setTemporaryPassword("");
      setNewLoginName("");
      setValidation("");
      setNotice("密码重置已接纳。原会话已撤销，用户下次登录需修改临时密码。");
      void detail.refetch();
    },
  });
  function submitReset(event: FormEvent) {
    event.preventDefault();
    action.reset();
    if (!temporaryPasswordValid(temporaryPassword)) {
      setValidation("临时密码需为 15–128 个字符，UTF-8 不超过 512 字节。");
      return;
    }
    if (
      newLoginName.trim() &&
      !/^[a-z0-9][a-z0-9._@+-]{2,127}$/.test(newLoginName.trim().toLowerCase())
    ) {
      setValidation("登录名格式无效。");
      return;
    }
    setValidation("");
    setNotice("");
    reset.mutate();
  }
  async function refresh() {
    const latest = await detail.refetch();
    if (latest.data) {
      setSnapshot(latest.data);
      setRole(latest.data.platformRole);
      setConfirming(null);
      action.reset();
    }
  }
  return (
    <section className="workbench-panel" aria-label="管理用户">
      <header className="panel-heading">
        <div>
          <h2>管理用户 · {initialUser.displayName}</h2>
          <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
            {initialUser.id}
          </p>
        </div>
        <Button variant="ghost" onClick={onClose}>
          关闭
        </Button>
      </header>
      <div className="space-y-5 p-5 text-sm">
        {detail.error && (
          <QueryNotice
            error={detail.error}
            retry={() => void detail.refetch()}
          />
        )}
        {detail.data && (
          <p>
            当前状态：{detail.data.status === "active" ? "正常" : "已停用"} ·{" "}
            {detail.data.platformRole === "platform_admin"
              ? "平台管理员"
              : "普通用户"}
          </p>
        )}
        {changed && (
          <div
            role="alert"
            className="flex flex-wrap items-center gap-3 rounded border border-amber-300 bg-amber-50 p-3"
          >
            <span className="grow">
              列表或操作期间状态已变化，请先确认最新状态。
            </span>
            <Button variant="outline" size="sm" onClick={() => void refresh()}>
              使用最新状态
            </Button>
          </div>
        )}
        {notice && (
          <p role="status" className="rounded border bg-accent p-3">
            {notice}
          </p>
        )}
        <div className="space-y-3 border-t pt-5">
          <h3 className="font-semibold">平台角色与账号状态</h3>
          <div className="flex flex-wrap items-end gap-2">
            <div className="flex min-w-0 flex-col gap-2">
              <Label htmlFor="platform-role">平台角色</Label>
              <Select
                id="platform-role"
                className="h-10 text-sm"
                value={role}
                onValueChange={(value) => {
                  setRole(value as Role);
                  setConfirming(null);
                }}
              >
                <SelectItem value="user">普通用户</SelectItem>
                <SelectItem value="platform_admin">平台管理员</SelectItem>
              </Select>
            </div>
            <Button
              variant="outline"
              className="h-10 max-md:min-h-11"
              disabled={!!changed || role === detail.data?.platformRole}
              onClick={() => {
                setConfirming({ kind: "role", role });
                action.reset();
                reset.reset();
              }}
            >
              修改角色
            </Button>
            <Button
              variant="outline"
              className="h-10 max-md:min-h-11"
              disabled={!!changed || !detail.data}
              onClick={() => {
                setConfirming({
                  kind: detail.data!.status === "active" ? "disable" : "enable",
                });
                action.reset();
                reset.reset();
              }}
            >
              {detail.data?.status === "active" ? "停用账号" : "启用账号"}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              className="h-10 max-md:min-h-11"
              onClick={() => void refresh()}
            >
              刷新详情
            </Button>
          </div>
          {confirming && (
            <div className="flex flex-wrap items-center gap-3 rounded border bg-muted/40 p-3">
              <p className="grow">
                确认
                {confirming.kind === "role"
                  ? `将平台角色改为${confirming.role === "platform_admin" ? "管理员" : "普通用户"}`
                  : confirming.kind === "disable"
                    ? "停用账号并撤销会话"
                    : "启用账号"}
                ？
              </p>
              <Button
                variant="destructive"
                disabled={action.isPending || !!changed}
                onClick={() => action.mutate(confirming)}
              >
                确认提交
              </Button>
              <Button variant="ghost" onClick={() => setConfirming(null)}>
                取消
              </Button>
            </div>
          )}
          {action.error &&
            !(
              action.error instanceof ApiError &&
              action.error.code === "recent_authentication_required"
            ) && (
              <p role="alert" className="text-destructive">
                {securityError(action.error)}
                {!(action.error instanceof ApiError) &&
                  " 结果未确认，请先刷新详情。"}
              </p>
            )}
        </div>
        <form onSubmit={submitReset} className="space-y-3 border-t pt-5">
          <h3 className="font-semibold">重置本地密码</h3>
          <p className="text-xs text-muted-foreground">
            已有本地账号时，登录名留空。OIDC
            专用账号首次建立本地登录时填写新登录名。提交前请安全保存临时密码。
          </p>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="reset-login">新登录名（仅首次建立）</Label>
              <Input
                id="reset-login"
                autoComplete="off"
                value={newLoginName}
                onChange={(event) => setNewLoginName(event.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="reset-password">临时密码</Label>
              <Input
                id="reset-password"
                type="password"
                autoComplete="new-password"
                value={temporaryPassword}
                onChange={(event) => setTemporaryPassword(event.target.value)}
                required
              />
            </div>
          </div>
          {validation && (
            <p role="alert" className="text-destructive">
              {validation}
            </p>
          )}
          {reset.error &&
            !(
              reset.error instanceof ApiError &&
              reset.error.code === "recent_authentication_required"
            ) && (
              <p role="alert" className="text-destructive">
                {securityError(reset.error)}
                {!(reset.error instanceof ApiError) &&
                  " 结果未确认，请先核对账号。"}
              </p>
            )}
          <Button type="submit" variant="outline" disabled={reset.isPending}>
            重置密码
          </Button>
        </form>
        <RecentAuthPrompt error={action.error ?? reset.error} />
      </div>
    </section>
  );
}
