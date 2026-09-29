import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { getProject, getProjectPermissions } from "@/api/catalog";
import { errorText } from "@/api/http";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { Fact, QueryNotice, Timestamp } from "@/shared/OverviewUI";
import { useCommandKey } from "@/shared/use-command-key";
import { AccessRoutesPanel } from "./AccessRoutesPanel";
import {
  accessKeys,
  createHost,
  deleteHost,
  getHost,
  getHostOptions,
  getHostStatus,
  updateHost,
} from "./api";

type Host = components["schemas"]["AccessHost"];
type HostStatus = components["schemas"]["AccessHostStatus"];
type HostInput = components["schemas"]["AccessHostInput"];

export function AccessHostPage() {
  const { projectId = "", hostId = "" } = useParams();
  const creating = hostId === "new";
  const project = useQuery({
    queryKey: ["project", projectId],
    queryFn: () => getProject(projectId),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", projectId],
    queryFn: () => getProjectPermissions(projectId),
  });
  const host = useQuery({
    queryKey: accessKeys.host(projectId, hostId),
    queryFn: () => getHost(projectId, hostId),
    enabled: !creating,
  });
  if (
    project.isPending ||
    permissions.isPending ||
    (!creating && host.isPending)
  )
    return <LoadingPage label="正在加载访问域名" />;
  if (project.error)
    return (
      <ErrorPanel
        title="无法加载项目"
        error={project.error}
        onRetry={() => void project.refetch()}
      />
    );
  if (permissions.error)
    return (
      <ErrorPanel
        title="无法确认项目权限"
        error={permissions.error}
        onRetry={() => void permissions.refetch()}
      />
    );
  if (!creating && host.error)
    return (
      <ErrorPanel
        title="无法加载访问域名"
        error={host.error}
        onRetry={() => void host.refetch()}
      />
    );
  if (!creating && host.data?.projectId !== projectId)
    return (
      <EmptyState
        title="域名不属于该项目"
        description="请从所属项目重新进入。"
      />
    );
  if (creating) {
    if (!permissions.data.allowed.includes("manage_access_hosts"))
      return (
        <EmptyState
          title="当前角色不能创建访问域名"
          description="请联系项目管理员确认入口管理权限。"
        />
      );
    return <HostCreate projectId={projectId} projectName={project.data.name} />;
  }
  return (
    <HostDetail
      key={hostId}
      projectId={projectId}
      host={host.data!}
      canManageHost={permissions.data.allowed.includes("manage_access_hosts")}
      canManageRoutes={permissions.data.allowed.includes(
        "manage_access_routes",
      )}
    />
  );
}

