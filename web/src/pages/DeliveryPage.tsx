import type { FormEventHandler, ReactNode } from "react";
import type { UseFormReturn } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  Activity,
  AppWindow,
  Box,
  Boxes,
  CircleDot,
  Container,
  LayoutDashboard,
  Menu,
  MoreHorizontal,
  Network,
  Rocket,
  Search,
  Server,
  Settings,
  ShieldCheck,
  X,
} from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";
import {
  createDelivery,
  getOperation,
  getReleaseDiagnostics,
  type DeliveryAcceptance,
  type DeliveryInput,
  type Operation,
  type ReleaseDiagnosticReport,
} from "../api/client";

const readyImage =
  "registry.k8s.io/pause@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a";
const failingImage =
  "registry.invalid/orbitops/missing@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc";

const formSchema = z.object({
  projectName: z.string().trim().min(1, "请输入项目名称").max(100),
  projectSlug: z
    .string()
    .trim()
    .min(1, "请输入项目 Slug")
    .max(63)
    .regex(/^[a-z][a-z0-9-]*$/, "使用小写字母、数字和连字符，并以字母开头"),
  applicationName: z.string().trim().min(1, "请输入应用名称").max(100),
  applicationSlug: z
    .string()
    .trim()
    .min(1, "请输入应用 Slug")
    .max(63)
    .regex(/^[a-z][a-z0-9-]*$/, "使用小写字母、数字和连字符，并以字母开头"),
  replicas: z.coerce.number().int().min(1).max(5),
  containerPort: z.coerce.number().int().min(1).max(65535),
  imageReference: z
    .string()
    .trim()
    .regex(/^[^\s@]+@sha256:[a-f0-9]{64}$/, "请输入包含 sha256 Digest 的不可变镜像引用"),
});

type FormValues = z.input<typeof formSchema>;

type WorkspaceProps = {
  acceptance?: DeliveryAcceptance;
  error?: Error;
  form: UseFormReturn<FormValues, unknown, DeliveryInput>;
  mobileNavOpen: boolean;
  onCloseMobileNav: () => void;
  onSubmit: FormEventHandler<HTMLFormElement>;
  onToggleMobileNav: () => void;
  operation?: Operation;
  operationFetching: boolean;
  pending: boolean;
  diagnosticError: Error | null;
  diagnosticReport?: ReleaseDiagnosticReport;
  useImage: (image: string) => void;
};

export function DeliveryPage() {
  const [acceptance, setAcceptance] = useState<DeliveryAcceptance>();
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  const idempotency = useRef<{ fingerprint: string; key: string } | undefined>(undefined);
  const form = useForm<FormValues, unknown, DeliveryInput>({
    resolver: zodResolver(formSchema),
    defaultValues: {
      projectName: "Orbit 示例项目",
      projectSlug: "orbit-demo",
      applicationName: "演示应用",
      applicationSlug: "demo-app",
      replicas: 1,
      containerPort: 8080,
      imageReference: readyImage,
    },
  });
  const createMutation = useMutation({
    mutationFn: async (values: DeliveryInput) => {
      const fingerprint = JSON.stringify(values);
      if (idempotency.current?.fingerprint !== fingerprint) {
        idempotency.current = { fingerprint, key: crypto.randomUUID() };
      }
      return createDelivery(values, idempotency.current.key);
    },
    onSuccess: (result) => setAcceptance(result),
  });
  const operationQuery = useQuery({
    queryKey: ["operation", acceptance?.operation.id],
    queryFn: () => getOperation(acceptance!.operation.id),
    enabled: acceptance !== undefined,
    initialData: acceptance?.operation,
    refetchInterval: (query) => (isTerminal(query.state.data?.status) ? false : 500),
  });
  const operation = operationQuery.data ?? acceptance?.operation;
  const diagnosticQuery = useQuery({
    queryKey: ["release-diagnostics", acceptance?.release.id],
    queryFn: () => getReleaseDiagnostics(acceptance!.release.id),
    enabled: acceptance !== undefined,
    // 完整诊断需要同时读取 PostgreSQL 和多类 Kubernetes 资源，因此只做低频刷新。
    refetchInterval: acceptance === undefined || isTerminal(operation?.status) ? false : 4_000,
  });
  const terminalRefresh = useRef<string | undefined>(undefined);

  useEffect(() => {
    if (!acceptance || !isTerminal(operation?.status)) {
      return;
    }
    // Operation 首次进入终态时再取一次证据，随后停止自动轮询。
    if (terminalRefresh.current !== acceptance.operation.id) {
      terminalRefresh.current = acceptance.operation.id;
      void diagnosticQuery.refetch();
    }
  }, [acceptance, diagnosticQuery.refetch, operation?.status]);

  return (
    <Workspace
      acceptance={acceptance}
      error={createMutation.error instanceof Error ? createMutation.error : undefined}
      form={form}
      mobileNavOpen={mobileNavOpen}
      onCloseMobileNav={() => setMobileNavOpen(false)}
      onSubmit={form.handleSubmit((values) => createMutation.mutate(values))}
      onToggleMobileNav={() => setMobileNavOpen((open) => !open)}
      operation={operation}
      operationFetching={operationQuery.isFetching}
      pending={createMutation.isPending}
      diagnosticError={diagnosticQuery.error}
      diagnosticReport={diagnosticQuery.data}
      useImage={(image) => form.setValue("imageReference", image, { shouldDirty: true, shouldValidate: true })}
    />
  );
}

