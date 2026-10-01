import { Select, SelectItem } from "@/components/ui/select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiError, errorText } from "@/api/http";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { QueryNotice } from "@/shared/OverviewUI";
import { useCommandKey } from "@/shared/use-command-key";
import {
  addMember,
  listMembers,
  memberKeys,
  removeMember,
  resolveMember,
  updateMember,
} from "./api";

type Member = components["schemas"]["ProjectMember"];
type Role = components["schemas"]["ProjectRole"];
type LookupKind =
  components["schemas"]["ResolveProjectMemberCandidateRequest"]["kind"];

const roleNames: Record<Role, string> = {
  owner: "所有者",
  admin: "管理员",
  developer: "开发者",
  viewer: "观察者",
};

function memberError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.code === "last_project_owner")
      return "这是项目最后一位有效所有者，请先指定其他所有者。";
    if (error.code === "project_member_exists") return "该用户已是项目成员。";
    if (error.code === "invalid_project_member")
      return "该用户当前不能加入项目，请确认账号处于有效状态。";
  }
  return errorText(error);
}

export function ProjectMembersPanel({
  projectId,
  canManage,
  canManageOwners,
}: {
  projectId: string;
  canManage: boolean;
  canManageOwners: boolean;
}) {
  const [cursors, setCursors] = useState([""]);
  const [pageIndex, setPageIndex] = useState(0);
  const cursor = cursors[pageIndex] || undefined;
  const [kind, setKind] = useState<LookupKind>("login_name");
  const [value, setValue] = useState("");
  const [addRole, setAddRole] = useState<Role>("developer");
  const [editing, setEditing] = useState<Member | null>(null);
  const [editRole, setEditRole] = useState<Role>("developer");
  const [removing, setRemoving] = useState<Member | null>(null);
  const [notice, setNotice] = useState("");
  const addKey = useCommandKey();
  const updateKey = useCommandKey();
  const removeKey = useCommandKey();
  const queryClient = useQueryClient();
  const page = useQuery({
    queryKey: memberKeys.page(projectId, cursor),
    queryFn: () => listMembers(projectId, cursor),
  });
  const lookup = useMutation({
    mutationFn: () => resolveMember(projectId, { kind, value: value.trim() }),
  });
  function refreshMembers() {
    // 操作者也可能修改自己的角色或退出项目；命令完成前回读权限与项目可见性。
    return Promise.all([
      queryClient.invalidateQueries({
        queryKey: ["project-members", projectId],
      }),
      queryClient.invalidateQueries({
        queryKey: ["project-permissions", projectId],
      }),
      queryClient.invalidateQueries({ queryKey: ["project", projectId] }),
      queryClient.invalidateQueries({ queryKey: ["projects"] }),
    ]);
  }
  const add = useMutation({
    mutationFn: () =>
      addMember(
        projectId,
        lookup.data!.candidate!.userId,
        addRole,
        addKey.forPayload({
          projectId,
          userId: lookup.data!.candidate!.userId,
          role: addRole,
        }),
      ),
    onSuccess() {
      addKey.clear();
      lookup.reset();
      setValue("");
      setNotice("成员已添加。");
      setPageIndex(0);
      setCursors([""]);
      return refreshMembers();
    },
  });
  const update = useMutation({
    mutationFn: () =>
      updateMember(
        projectId,
        editing!.userId,
        editRole,
        updateKey.forPayload({
          projectId,
          userId: editing!.userId,
          role: editRole,
        }),
      ),
    onSuccess() {
      updateKey.clear();
      setEditing(null);
      setNotice("成员角色已更新。");
      return refreshMembers();
    },
  });
  const remove = useMutation({
    mutationFn: () =>
      removeMember(
        projectId,
        removing!.userId,
        removeKey.forPayload({ projectId, userId: removing!.userId }),
      ),
    onSuccess() {
      removeKey.clear();
      setRemoving(null);
      setNotice("成员已移除。");
      return refreshMembers();
    },
  });
  const pending = add.isPending || update.isPending || remove.isPending;
  function search(event: FormEvent) {
    event.preventDefault();
    if (!canManage || pending) return;
    setNotice("");
    add.reset();
    if (value.trim()) lookup.mutate();
  }
  function changePage(next: number, nextCursor?: string) {
    if (next > pageIndex && nextCursor)
      setCursors((previous) => [
        ...previous.slice(0, pageIndex + 1),
        nextCursor,
      ]);
    setPageIndex(next);
    setEditing(null);
    setRemoving(null);
  }
  return (
    <section className="workbench-panel mt-5" aria-label="项目成员">
      <header className="panel-heading">
        <div>
          <h2>项目成员</h2>
          <p className="mt-1 text-xs text-muted-foreground">
            按项目角色管理访问权限
          </p>
        </div>
      </header>
      {notice && (
        <p role="status" className="mx-5 mt-5 text-sm text-emerald-700">
          {notice}
        </p>
      )}
      {canManage && (
        <form
          onSubmit={search}
          className="grid gap-3 border-b p-5 md:grid-cols-[11rem_1fr_auto] md:items-end"
        >
          <div className="space-y-2">
            <Label htmlFor="member-kind">查找方式</Label>
            <Select
              id="member-kind"
              className="h-10 w-full rounded-md border border-input bg-background px-3 text-sm"
              value={kind}
              disabled={pending}
              onValueChange={(value) => {
                setKind(value as LookupKind);
                lookup.reset();
              }}
            >
              <SelectItem value="login_name">登录名</SelectItem>
              <SelectItem value="verified_email">已验证邮箱</SelectItem>
              <SelectItem value="user_id">User ID</SelectItem>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="member-value">精确查找用户</Label>
            <Input
              id="member-value"
              value={value}
              maxLength={320}
              disabled={pending}
              onChange={(event) => {
                setValue(event.target.value);
                lookup.reset();
              }}
              placeholder={
                kind === "login_name"
                  ? "输入完整登录名"
                  : kind === "verified_email"
                    ? "输入完整邮箱"
                    : "输入完整 User ID"
              }
            />
          </div>
          <Button
            type="submit"
            variant="outline"
            disabled={!value.trim() || lookup.isPending || pending}
          >
            查找
          </Button>
        </form>
      )}
      {lookup.error && (
        <p role="alert" className="px-5 pt-4 text-sm text-destructive">
          {memberError(lookup.error)}
        </p>
      )}
      {lookup.data?.status === "not_found" && (
        <p className="px-5 pt-4 text-sm text-muted-foreground">
          未找到有效用户。
        </p>
      )}
      {lookup.data?.status === "ambiguous" && (
        <p className="px-5 pt-4 text-sm text-amber-700">
          该邮箱对应多个用户，请使用登录名或 User ID 查找。
        </p>
      )}
      {canManage && lookup.data?.candidate && (
        <div className="flex flex-wrap items-end justify-between gap-4 border-b bg-muted/30 p-5 text-sm">
          <div>
            <p className="font-medium">{lookup.data.candidate.displayName}</p>
            <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
              {lookup.data.candidate.userId}
            </p>
          </div>
          <div className="flex flex-wrap items-end gap-2">
            <div className="space-y-2">
              <Label htmlFor="member-add-role">项目角色</Label>
              <RoleSelect
                id="member-add-role"
                value={addRole}
                onChange={setAddRole}
                canManageOwners={canManageOwners}
                disabled={pending}
              />
            </div>
            <Button disabled={pending} onClick={() => add.mutate()}>
              添加成员
            </Button>
          </div>
          {add.error && (
            <p role="alert" className="w-full text-destructive">
              {memberError(add.error)}
            </p>
          )}
        </div>
      )}
      {page.isPending || page.error ? (
        <QueryNotice error={page.error} retry={() => void page.refetch()} />
      ) : (
        <>
          {page.data.items.length ? (
            <div className="divide-y">
              {page.data.items.map((member) => (
                <article
                  key={member.userId}
                  className="flex flex-wrap items-center justify-between gap-3 p-5 text-sm"
                >
                  <div>
                    <p className="font-medium">
                      {member.displayName || member.userId}{" "}
                      <span className="ml-2 rounded bg-muted px-2 py-0.5 text-xs text-muted-foreground">
                        {roleNames[member.role]}
                      </span>
                    </p>
                    <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
                      {member.userId}
                    </p>
                  </div>
                  {canManage &&
                    (member.role !== "owner" || canManageOwners) && (
                      <div className="flex gap-2">
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={pending}
                          onClick={() => {
                            setEditing(member);
                            setEditRole(member.role);
                            update.reset();
                            setRemoving(null);
                          }}
                        >
                          修改角色
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={pending}
                          onClick={() => {
                            setRemoving(member);
                            remove.reset();
                            setEditing(null);
                          }}
                        >
                          移除
                        </Button>
                      </div>
                    )}
                </article>
              ))}
            </div>
          ) : (
            <p className="p-5 text-sm text-muted-foreground">暂无成员</p>
          )}
          {(pageIndex > 0 || page.data.nextCursor) && (
            <div className="flex items-center justify-end gap-2 border-t p-4 text-sm">
              <span className="mr-2 text-muted-foreground">
                第 {pageIndex + 1} 页
              </span>
              <Button
                size="sm"
                variant="outline"
                disabled={pageIndex === 0}
                onClick={() => changePage(pageIndex - 1)}
              >
                上一页
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={!page.data.nextCursor}
                onClick={() => changePage(pageIndex + 1, page.data.nextCursor)}
              >
                下一页
              </Button>
            </div>
          )}
        </>
      )}
      {canManage &&
        editing &&
        (editing.role !== "owner" || canManageOwners) && (
          <div className="flex flex-wrap items-end gap-3 border-t bg-muted/30 p-5 text-sm">
            <div className="space-y-2">
              <Label htmlFor="member-edit-role">
                修改 {editing.displayName || editing.userId} 的角色
              </Label>
              <RoleSelect
                id="member-edit-role"
                value={editRole}
                onChange={setEditRole}
                canManageOwners={canManageOwners}
                disabled={pending}
              />
            </div>
            <Button
              disabled={pending || editRole === editing.role}
              onClick={() => update.mutate()}
            >
              保存角色
            </Button>
            <Button
              variant="ghost"
              disabled={pending}
              onClick={() => setEditing(null)}
            >
              取消
            </Button>
            {update.error && (
              <p role="alert" className="w-full text-destructive">
                {memberError(update.error)}
              </p>
            )}
          </div>
        )}
      {canManage &&
        removing &&
        (removing.role !== "owner" || canManageOwners) && (
          <div className="flex flex-wrap items-center gap-3 border-t bg-muted/30 p-5 text-sm">
            <p className="grow">
              确认移除 {removing.displayName || removing.userId}
              ？该用户将失去此项目的访问权限。
            </p>
            <Button
              variant="destructive"
              disabled={pending}
              onClick={() => remove.mutate()}
            >
              确认移除
            </Button>
            <Button
              variant="ghost"
              disabled={pending}
              onClick={() => setRemoving(null)}
            >
              取消
            </Button>
            {remove.error && (
              <p role="alert" className="w-full text-destructive">
                {memberError(remove.error)}
              </p>
            )}
          </div>
        )}
    </section>
  );
}

function RoleSelect({
  id,
  value,
  onChange,
  canManageOwners,
  disabled,
}: {
  id: string;
  value: Role;
  onChange: (role: Role) => void;
  canManageOwners: boolean;
  disabled: boolean;
}) {
  return (
    <Select
      id={id}
      className="h-10 min-w-32 rounded-md border border-input bg-background px-3 text-sm"
      value={value}
      disabled={disabled}
      onValueChange={(value) => onChange(value as Role)}
    >
      {canManageOwners && <SelectItem value="owner">所有者</SelectItem>}
      <SelectItem value="admin">管理员</SelectItem>
      <SelectItem value="developer">开发者</SelectItem>
      <SelectItem value="viewer">观察者</SelectItem>
    </Select>
  );
}
