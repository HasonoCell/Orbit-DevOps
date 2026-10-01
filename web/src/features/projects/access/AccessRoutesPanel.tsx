import { Select, SelectItem } from "@/components/ui/select";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import type { components } from "@/api/schema";
import { errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { QueryNotice } from "@/shared/OverviewUI";
import { useCommandKey } from "@/shared/use-command-key";
import {
  accessKeys,
  createRoute,
  deleteRoute,
  listEligibleTargets,
  listRoutes,
  updateRoute,
} from "./api";

type Host = components["schemas"]["AccessHost"];
type Route = components["schemas"]["AccessRoute"];

export function AccessRoutesPanel({
  projectId,
  host,
  canManage,
  onAccepted,
}: {
  projectId: string;
  host: Host;
  canManage: boolean;
  onAccepted: () => void;
}) {
  const [offset, setOffset] = useState(0);
  const [targetOffset, setTargetOffset] = useState(0);
  const [editing, setEditing] = useState<Route | null>(null);
  const [pathPrefix, setPathPrefix] = useState("/");
  const [targetId, setTargetId] = useState("");
  const [validation, setValidation] = useState("");
  const [deleteId, setDeleteId] = useState<string | null>(null);
  const [cleanup, setCleanup] = useState("");
  const commandKey = useCommandKey();
  const deleteKey = useCommandKey();
  const queryClient = useQueryClient();
  const routes = useQuery({
    queryKey: accessKeys.routes(projectId, host.id, offset),
    queryFn: () => listRoutes(projectId, host.id, offset),
  });
  const targets = useQuery({
    queryKey: accessKeys.eligible(projectId, host.id, targetOffset),
    queryFn: () => listEligibleTargets(projectId, host.id, targetOffset),
    enabled: canManage && host.lifecycle === "active",
  });
  const visibleTargets = targets.data?.slice(0, 20) ?? [];
  const write = useMutation({
    mutationFn: (body: { pathPrefix: string; deploymentTargetId: string }) => {
      const key = commandKey.forPayload({
        hostId: host.id,
        routeId: editing?.id,
        ...body,
      });
      return editing
        ? updateRoute(projectId, host.id, editing.id, body, key)
        : createRoute(projectId, host.id, body, key);
    },
    onSuccess() {
      commandKey.clear();
      onAccepted();
      setEditing(null);
      setPathPrefix("/");
      setTargetId("");
      setValidation("");
      void queryClient.invalidateQueries({
        queryKey: ["access-routes", projectId, host.id],
      });
      void queryClient.invalidateQueries({
        queryKey: accessKeys.status(projectId, host.id),
      });
      void queryClient.invalidateQueries({
        queryKey: ["target-access-routes"],
      });
    },
  });
  const remove = useMutation({
    mutationFn: (routeId: string) =>
      deleteRoute(
        projectId,
        host.id,
        routeId,
        deleteKey.forPayload({ hostId: host.id, routeId, action: "delete" }),
      ),
    onSuccess(updated) {
      deleteKey.clear();
      onAccepted();
      setDeleteId(null);
      setCleanup(updated.id);
      void queryClient.invalidateQueries({
        queryKey: ["access-routes", projectId, host.id],
      });
      void queryClient.invalidateQueries({
        queryKey: accessKeys.status(projectId, host.id),
      });
      void queryClient.invalidateQueries({
        queryKey: ["target-access-routes"],
      });
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    const path = pathPrefix.trim();
    if (
      !path.startsWith("/") ||
      path.includes("?") ||
      path.includes("#") ||
      path.includes("//")
    ) {
      setValidation("PathPrefix 必须以 / 开头，不能包含查询或片段。");
      return;
    }
    if (!targetId) {
      setValidation("请选择同项目、同集群和 Namespace 的 Target。");
      return;
    }
    setValidation("");
    write.mutate({ pathPrefix: path, deploymentTargetId: targetId });
  }
  function edit(route: Route) {
    write.reset();
    setEditing(route);
    setPathPrefix(route.pathPrefix);
    setTargetId(route.deploymentTargetId);
  }
  return (
    <section className="workbench-panel">
      <header className="panel-heading">
        <h2>路径路由</h2>
        <span className="text-xs text-muted-foreground">
          PathPrefix → Target Service
        </span>
      </header>
      {cleanup && (
        <p role="status" className="m-5 rounded border bg-accent p-3 text-sm">
          路由 {cleanup} 清理已接纳，等待控制器完成。
        </p>
      )}
      {routes.isPending || routes.error ? (
        <QueryNotice error={routes.error} retry={() => void routes.refetch()} />
      ) : (
        <>
          {routes.data.length ? (
            <div className="divide-y">
              {routes.data.slice(0, 20).map((route) => (
                <article
                  key={route.id}
                  className="flex flex-wrap items-center justify-between gap-3 p-5 text-sm"
                >
                  <div>
                    <p className="font-mono font-medium">{route.pathPrefix}</p>
                    <p className="mt-1 break-all text-xs text-muted-foreground">
                      Target {route.deploymentTargetId} ·{" "}
                      {route.lifecycle === "deleting" ? "清理中" : "已声明"}
                    </p>
                  </div>
                  {canManage &&
                    host.lifecycle === "active" &&
                    route.lifecycle === "active" && (
                      <div className="flex gap-2">
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => edit(route)}
                        >
                          修改
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => {
                            remove.reset();
                            setDeleteId(route.id);
                          }}
                        >
                          删除
                        </Button>
                      </div>
                    )}
                </article>
              ))}
            </div>
          ) : (
            <p className="p-5 text-sm text-muted-foreground">暂无路径路由</p>
          )}
          {(offset > 0 || routes.data.length > 20) && (
            <div className="flex justify-end gap-2 border-t p-4">
              <Button
                variant="outline"
                disabled={offset === 0}
                onClick={() => setOffset(Math.max(0, offset - 20))}
              >
                上一页
              </Button>
              <Button
                variant="outline"
                disabled={routes.data.length <= 20 || offset >= 9980}
                onClick={() => setOffset(offset + 20)}
              >
                下一页
              </Button>
            </div>
          )}
        </>
      )}
      {deleteId && (
        <div className="m-5 space-y-3 rounded border p-4 text-sm">
          <p>确认清理路由 {deleteId}？</p>
          {remove.error && (
            <p role="alert" className="text-destructive">
              {errorText(remove.error)}
            </p>
          )}
          <div className="flex gap-2">
            <Button
              variant="destructive"
              disabled={remove.isPending}
              onClick={() => remove.mutate(deleteId)}
            >
              {remove.isPending ? "正在提交…" : "确认删除路由"}
            </Button>
            <Button
              variant="outline"
              disabled={remove.isPending}
              onClick={() => setDeleteId(null)}
            >
              返回
            </Button>
          </div>
        </div>
      )}
      {canManage && host.lifecycle === "active" && (
        <form onSubmit={submit} className="space-y-4 border-t p-5 text-sm">
          <h3 className="font-semibold">
            {editing ? `修改路由 ${editing.pathPrefix}` : "添加路由"}
          </h3>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="access-path">PathPrefix</Label>
              <Input
                id="access-path"
                value={pathPrefix}
                onChange={(event) => setPathPrefix(event.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="access-target">部署目标</Label>
              <Select
                id="access-target"
                className="w-full"
                value={targetId}
                onValueChange={(value) => setTargetId(value)}
              >
                <SelectItem value="">请选择 Target</SelectItem>
                {targetId &&
                  !visibleTargets.some((item) => item.id === targetId) && (
                    <SelectItem value={targetId}>
                      已选 Target {targetId}
                    </SelectItem>
                  )}
                {visibleTargets.map((item) => (
                  <SelectItem key={item.id} value={item.id}>
                    {item.applicationName} · {item.stage} ·{" "}
                    {item.id.slice(0, 8)}
                  </SelectItem>
                ))}
              </Select>
            </div>
          </div>
          {targets.isPending || targets.error ? (
            <QueryNotice
              error={targets.error}
              retry={() => void targets.refetch()}
            />
          ) : (
            <>
              {!targets.data.length && (
                <p className="text-muted-foreground">
                  同集群和 Namespace 下没有可选 Target。
                </p>
              )}
              {(targetOffset > 0 || targets.data.length > 20) && (
                <div className="flex gap-2">
                  <Button
                    type="button"
                    variant="outline"
                    disabled={targetOffset === 0}
                    onClick={() =>
                      setTargetOffset(Math.max(0, targetOffset - 20))
                    }
                  >
                    上一组 Target
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    disabled={targets.data.length <= 20 || targetOffset >= 9980}
                    onClick={() => setTargetOffset(targetOffset + 20)}
                  >
                    下一组 Target
                  </Button>
                </div>
              )}
            </>
          )}
          <p className="text-xs text-muted-foreground">
            目标 Service 的端口由 Orbit 读取；实际是否被 HTTPRoute
            控制器接受，请看上方入口观测。
          </p>
          {validation && (
            <p role="alert" className="text-destructive">
              {validation}
            </p>
          )}
          {write.error && (
            <p role="alert" className="text-destructive">
              {errorText(write.error)}。结果未知时相同输入重试会复用幂等键。
            </p>
          )}
          <div className="flex gap-2">
            <Button type="submit" disabled={write.isPending || !targetId}>
              {write.isPending
                ? "正在保存…"
                : editing
                  ? "保存路由"
                  : "创建路由"}
            </Button>
            {editing && (
              <Button
                type="button"
                variant="outline"
                disabled={write.isPending}
                onClick={() => {
                  setEditing(null);
                  setTargetId("");
                  setPathPrefix("/");
                }}
              >
                取消修改
              </Button>
            )}
          </div>
        </form>
      )}
    </section>
  );
}