function Workspace(props: WorkspaceProps) {
  return (
    <div className="a-shell">
      {props.mobileNavOpen && <button className="sidebar-backdrop" type="button" aria-label="关闭导航" onClick={props.onCloseMobileNav} />}
      <aside className={`a-sidebar ${props.mobileNavOpen ? "mobile-open" : ""}`} aria-label="产品导航">
        <div className="sidebar-brand">
          <Brand />
          <button className="sidebar-close" type="button" aria-label="关闭导航" onClick={props.onCloseMobileNav}><X /></button>
        </div>
        <nav className="a-nav" aria-label="主导航">
          <NavSection label="工作台">
            <NavButton icon={<LayoutDashboard />} label="概览" />
            <NavButton active icon={<Rocket />} label="交付" />
            <NavButton icon={<Activity />} label="操作记录" />
          </NavSection>
          <NavSection label="资源">
            <NavButton icon={<AppWindow />} label="应用" />
            <NavButton icon={<Container />} label="运行目标" />
            <NavButton icon={<Server />} label="集群" suffix="1" />
          </NavSection>
          <NavSection label="系统">
            <NavButton icon={<Settings />} label="设置" />
          </NavSection>
        </nav>
        <div className="a-profile">
          <span className="avatar">HD</span>
          <span><strong>local-developer</strong><small>本地开发者</small></span>
          <MoreHorizontal size={16} />
        </div>
      </aside>

      <div className="a-workspace">
        <header className="a-topbar">
          <button className="icon-button mobile-only" type="button" aria-label="打开导航" aria-expanded={props.mobileNavOpen} onClick={props.onToggleMobileNav}><Menu /></button>
          <div className="breadcrumbs">
            <span>项目</span><b>/</b><strong>{props.acceptance?.project.name ?? "Orbit 示例项目"}</strong>
            <b>/</b><span>{props.acceptance?.application.name ?? "演示应用"}</span>
          </div>
          <div className="topbar-actions">
            <button className="icon-button" type="button" aria-label="搜索（后续切片开放）" disabled><Search /></button>
            <EnvironmentPill />
          </div>
        </header>

        <main className="a-main">
          <div className="page-heading">
            <div>
              <p className="overline">DELIVERY WORKSPACE</p>
              <h1>发布到开发环境</h1>
              <p>配置不可变制品，接纳发布，并观察 Kubernetes 的权威状态。</p>
            </div>
            <div className="heading-meta">
              <span>目标</span>
              <strong>development</strong>
              <small>kind-orbitops-s1 / orbitops-s1</small>
            </div>
          </div>

          <div className="a-layout">
            <form className="a-editor" onSubmit={props.onSubmit}>
              <EditorHeader index="01" title="交付配置" description="Project、Application 与运行目标" />
              <div className="form-section">
                <SectionTitle icon={<Boxes />} title="资源上下文" />
                <div className="form-grid two">
                  <FormField label="项目名称" error={props.form.formState.errors.projectName?.message}>
                    <input {...props.form.register("projectName")} />
                  </FormField>
                  <FormField label="项目标识" error={props.form.formState.errors.projectSlug?.message}>
                    <input className="mono" {...props.form.register("projectSlug")} />
                  </FormField>
                  <FormField label="应用名称" error={props.form.formState.errors.applicationName?.message}>
                    <input {...props.form.register("applicationName")} />
                  </FormField>
                  <FormField label="应用标识" error={props.form.formState.errors.applicationSlug?.message}>
                    <input className="mono" {...props.form.register("applicationSlug")} />
                  </FormField>
                </div>
              </div>
              <div className="form-section">
                <SectionTitle icon={<Server />} title="运行规格" />
                <div className="form-grid two">
                  <FormField label="副本数" error={props.form.formState.errors.replicas?.message} hint="1–5">
                    <input type="number" min="1" max="5" {...props.form.register("replicas")} />
                  </FormField>
                  <FormField label="容器端口" error={props.form.formState.errors.containerPort?.message} hint="ClusterIP">
                    <input className="mono" type="number" min="1" max="65535" {...props.form.register("containerPort")} />
                  </FormField>
                </div>
              </div>
              <div className="form-section">
                <SectionTitle icon={<Box />} title="不可变制品" />
                <FormField label="OCI 镜像引用" error={props.form.formState.errors.imageReference?.message} hint="必须包含 sha256 Digest">
                  <textarea className="mono" rows={3} spellCheck={false} {...props.form.register("imageReference")} />
                </FormField>
                <ImageExamples useImage={props.useImage} />
              </div>
              <MutationError error={props.error} />
              <div className="editor-actions">
                <span><ShieldCheck size={15} /> 仅写入本地受管 Namespace</span>
                <SubmitButton pending={props.pending} />
              </div>
            </form>

            <aside className="a-observation">
              <EditorHeader index="02" title="实时状态" description="Operation 与 Kubernetes 观测" />
              <OperationPanel operation={props.operation} fetching={props.operationFetching} />
              <RuntimePanel report={props.diagnosticReport} error={props.diagnosticError} />
              <IdentityPanel acceptance={props.acceptance} />
            </aside>
          </div>
        </main>
      </div>
    </div>
  );
}

