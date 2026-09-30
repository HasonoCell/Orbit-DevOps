import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiError, errorText } from "@/api/http";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RecentAuthPrompt } from "@/features/auth/RecentAuthPrompt";
import { QueryNotice, Timestamp } from "@/shared/OverviewUI";
import { createLocalUser, listUsers } from "./api";
import { UserDetailPanel } from "./UserDetailPanel";
import { temporaryPasswordValid } from "./password-policy";

type User = components["schemas"]["User"];

export function UsersPanel() {
  const [cursors, setCursors] = useState([""]);
  const [pageIndex, setPageIndex] = useState(0);
  const [selected, setSelected] = useState<User | null>(null);
  const [loginName, setLoginName] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [temporaryPassword, setTemporaryPassword] = useState("");
  const [validation, setValidation] = useState("");
  const [notice, setNotice] = useState("");
  const cursor = cursors[pageIndex] || undefined;
  const queryClient = useQueryClient();
  const page = useQuery({
    queryKey: ["platform-users", cursor],
    queryFn: () => listUsers(cursor),
    retry: false,
  });
  const create = useMutation({
    mutationFn: () =>
      createLocalUser({
        loginName: loginName.trim().toLowerCase(),
        displayName: displayName.trim(),
        temporaryPassword,
      }),
    onSuccess(user) {
      setTemporaryPassword("");
      setValidation("");
      setNotice(
        `已创建 ${user.displayName}。临时密码不会再次显示，用户首次登录后需改密。`,
      );
      void queryClient.invalidateQueries({ queryKey: ["platform-users"] });
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    if (
      !/^[a-z0-9][a-z0-9._@+-]{2,127}$/.test(loginName.trim().toLowerCase()) ||
      !displayName.trim()
    ) {
      setValidation("请填写合法登录名和显示名称。");
      return;
    }
    if (!temporaryPasswordValid(temporaryPassword)) {
      setValidation("临时密码需为 15–128 个字符，UTF-8 不超过 512 字节。");
      return;
    }
    setValidation("");
    setNotice("");
    create.mutate();
  }
  return (
    <div className="space-y-5">
      <section className="workbench-panel max-w-3xl">
        <header className="panel-heading">
          <h2>创建本地用户</h2>
        </header>
        <form
          onSubmit={submit}
          className="grid gap-4 p-5 text-sm sm:grid-cols-2"
        >
          <div className="space-y-2">
            <Label htmlFor="user-login">登录名</Label>
            <Input
              id="user-login"
              autoComplete="off"
              value={loginName}
              onChange={(event) => setLoginName(event.target.value)}
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="user-name">显示名称</Label>
            <Input
              id="user-name"
              value={displayName}
              onChange={(event) => setDisplayName(event.target.value)}
              required
            />
          </div>
          <div className="space-y-2 sm:col-span-2">
            <Label htmlFor="user-password">临时密码</Label>
            <Input
              id="user-password"
              type="password"
              autoComplete="new-password"
              value={temporaryPassword}
              onChange={(event) => setTemporaryPassword(event.target.value)}
              required
            />
            <p className="text-xs text-muted-foreground">
              提交前请通过安全渠道保存临时密码；成功后不再显示。
            </p>
          </div>
          {validation && (
            <p role="alert" className="sm:col-span-2 text-destructive">
              {validation}
            </p>
          )}
          {create.error &&
            !(
              create.error instanceof ApiError &&
              create.error.code === "recent_authentication_required"
            ) && (
              <p role="alert" className="sm:col-span-2 text-destructive">
                {create.error instanceof ApiError &&
                create.error.code === "login_name_conflict"
                  ? "登录名已被占用。"
                  : errorText(create.error)}
                {!(create.error instanceof ApiError) &&
                  " 结果尚未确认，请先核对用户列表。"}
              </p>
            )}
          <div className="sm:col-span-2">
            <Button type="submit" disabled={create.isPending}>
              创建用户
            </Button>
          </div>
        </form>
        <div className="px-5 pb-5">
          <RecentAuthPrompt error={create.error} />
        </div>
      </section>
      {notice && (
        <p role="status" className="rounded border bg-accent p-4 text-sm">
          {notice}
        </p>
      )}
      <section className="workbench-panel">
        <header className="panel-heading">
          <h2>平台用户</h2>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void page.refetch()}
          >
            刷新
          </Button>
        </header>
        {page.isPending || page.error ? (
          <div className="p-5">
            <QueryNotice error={page.error} retry={() => void page.refetch()} />
          </div>
        ) : (
          <>
            {page.data.items.length ? (
              <div className="divide-y">
                {page.data.items.map((user) => (
                  <article
                    key={user.id}
                    className="flex flex-wrap items-center justify-between gap-3 p-5 text-sm"
                  >
                    <div>
                      <p className="font-medium">
                        {user.displayName}{" "}
                        <span className="ml-2 rounded bg-muted px-2 py-0.5 text-xs">
                          {user.platformRole === "platform_admin"
                            ? "平台管理员"
                            : "普通用户"}
                        </span>{" "}
                        <span className="text-xs text-muted-foreground">
                          {user.status === "active" ? "正常" : "已停用"}
                        </span>
                      </p>
                      <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
                        {user.id}
                      </p>
                    </div>
                    <div className="flex items-center gap-3">
                      <Timestamp value={user.createdAt} />
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => setSelected(user)}
                      >
                        管理
                      </Button>
                    </div>
                  </article>
                ))}
              </div>
            ) : (
              <p className="p-5 text-sm text-muted-foreground">暂无用户</p>
            )}
            {(pageIndex > 0 || page.data.nextCursor) && (
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
      </section>
      {selected && (
        <UserDetailPanel
          key={selected.id}
          initialUser={selected}
          onClose={() => setSelected(null)}
        />
      )}
    </div>
  );
}