function HostCreate({
  projectId,
  projectName,
}: {
  projectId: string;
  projectName: string;
}) {
  const [hostname, setHostname] = useState("");
  const [tlsMode, setTlsMode] = useState<"http_only" | "managed">("http_only");
  const [policyKey, setPolicyKey] = useState("");
  const [validation, setValidation] = useState("");
  const commandKey = useCommandKey();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const options = useQuery({
    queryKey: ["access-host-options", projectId],
    queryFn: () => getHostOptions(projectId),
  });
  const mutation = useMutation({
    mutationFn: (body: HostInput) =>
      createHost(
        projectId,
        body,
        commandKey.forPayload({ projectId, ...body }),
      ),
    onSuccess(created) {
      commandKey.clear();
      void queryClient.invalidateQueries({
        queryKey: ["access-hosts", projectId],
      });
      navigate(`/projects/${projectId}/access-hosts/${created.id}`);
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    const value = hostname.trim();
    if (!value || value.includes("/") || value.includes(":")) {
      setValidation("请输入完整域名，例如 payment.example.com。");
      return;
    }
    if (
      tlsMode === "managed" &&
      !options.data?.issuerPolicies.some((policy) => policy.key === policyKey)
    ) {
      setValidation("请选择当前可用的 Issuer Policy。");
      return;
    }
    setValidation("");
    mutation.mutate({
      hostname: value,
      tlsMode,
      ...(tlsMode === "managed" ? { issuerPolicyKey: policyKey } : {}),
    });
  }
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div>
          <Link
            className="text-xs text-muted-foreground hover:text-primary"
            to={`/projects/${projectId}?view=access`}
          >
            {projectName} / 访问入口
          </Link>
          <h1 className="mt-2">添加访问域名</h1>
        </div>
      </div>
      <section className="workbench-panel max-w-3xl">
        <header className="panel-heading">
          <h2>入口配置</h2>
        </header>
        <form onSubmit={submit} className="space-y-5 p-5 text-sm">
          <div className="space-y-2">
            <Label htmlFor="access-hostname">域名</Label>
            <Input
              id="access-hostname"
              value={hostname}
              onChange={(event) => setHostname(event.target.value)}
              placeholder="payment.example.com"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="access-tls-mode">TLS 模式</Label>
            <select
              id="access-tls-mode"
              className="runtime-select w-full"
              value={tlsMode}
              onChange={(event) => {
                setTlsMode(event.target.value as "http_only" | "managed");
                setPolicyKey("");
              }}
            >
              <option value="http_only">仅 HTTP</option>
              <option
                value="managed"
                disabled={!options.data?.issuerPolicies.length}
              >
                托管证书
              </option>
            </select>
          </div>
          {options.isPending || options.error ? (
            <QueryNotice
              error={options.error}
              retry={() => void options.refetch()}
            />
          ) : (
            <>
              {!options.data.issuerPolicies.length && (
                <p className="text-xs text-muted-foreground">
                  当前没有可用的托管证书 Policy；仍可创建 HTTP 入口。
                </p>
              )}
              {tlsMode === "managed" && (
                <div className="space-y-2">
                  <Label htmlFor="access-issuer">Issuer Policy</Label>
                  <select
                    id="access-issuer"
                    className="runtime-select w-full"
                    value={policyKey}
                    onChange={(event) => setPolicyKey(event.target.value)}
                  >
                    <option value="">请选择</option>
                    {options.data.issuerPolicies.map((policy) => (
                      <option key={policy.key} value={policy.key}>
                        {policy.key} · {policy.kind}/{policy.name}
                      </option>
                    ))}
                  </select>
                </div>
              )}
            </>
          )}
          <p className="text-muted-foreground">
            创建后可配置 PathPrefix 路由。证书控制器与 DNS
            验证在详情中单独观察。
          </p>
          {validation && (
            <p role="alert" className="text-destructive">
              {validation}
            </p>
          )}
          {mutation.error && (
            <p role="alert" className="text-destructive">
              {errorText(mutation.error)}
              。结果未知时，相同输入重试会复用幂等键。
            </p>
          )}
          <Button
            type="submit"
            disabled={
              mutation.isPending || (tlsMode === "managed" && !policyKey)
            }
          >
            {mutation.isPending ? "正在创建…" : "创建域名"}
          </Button>
        </form>
      </section>
    </div>
  );
}

function HostDetail({
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
  const queryClient = useQueryClient();
  const [deleteConfirm, setDeleteConfirm] = useState(false);
  const [deleteAccepted, setDeleteAccepted] = useState(false);
  const commandKey = useCommandKey();
  const current = useQuery({
    queryKey: accessKeys.host(projectId, host.id),
    queryFn: () => getHost(projectId, host.id),
    initialData: host,
  });
  const status = useQuery({
    queryKey: accessKeys.status(projectId, host.id),
    queryFn: () => getHostStatus(projectId, host.id),
    refetchInterval: (query) =>
      query.state.data?.sync.state !== "applied" ? 15_000 : false,
  });
  const remove = useMutation({
    mutationFn: () =>
      deleteHost(
        projectId,
        host.id,
        commandKey.forPayload({ projectId, hostId: host.id, action: "delete" }),
      ),
    onSuccess(updated) {
      commandKey.clear();
      queryClient.setQueryData(accessKeys.host(projectId, host.id), updated);
      void queryClient.invalidateQueries({
        queryKey: ["access-hosts", projectId],
      });
      void queryClient.invalidateQueries({
        queryKey: accessKeys.status(projectId, host.id),
      });
      setDeleteConfirm(false);
      setDeleteAccepted(true);
    },
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
          onClick={() => {
            void current.refetch();
            void status.refetch();
          }}
        >
          刷新入口状态
        </Button>
      </div>
      {deleteAccepted && (
        <p role="status" className="mb-5 rounded border bg-accent p-4 text-sm">
          清理已接纳。请继续观察入口调和与控制器状态。
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
          <Fact label="更新于">
            <Timestamp value={displayed.updatedAt} />
          </Fact>
        </dl>
      </section>
      {canManageHost && displayed.lifecycle === "active" && (
        <HostTlsEditor
          key={`${displayed.id}-${displayed.updatedAt}`}
          projectId={projectId}
          host={displayed}
        />
      )}
      <section className="workbench-panel mb-5">
        <header className="panel-heading">
          <h2>入口观测</h2>
          <span className="text-xs text-muted-foreground">
            独立观察，不包含公网探测
          </span>
        </header>
        {status.isPending || status.error ? (
          <QueryNotice
            error={status.error}
            retry={() => void status.refetch()}
          />
        ) : (
          <HostEvidence status={status.data} />
        )}
      </section>
      <AccessRoutesPanel
        projectId={projectId}
        host={displayed}
        canManage={canManageRoutes}
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

function HostTlsEditor({ projectId, host }: { projectId: string; host: Host }) {
  const [mode, setMode] = useState<Host["tlsMode"]>(host.tlsMode);
  const [policyKey, setPolicyKey] = useState(host.issuerPolicyKey ?? "");
  const [validation, setValidation] = useState("");
  const commandKey = useCommandKey();
  const queryClient = useQueryClient();
  const options = useQuery({
    queryKey: ["access-host-options", projectId],
    queryFn: () => getHostOptions(projectId),
  });
  const selectedPolicy = options.data?.issuerPolicies.find(
    (policy) => policy.key === policyKey,
  );
  const mutation = useMutation({
    mutationFn: (body: HostInput) =>
      updateHost(
        projectId,
        host.id,
        body,
        commandKey.forPayload({ hostId: host.id, ...body }),
      ),
    onSuccess(updated) {
      commandKey.clear();
      queryClient.setQueryData(accessKeys.host(projectId, host.id), updated);
      void queryClient.invalidateQueries({
        queryKey: accessKeys.status(projectId, host.id),
      });
      void queryClient.invalidateQueries({
        queryKey: ["access-hosts", projectId],
      });
      setValidation("");
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    if (mode === "existing_secret") return;
    if (mode === "managed" && !selectedPolicy) {
      setValidation("当前 Issuer Policy 不可用，请选择新 Policy。");
      return;
    }
    setValidation("");
    mutation.mutate({
      hostname: host.hostname,
      tlsMode: mode,
      ...(mode === "managed" ? { issuerPolicyKey: policyKey } : {}),
    });
  }
  return (
    <section className="workbench-panel mb-5 max-w-3xl">
      <header className="panel-heading">
        <h2>TLS 配置</h2>
      </header>
      <form onSubmit={submit} className="space-y-4 p-5 text-sm">
        <div className="space-y-2">
          <Label htmlFor="access-detail-tls-mode">模式</Label>
          <select
            id="access-detail-tls-mode"
            className="runtime-select w-full"
            value={mode}
            onChange={(event) => {
              setMode(event.target.value as Host["tlsMode"]);
              setPolicyKey("");
            }}
          >
            <option value="http_only">仅 HTTP</option>
            <option
              value="managed"
              disabled={!options.data?.issuerPolicies.length}
            >
              托管证书
            </option>
            {host.tlsMode === "existing_secret" && (
              <option value="existing_secret" disabled>
                已有 TLS Secret
              </option>
            )}
          </select>
        </div>
        {options.isPending || options.error ? (
          <QueryNotice
            error={options.error}
            retry={() => void options.refetch()}
          />
        ) : (
          <>
            {!options.data.issuerPolicies.length && (
              <p className="text-xs text-muted-foreground">
                当前没有可用的托管证书 Policy。
              </p>
            )}
            {mode === "managed" && (
              <div className="space-y-2">
                <Label htmlFor="access-detail-issuer">Issuer Policy</Label>
                <select
                  id="access-detail-issuer"
                  className="runtime-select w-full"
                  value={policyKey}
                  onChange={(event) => setPolicyKey(event.target.value)}
                >
                  <option value="">请选择</option>
                  {policyKey && !selectedPolicy && (
                    <option value={policyKey} disabled>
                      {policyKey}（已不可用）
                    </option>
                  )}
                  {options.data.issuerPolicies.map((policy) => (
                    <option key={policy.key} value={policy.key}>
                      {policy.key} · {policy.kind}/{policy.name}
                    </option>
                  ))}
                </select>
              </div>
            )}
          </>
        )}
        {mode === "managed" &&
          policyKey &&
          !options.isPending &&
          !selectedPolicy && (
            <p role="alert" className="text-amber-800">
              当前引用的 Issuer Policy 已不可用；请选择新 Policy。
            </p>
          )}
        {validation && (
          <p role="alert" className="text-destructive">
            {validation}
          </p>
        )}
        {mutation.error && (
          <p role="alert" className="text-destructive">
            {errorText(mutation.error)}。结果未知时，相同输入重试会复用幂等键。
          </p>
        )}
        {mutation.isSuccess && (
          <p role="status" className="text-emerald-700">
            配置已保存，等待入口与证书控制器调和。
          </p>
        )}
        <Button
          type="submit"
          disabled={
            mutation.isPending ||
            mode === "existing_secret" ||
            (mode === "managed" && !selectedPolicy)
          }
        >
          {mutation.isPending ? "正在保存…" : "保存 TLS 配置"}
        </Button>
      </form>
    </section>
  );
}

function HostEvidence({ status }: { status: HostStatus }) {
  const ready = (value: string) =>
    value === "ready"
      ? "就绪"
      : value === "not_ready"
        ? "未就绪"
        : value === "not_applicable"
          ? "不适用"
          : "未知";
  const sync = {
    pending: "待调和",
    applying: "应用中",
    applied: "已应用",
    retrying: "重试中",
    attention_required: "需要处理",
    deleting: "清理中",
  }[status.sync.state];
  const dns = {
    verified: "已验证",
    mismatch: "不匹配",
    unavailable: "不可用",
    not_configured: "未配置",
  }[status.dns.state];
  return (
    <div className="space-y-5 p-5 text-sm">
      <dl className="grid gap-5 sm:grid-cols-2">
        <Fact label="入口调和">
          {sync} · 修订 {status.sync.appliedRevision}/
          {status.sync.desiredRevision}
          {status.sync.lastErrorCode && (
            <span className="block text-destructive">
              {status.sync.lastErrorCode}
            </span>
          )}
        </Fact>
        <Fact label="Gateway / Listener">
          {ready(status.controller.gatewayState)} /{" "}
          {ready(status.controller.listenerState)}
        </Fact>
        <Fact label="证书 / Secret">
          {ready(status.controller.certificateState)} /{" "}
          {ready(status.controller.secretState)}
          {status.controller.certificateNotAfter && (
            <span className="block">
              有效期至{" "}
              <Timestamp value={status.controller.certificateNotAfter} />
            </span>
          )}
        </Fact>
        <Fact label="DNS 验证">
          {dns}
          {status.dns.errorCode && (
            <span className="block text-destructive">
              {status.dns.errorCode}
            </span>
          )}
        </Fact>
        <Fact label="Gateway 地址">
          {status.controller.addresses.join(", ") || "尚无地址"}
        </Fact>
        <Fact label="DNS 答案">
          {status.dns.answers.join(", ") || "尚无答案"}
        </Fact>
      </dl>
      {status.controller.routes.length > 0 && (
        <div>
          <h3 className="font-medium">HTTPRoute 控制器</h3>
          <ul className="mt-2 space-y-1 text-xs text-muted-foreground">
            {status.controller.routes.map((route) => (
              <li key={route.routeId}>
                {route.routeId} · Accepted {ready(route.accepted)} ·
                ResolvedRefs {ready(route.resolvedRefs)}
              </li>
            ))}
          </ul>
        </div>
      )}
      <p className="border-t pt-4 text-xs text-muted-foreground">
        Gateway/HTTPRoute、证书与 DNS 是不同观测。这里未主动验证公网 HTTP/HTTPS
        请求。
      </p>
    </div>
  );
}