function Brand() {
  return <div className="brand"><span className="brand-mark"><span /></span><span><strong>OrbitOps</strong><small>DELIVERY CONTROL</small></span></div>;
}

function NavSection({ label, children }: { label: string; children: ReactNode }) {
  return <div className="nav-section"><p>{label}</p>{children}</div>;
}

function NavButton({ active = false, icon, label, suffix }: { active?: boolean; icon: ReactNode; label: string; suffix?: string }) {
  return <button className={active ? "active" : ""} type="button" disabled={!active} title={active ? undefined : "后续切片开放"}><span>{icon}</span><b>{label}</b>{suffix && <i>{suffix}</i>}</button>;
}

function EnvironmentPill() {
  return <span className="environment-pill"><span /> 本地开发环境</span>;
}

function EditorHeader({ index, title, description }: { index: string; title: string; description: string }) {
  return <div className="editor-header"><span>{index}</span><div><h2>{title}</h2><p>{description}</p></div></div>;
}

function SectionTitle({ icon, title }: { icon: ReactNode; title: string }) {
  return <div className="section-title"><span>{icon}</span><h3>{title}</h3></div>;
}

function FormField({ label, error, hint, children }: { label: string; error?: string; hint?: string; children: ReactNode }) {
  return (
    <label className="form-field">
      <span><b>{label}</b>{hint && <small>{hint}</small>}</span>
      {children}
      {error && <em>{error}</em>}
    </label>
  );
}

function ImageExamples({ useImage }: { useImage: (image: string) => void }) {
  return <div className="image-examples"><button type="button" onClick={() => useImage(readyImage)}><CircleDot /> 可用镜像</button><button type="button" onClick={() => useImage(failingImage)}><CircleDot /> 模拟拉取失败</button></div>;
}

function SubmitButton({ pending }: { pending: boolean }) {
  return <button className="submit-button" type="submit" disabled={pending}><Rocket />{pending ? "正在接纳发布…" : "创建并发布"}<span>→</span></button>;
}

function MutationError({ error }: { error?: Error }) {
  if (!error) return null;
  return <div className="mutation-error" role="alert">{error.message}</div>;
}

