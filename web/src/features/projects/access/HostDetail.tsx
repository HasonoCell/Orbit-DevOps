import { errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Fact, QueryNotice, Timestamp } from "@/shared/OverviewUI";
import { useObservationWindow } from "@/shared/use-observation-window";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";
import { AccessRoutesPanel } from "./AccessRoutesPanel";
import { accessQueries } from "./api";
import { useDeleteHost } from "./mutations";

import { HostEvidence } from "./HostEvidence";
import { HostTlsEditor } from "./HostTlsEditor";
import type { Host } from "./types";

/** 组合 Host 配置、路由和独立控制器观测，删除命令不冒充清理完成。 */
export function HostDetail({
  projectId,
  host,
  canManageHost,
  canManageRoutes,
}: {
  projectId: string;
  host: Host;
  canManageHost: boolean;
  canManageRoutes: boolean;
}) {
  const [deleteConfirm, setDeleteConfirm] = useState(false);
  const [deleteAccepted, setDeleteAccepted] = useState(false);
  const { until: observeUntil, restart: restartObservation } =
    useObservationWindow(host.id);
  const current = useQuery({
    ...accessQueries.host(projectId, host.id),
    initialData: host,
  });
  const status = useQuery({
    ...accessQueries.status(projectId, host.id),
    // 数据库调和完成后，Gateway、证书和 DNS 仍可能继续变化。
    refetchInterval: (query) =>
      !query.state.error && Date.now() < observeUntil ? 15_000 : false,
    refetchIntervalInBackground: false,
  });
  function refresh() {
    restartObservation();
    void current.refetch();
    void status.refetch();
  }
  const remove = useDeleteHost(projectId, host.id, () => {
    restartObservation();
    setDeleteConfirm(false);
    setDeleteAccepted(true);
  });
  const displayed = current.data ?? host;
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div className="min-w-0">
          <Link
            className="text-xs text-muted-foreground hover:text-primary"
            to={`/projects/${projectId}?view=access`}
          >
            项目 / 访问入口
          </Link>
          <h1 className="mt-2 break-all">{displayed.hostname}</h1>
        </div>
        <Button
          variant="outline"
          disabled={current.isFetching || status.isFetching}
          onClick={refresh}
        >
          刷新入口状态
        </Button>
      </div>
      {deleteAccepted && (
        <p role="status" className="mb-5 rounded border bg-accent p-4 text-sm">
          已提交清理，等待控制器完成。
        </p>
      )}
      {displayed.tlsMode === "existing_secret" &&
        displayed.secretBindingState === "revoked" && (
          <p
            role="alert"
            className="mb-5 rounded border border-amber-300 bg-amber-50 p-4 text-sm text-amber-950"
          >
            当前 TLS Secret 授权已撤销，HTTPS
            入口正在失效；请选择其他有效授权或切换 TLS 模式。
          </p>
        )}
      <section className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>域名配置</h2>
          <span className="text-xs">
            {displayed.lifecycle === "deleting" ? "清理中" : "已声明"}
          </span>
        </header>
        <dl className="grid gap-5 p-5 text-sm sm:grid-cols-2">
          <Fact label="集群 / Namespace">
            {displayed.clusterRef} / {displayed.namespace}
          </Fact>
          <Fact label="TLS 模式">
            {displayed.tlsMode === "http_only"
              ? "仅 HTTP"
              : displayed.tlsMode === "managed"
                ? "托管证书"
                : "已有 TLS Secret"}
          </Fact>
          {displayed.secretBindingId && (
            <Fact label="TLS Secret 授权">
              {displayed.secretBindingState === "revoked"
                ? "已撤销"
                : displayed.secretBindingState === "active"
                  ? "有效"
                  : "待核验"}{" "}
              · {displayed.secretBindingId}
            </Fact>
          )}
          <Fact label="更新于">
            <Timestamp value={displayed.updatedAt} />
          </Fact>
        </dl>
      </section>
      {canManageHost && displayed.lifecycle === "active" && (
        <HostTlsEditor
          key={displayed.id}
          projectId={projectId}
          host={displayed}
          onAccepted={restartObservation}
        />
      )}
      <section className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>入口观测</h2>
        </header>
        {status.isPending || status.error ? (
          <QueryNotice error={status.error} retry={refresh} />
        ) : (
          <HostEvidence status={status.data} />
        )}
      </section>
      <AccessRoutesPanel
        projectId={projectId}
        host={displayed}
        canManage={canManageRoutes}
        onAccepted={restartObservation}
      />
      {canManageHost && displayed.lifecycle === "active" && (
        <section className="workbench-panel mt-5 max-w-3xl p-5 text-sm">
          <h2 className="font-semibold">删除域名</h2>
          <p className="mt-2 text-muted-foreground">
            会清理该域名的受控入口资源，已存在的路由也将停止服务。
          </p>
          {deleteConfirm ? (
            <div className="mt-4 space-y-3 rounded border p-4">
              <p>确认删除 {displayed.hostname}？</p>
              {remove.error && (
                <p role="alert" className="text-destructive">
                  {errorText(remove.error)}
                </p>
              )}
              <div className="flex gap-2">
                <Button
                  variant="destructive"
                  disabled={remove.isPending}
                  onClick={() => remove.mutate()}
                >
                  {remove.isPending ? "正在提交…" : "确认删除"}
                </Button>
                <Button
                  variant="outline"
                  disabled={remove.isPending}
                  onClick={() => setDeleteConfirm(false)}
                >
                  返回
                </Button>
              </div>
            </div>
          ) : (
            <Button
              className="mt-4"
              variant="outline"
              onClick={() => setDeleteConfirm(true)}
            >
              删除域名
            </Button>
          )}
        </section>
      )}
    </div>
  );
}
