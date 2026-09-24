import { useQuery } from "@tanstack/react-query";
import { ArrowRight, GitBranch } from "lucide-react";
import { useSearchParams } from "react-router-dom";
import { latestRun, listPipelines, type Pipeline, type DeliveryRun } from "@/api/overview";
import { CursorPagination } from "@/components/CursorPagination";
import { Fact, Panel, QueryNotice, QuietEmpty, StatusPill, Timestamp } from "./OverviewUI";
import { isActiveRun, pollInterval, runStatus } from "@/lib/overview-status";

export function PipelineSummary({ applicationId, until }: { applicationId: string; until: number }) {
  const [params, setParams] = useSearchParams();
  const cursor = params.get("pipelineCursor") ?? undefined;
  const pipelines = useQuery({
    queryKey: ["application-overview", applicationId, "pipelines", cursor],
    queryFn: () => listPipelines(applicationId, cursor),
  });
  return <Panel title="自动交付" icon={<GitBranch className="size-4" />} subtitle="Webhook → 构建 → 部署">
    {pipelines.isPending || pipelines.error ? <QueryNotice error={pipelines.error} retry={() => void pipelines.refetch()} /> : <>
      {pipelines.data.items.length === 0 ? <QuietEmpty>尚未配置 Pipeline<br />配置后，代码变更可触发自动构建与交付。</QuietEmpty> : <div className="divide-y">{pipelines.data.items.map((pipeline) => <PipelineItem key={pipeline.pipeline.id} pipeline={pipeline} applicationId={applicationId} until={until} />)}</div>}
      <CursorPagination className="px-5 pb-4" cursor={cursor} nextCursor={pipelines.data.nextCursor} onChange={(next) => setParams((previous) => { const updated = new URLSearchParams(previous); if (next) updated.set("pipelineCursor", next); else updated.delete("pipelineCursor"); return updated; })} />
    </>}
  </Panel>;
}

function PipelineItem({ pipeline: { pipeline, revision }, applicationId, until }: { pipeline: Pipeline; applicationId: string; until: number }) {
  const run = useQuery({
    queryKey: ["application-overview", applicationId, "run", pipeline.id],
    queryFn: () => latestRun(pipeline.id),
    refetchInterval: (query) => pollInterval(until, query.state.error, !!query.state.data && isActiveRun(query.state.data.status)),
    refetchIntervalInBackground: false,
  });
  return <article className="px-5 py-5">
    <div className="flex flex-wrap items-center justify-between gap-2"><h3 className="break-all text-sm font-semibold">{pipeline.name}</h3><StatusPill label={pipeline.enabled ? "已启用" : "已停用"} tone={pipeline.enabled ? "success" : "neutral"} /></div>
    <p className="mt-2 break-all text-xs text-muted-foreground">{revision.repositoryFullName} · {revision.gitRef.replace(/^refs\/heads\//, "")}</p>
    <p className="mt-1 text-xs text-muted-foreground">当前配置：{revision.mode === "auto_release" ? "构建并自动部署" : "仅构建"} · Revision {revision.revision}</p>
    {run.isPending || run.error ? <QueryNotice error={run.error} retry={() => void run.refetch()} /> : !run.data ? <p className="mt-4 text-xs text-muted-foreground">暂无触发记录，等待匹配的 Webhook。</p> : <div className="mt-4 space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2"><span className="text-xs text-muted-foreground">最近运行 · {run.data.run.sourceCommit.slice(0, 8)}</span><StatusPill {...runStatus[run.data.status]} /></div>
      {run.data.run.pipelineRevision === revision.revision ? <RunStages run={run.data} buildOnly={revision.mode === "build_only"} /> : <p className="text-xs text-muted-foreground">该运行使用历史配置 Revision {run.data.run.pipelineRevision}；不套用当前配置的阶段。</p>}
      <details className="text-xs"><summary className="cursor-pointer font-medium text-primary">运行摘要</summary><dl className="mt-3 grid gap-3 rounded bg-muted/60 p-3"><Fact label="Delivery Run">{run.data.run.id}</Fact><Fact label="触发来源">{run.data.trigger.eventType} · {run.data.trigger.gitRef} · <Timestamp value={run.data.trigger.receivedAt} /></Fact><Fact label="关联构建">{run.data.run.buildId}</Fact>{run.data.run.releaseId && <Fact label="关联发布">{run.data.run.releaseId}</Fact>}{run.data.run.reasonCode && <Fact label="停止原因">{run.data.run.reasonCode}</Fact>}</dl></details>
    </div>}
  </article>;
}

/** 来源校验归入部署阶段；仅构建流水线不虚构一次成功的部署。 */
function RunStages({ run, buildOnly }: { run: DeliveryRun; buildOnly: boolean }) {
  const artifactReady = !!run.run.imageArtifactId;
  const buildLabel = artifactReady ? "构建完成" : run.activeStage === "build" || ["building", "build_failed", "build_canceled"].includes(run.status) ? runStatus[run.status].label : "构建阶段";
  const deployLabel = run.status === "succeeded" ? "部署完成" : run.activeStage === "source_verification" ? "校验来源中" : run.activeStage === "release" || ["releasing", "release_failed", "release_canceled", "superseded", "blocked"].includes(run.status) ? runStatus[run.status].label : "等待部署";
  return <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/40 p-3 text-xs"><span>{buildLabel}</span>{!buildOnly && <><ArrowRight className="size-3 text-muted-foreground" /><span>{deployLabel}</span></>}{buildOnly && <span className="ml-auto text-muted-foreground">不自动部署</span>}</div>;
}