function OperationPanel({ operation, fetching }: { operation?: Operation; fetching: boolean }) {
  const status = operation?.status ?? "idle";
  return (
    <section className="observation-block" aria-label="发布操作">
      <div className="block-heading"><span><Activity /> Operation</span><StatusBadge status={status} pulse={fetching && !isTerminal(status)} /></div>
      <div className="operation-id"><span>操作 ID</span><code>{operation?.id ?? "等待创建"}</code></div>
      <div className="attempt-line"><span>Attempt</span><strong>{operation?.attemptCount ?? 0}</strong><span>Worker</span><strong>{operation?.attempts.at(-1)?.workerId ?? "—"}</strong></div>
      {operation?.errorCode && <div className="error-summary"><b>{operation.errorCode}</b><p>{operation.errorSummary}</p></div>}
    </section>
  );
}

function RuntimePanel({ report, error }: { report?: ReleaseDiagnosticReport; error: Error | null }) {
  const workload = report?.workloadObservation;
  const deployment = workload?.deployment;
  const observationStatus = workload?.metadata.status;
  return (
    <section className="observation-block" aria-label="Kubernetes 实况">
      <div className="block-heading"><span><Server /> Kubernetes</span><span className={`freshness ${observationStatus ?? "idle"}`}>{observationStatusText(observationStatus)}</span></div>
      <div className="runtime-metrics"><Metric label="期望" value={deployment?.desiredReplicas ?? 0} /><Metric label="已更新" value={deployment?.updatedReplicas ?? 0} /><Metric label="就绪" value={deployment?.readyReplicas ?? 0} accent /></div>
      {workload?.pods.map((pod) => {
        const container = pod.containers.find((item) => item.reason !== "") ?? pod.containers[0];
        const reason = container?.reason || pod.reason || pod.phase;
        const restarts = pod.containers.reduce((total, item) => total + item.restartCount, 0);
        return <div className="runtime-pod" key={pod.name}><span><CircleDot /> Pod</span><code>{pod.name}</code><strong className={pod.ready ? "ready" : "failed"}>{reason}{restarts > 0 ? ` · 重启 ${restarts}` : ""}</strong></div>;
      })}
      <div className="runtime-detail"><span>Deployment</span><code>{deployment?.name ?? "等待首次发布"}</code><span>Release 关系</span><strong>{releaseRelationText(report?.runtimeReleaseRelation)}</strong><span>观测时间</span><strong>{workload ? formatTime(workload.metadata.observedAt) : "—"}</strong></div>
      {report?.signals.map((signal) => <div className="error-summary" key={`${signal.code}-${signal.summary}`}><b>{signal.code}</b><p>{signal.summary}</p></div>)}
      {error && <div className="error-summary"><b>runtime_unavailable</b><p>{error.message}</p></div>}
    </section>
  );
}

function IdentityPanel({ acceptance }: { acceptance?: DeliveryAcceptance }) {
  return <section className="identity-panel"><span><Network /> 稳定标识</span><dl><ResourceLine label="Project" value={shortID(acceptance?.project.id)} /><ResourceLine label="Application" value={shortID(acceptance?.application.id)} /><ResourceLine label="Release" value={acceptance?.release.id ?? "—"} /></dl></section>;
}

function StatusBadge({ status, pulse = false }: { status: string; pulse?: boolean }) {
  return <span className={`status status-${status}`} data-testid="operation-status"><i className={pulse ? "pulse" : ""} />{statusText(status)}</span>;
}

function Metric({ label, value, accent = false }: { label: string; value: number; accent?: boolean }) {
  return <div className={accent ? "metric accent" : "metric"}><strong>{value}</strong><span>{label}</span></div>;
}

function ResourceLine({ label, value }: { label: string; value: string }) {
  return <div><dt>{label}</dt><dd title={value}>{value}</dd></div>;
}

function isTerminal(status?: string): boolean {
  return status === "succeeded" || status === "failed";
}

function observationStatusText(status?: string): string {
  return { complete: "完整", partial: "部分可用", unavailable: "不可用" }[status ?? ""] ?? "等待";
}

function releaseRelationText(relation?: string): string {
  return { matches: "当前 Release", different: "其他 Release", absent: "尚未部署", unknown: "未知" }[relation ?? ""] ?? "等待";
}

function statusText(status: string): string {
  return { idle: "未开始", pending: "等待中", running: "执行中", succeeded: "已成功", failed: "已失败" }[status] ?? status;
}

function formatTime(value: string): string {
  return new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(new Date(value));
}

function shortID(value?: string): string {
  return value ? `${value.slice(0, 8)}…${value.slice(-4)}` : "—";
}
