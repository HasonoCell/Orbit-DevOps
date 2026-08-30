import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";
import {
  createDelivery,
  getOperation,
  getRuntimeSnapshot,
  type DeliveryAcceptance,
  type DeliveryInput,
  type Operation,
  type RuntimeSnapshot,
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

export function DeliveryPage() {
  const [acceptance, setAcceptance] = useState<DeliveryAcceptance>();
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
  const runtimeQuery = useQuery({
    queryKey: ["runtime", acceptance?.target.id],
    queryFn: () => getRuntimeSnapshot(acceptance!.target.id),
    enabled: acceptance !== undefined,
    refetchInterval: acceptance === undefined ? false : 1_000,
  });
  const currentOperation = operationQuery.data ?? acceptance?.operation;
  const stageLabel = useMemo(
    () => (acceptance === undefined ? "等待创建" : "development"),
    [acceptance],
  );

  return (
    <div className="min-h-screen bg-[var(--canvas)] text-slate-100">
      <div className="ambient" aria-hidden="true" />
      <header className="mx-auto flex max-w-7xl items-center justify-between px-6 py-7 lg:px-10">
        <div className="flex items-center gap-3">
          <div className="grid h-10 w-10 place-items-center rounded-xl border border-cyan-300/30 bg-cyan-300/10 font-mono text-sm font-bold text-cyan-200">
            OO
          </div>
          <div>
            <p className="font-mono text-xs tracking-[0.24em] text-cyan-300">ORBITOPS</p>
            <p className="text-sm text-slate-400">本地交付控制台</p>
          </div>
        </div>
        <div className="flex items-center gap-2 rounded-full border border-emerald-400/20 bg-emerald-400/10 px-3 py-1.5 text-xs text-emerald-200">
          <span className="h-1.5 w-1.5 rounded-full bg-emerald-300 shadow-[0_0_10px_#6ee7b7]" />
          本地开发边界
        </div>
      </header>

      <main className="mx-auto grid max-w-7xl gap-6 px-6 pb-12 lg:grid-cols-[minmax(0,1.08fr)_minmax(380px,0.92fr)] lg:px-10">
        <section className="panel p-6 sm:p-8">
          <div className="mb-8 flex items-start justify-between gap-4">
            <div>
              <p className="eyebrow">新建交付</p>
              <h1 className="mt-2 text-3xl font-semibold tracking-tight text-white sm:text-4xl">
                把不可变镜像送入 Kubernetes
              </h1>
              <p className="mt-3 max-w-2xl text-sm leading-6 text-slate-400">
                一次建立项目、应用和开发目标。API 接纳发布后，Worker 会独立完成资源调和与状态回读。
              </p>
            </div>
            <span className="stage-chip">{stageLabel}</span>
          </div>

          <form className="space-y-7" onSubmit={form.handleSubmit((values) => createMutation.mutate(values))}>
            <FormSection number="01" title="项目与应用">
              <div className="field-grid">
                <Field label="项目名称" error={form.formState.errors.projectName?.message}>
                  <input {...form.register("projectName")} />
                </Field>
                <Field label="项目 Slug" error={form.formState.errors.projectSlug?.message}>
                  <input {...form.register("projectSlug")} />
                </Field>
                <Field label="应用名称" error={form.formState.errors.applicationName?.message}>
                  <input {...form.register("applicationName")} />
                </Field>
                <Field label="应用 Slug" error={form.formState.errors.applicationSlug?.message}>
                  <input {...form.register("applicationSlug")} />
                </Field>
              </div>
            </FormSection>

            <FormSection number="02" title="运行目标">
              <div className="field-grid">
                <Field label="副本数" error={form.formState.errors.replicas?.message}>
                  <input type="number" min="1" max="5" {...form.register("replicas")} />
                </Field>
                <Field label="容器端口" error={form.formState.errors.containerPort?.message}>
                  <input type="number" min="1" max="65535" {...form.register("containerPort")} />
                </Field>
              </div>
            </FormSection>

            <FormSection number="03" title="不可变制品">
              <Field label="OCI 镜像引用" error={form.formState.errors.imageReference?.message}>
                <textarea rows={3} spellCheck={false} {...form.register("imageReference")} />
              </Field>
              <div className="mt-3 flex flex-wrap gap-2">
                <button className="example-button" type="button" onClick={() => form.setValue("imageReference", readyImage, { shouldDirty: true, shouldValidate: true })}>
                  使用本地可用镜像
                </button>
                <button className="example-button" type="button" onClick={() => form.setValue("imageReference", failingImage, { shouldDirty: true, shouldValidate: true })}>
                  模拟拉取失败
                </button>
              </div>
            </FormSection>

            {createMutation.error instanceof Error && (
              <div className="rounded-xl border border-rose-400/30 bg-rose-400/10 px-4 py-3 text-sm text-rose-200" role="alert">
                {createMutation.error.message}
              </div>
            )}
            <button className="primary-button" type="submit" disabled={createMutation.isPending}>
              {createMutation.isPending ? "正在接纳发布…" : "创建并发布"}
              <span aria-hidden="true">→</span>
            </button>
          </form>
        </section>

        <aside className="space-y-6">
          <OperationCard operation={currentOperation} isFetching={operationQuery.isFetching} />
          <RuntimeCard snapshot={runtimeQuery.data} error={runtimeQuery.error} />
          <IdentityCard acceptance={acceptance} />
        </aside>
      </main>
    </div>
  );
}

function FormSection({ number, title, children }: { number: string; title: string; children: React.ReactNode }) {
  return (
    <fieldset>
      <legend className="mb-4 flex items-center gap-3 text-sm font-medium text-slate-200">
        <span className="font-mono text-xs text-cyan-300">{number}</span>
        {title}
      </legend>
      {children}
    </fieldset>
  );
}

function Field({ label, error, children }: { label: string; error?: string; children: React.ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      {error && <small>{error}</small>}
    </label>
  );
}

function OperationCard({ operation, isFetching }: { operation?: Operation; isFetching: boolean }) {
  const status = operation?.status ?? "idle";
  return (
    <section className="panel p-6" aria-label="发布操作">
      <div className="flex items-start justify-between gap-3">
        <div>
          <p className="eyebrow">Operation</p>
          <h2 className="mt-2 text-xl font-semibold">发布操作</h2>
        </div>
        <StatusBadge status={status} pulse={isFetching && !isTerminal(status)} />
      </div>
      {operation === undefined ? (
        <EmptyState text="创建发布后，这里会显示异步执行状态。" />
      ) : (
        <div className="mt-6 space-y-4">
          <DataRow label="操作 ID" value={operation.id} mono />
          <DataRow label="尝试次数" value={String(operation.attemptCount)} />
          {operation.errorCategory && <DataRow label="失败类别" value={operation.errorCategory} danger />}
          {operation.errorSummary && <p className="rounded-lg bg-rose-400/10 p-3 text-sm leading-6 text-rose-100">{operation.errorSummary}</p>}
          {operation.attempts.length > 0 && (
            <div className="border-t border-white/8 pt-4">
              <p className="mb-3 text-xs uppercase tracking-wider text-slate-500">Attempts</p>
              {operation.attempts.map((attempt) => (
                <div className="flex items-center justify-between text-sm" key={attempt.id}>
                  <span className="text-slate-400">#{attempt.number} · {attempt.workerId}</span>
                  <span className="text-slate-200">{statusText(attempt.status)}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </section>
  );
}

function RuntimeCard({ snapshot, error }: { snapshot?: RuntimeSnapshot; error: Error | null }) {
  return (
    <section className="panel p-6" aria-label="Kubernetes 实况">
      <div className="flex items-start justify-between gap-3">
        <div>
          <p className="eyebrow">Runtime Snapshot</p>
          <h2 className="mt-2 text-xl font-semibold">Kubernetes 实况</h2>
        </div>
        {snapshot && <span className={snapshot.freshness === "fresh" ? "freshness fresh" : "freshness unavailable"}>{snapshot.freshness === "fresh" ? "实时" : "不可用"}</span>}
      </div>
      {error instanceof Error ? (
        <p className="mt-5 text-sm text-rose-200">{error.message}</p>
      ) : snapshot === undefined ? (
        <EmptyState text="运行时状态只来自 Kubernetes 权威回读。" />
      ) : (
        <div className="mt-6">
          <div className="grid grid-cols-3 gap-3">
            <Metric label="期望" value={snapshot.desiredReplicas} />
            <Metric label="已更新" value={snapshot.updatedReplicas} />
            <Metric label="就绪" value={snapshot.readyReplicas} accent />
          </div>
          <div className="mt-5 space-y-3">
            <DataRow label="Deployment" value={snapshot.deploymentName} mono />
            <DataRow label="观测时间" value={formatTime(snapshot.observedAt)} />
            {snapshot.errorCategory && <DataRow label="观测错误" value={snapshot.errorCategory} danger />}
          </div>
          {snapshot.pods.length > 0 && (
            <div className="mt-5 border-t border-white/8 pt-4">
              <p className="mb-3 text-xs uppercase tracking-wider text-slate-500">Pods</p>
              {snapshot.pods.map((pod) => (
                <div className="flex items-center justify-between gap-4 text-sm" key={pod.name}>
                  <span className="truncate font-mono text-xs text-slate-400">{pod.name}</span>
                  <span className={pod.ready ? "text-emerald-300" : "text-amber-200"}>{pod.ready ? "Ready" : pod.reason || pod.phase}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </section>
  );
}

function IdentityCard({ acceptance }: { acceptance?: DeliveryAcceptance }) {
  return (
    <section className="rounded-2xl border border-white/8 bg-white/[0.025] p-5">
      <p className="text-xs uppercase tracking-[0.18em] text-slate-500">稳定标识</p>
      <div className="mt-4 space-y-3">
        <DataRow label="Project" value={acceptance?.project.id ?? "—"} mono />
        <DataRow label="Application" value={acceptance?.application.id ?? "—"} mono />
        <DataRow label="Release" value={acceptance?.release.id ?? "—"} mono />
      </div>
    </section>
  );
}

function StatusBadge({ status, pulse = false }: { status: string; pulse?: boolean }) {
  return (
    <span className={`status status-${status}`} data-testid="operation-status">
      <span className={pulse ? "status-dot animate-pulse" : "status-dot"} />
      {statusText(status)}
    </span>
  );
}

function Metric({ label, value, accent = false }: { label: string; value: number; accent?: boolean }) {
  return (
    <div className="rounded-xl border border-white/8 bg-black/15 p-3 text-center">
      <strong className={accent ? "text-2xl text-cyan-200" : "text-2xl text-white"}>{value}</strong>
      <span className="mt-1 block text-xs text-slate-500">{label}</span>
    </div>
  );
}

function DataRow({ label, value, mono = false, danger = false }: { label: string; value: string; mono?: boolean; danger?: boolean }) {
  return (
    <div className="flex items-start justify-between gap-4 text-sm">
      <span className="shrink-0 text-slate-500">{label}</span>
      <span className={`${mono ? "font-mono text-xs" : ""} ${danger ? "text-rose-200" : "text-right text-slate-300"} break-all`}>{value}</span>
    </div>
  );
}

function EmptyState({ text }: { text: string }) {
  return <p className="mt-6 rounded-xl border border-dashed border-white/10 px-4 py-6 text-center text-sm leading-6 text-slate-500">{text}</p>;
}

function isTerminal(status?: string): boolean {
  return status === "succeeded" || status === "failed";
}

function statusText(status: string): string {
  return {
    idle: "未开始",
    pending: "等待中",
    running: "执行中",
    succeeded: "已成功",
    failed: "已失败",
  }[status] ?? status;
}

function formatTime(value: string): string {
  return new Intl.DateTimeFormat("zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).format(new Date(value));
}
