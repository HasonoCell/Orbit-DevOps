import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import type { Pipeline } from "@/features/applications/api";
import { CursorPagination } from "@/shared/CursorPagination";
import {
  Fact,
  Panel,
  QueryNotice,
  QuietEmpty,
  StatusPill,
  Timestamp,
} from "@/shared/OverviewUI";
import { isActiveRun, pollInterval, runStatus } from "@/shared/overview-status";
import { useQuery } from "@tanstack/react-query";
import { GitBranch } from "lucide-react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { DeliveryStages } from "./DeliveryStages";
import { pipelineQueries } from "./pipeline-api";

export function PipelineSummary({
  applicationId,
  until,
}: {
  applicationId: string;
  until: number;
}) {
  const [params, setParams] = useSearchParams();
  const cursor = params.get("pipelineCursor") ?? undefined;
  const pipelines = useQuery({
    ...pipelineQueries.list(applicationId, cursor),
  });
  return (
    <Panel
      title="自动交付"
      icon={<GitBranch className="size-4" />}
      subtitle="Webhook → 构建 → 部署"
    >
      {pipelines.isPending || pipelines.error ? (
        <QueryNotice
          error={pipelines.error}
          retry={() => void pipelines.refetch()}
        />
      ) : (
        <>
          {pipelines.data.items.length === 0 ? (
            <QuietEmpty>尚未配置 Pipeline</QuietEmpty>
          ) : (
            <div className="divide-y">
              {pipelines.data.items.map((pipeline) => (
                <PipelineItem
                  key={pipeline.pipeline.id}
                  pipeline={pipeline}
                  applicationId={applicationId}
                  until={until}
                />
              ))}
            </div>
          )}
          <CursorPagination
            className="px-5 pb-4"
            cursor={cursor}
            nextCursor={pipelines.data.nextCursor}
            onChange={(next) =>
              setParams((previous) => {
                const updated = new URLSearchParams(previous);
                if (next) updated.set("pipelineCursor", next);
                else updated.delete("pipelineCursor");
                return updated;
              })
            }
          />
        </>
      )}
    </Panel>
  );
}

function PipelineItem({
  pipeline: { pipeline, revision },
  applicationId,
  until,
}: {
  pipeline: Pipeline;
  applicationId: string;
  until: number;
}) {
  const { projectId = "" } = useParams();
  const run = useQuery({
    ...pipelineQueries.latest(pipeline.id),
    refetchInterval: (query) =>
      pollInterval(
        until,
        query.state.error,
        !!query.state.data && isActiveRun(query.state.data.status),
      ),
    refetchIntervalInBackground: false,
  });
  return (
    <article className="px-5 py-5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="break-all text-sm font-semibold">{pipeline.name}</h3>
        <StatusPill
          label={pipeline.enabled ? "已启用" : "已停用"}
          tone={pipeline.enabled ? "success" : "neutral"}
        />
      </div>
      <p className="mt-2 break-all text-xs text-muted-foreground">
        {revision.repositoryFullName} ·{" "}
        {revision.gitRef.replace(/^refs\/heads\//, "")}
      </p>
      <p className="mt-1 text-xs text-muted-foreground">
        当前配置：
        {revision.mode === "auto_release" ? "构建并自动部署" : "仅构建"} ·
        Revision {revision.revision}
      </p>
      <Link
        className="mt-3 inline-block text-xs font-medium text-primary hover:underline"
        to={`/projects/${projectId}/applications/${applicationId}/pipelines/${pipeline.id}`}
      >
        查看与管理 Pipeline
      </Link>
      {run.isPending || run.error ? (
        <QueryNotice error={run.error} retry={() => void run.refetch()} />
      ) : !run.data ? (
        <p className="mt-4 text-xs text-muted-foreground">
          暂无触发记录，等待匹配的 Webhook。
        </p>
      ) : (
        <div className="mt-4 space-y-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-xs text-muted-foreground">
              最近运行 · {run.data.run.sourceCommit.slice(0, 8)}
            </span>
            <StatusPill {...runStatus[run.data.status]} />
          </div>
          <DeliveryStages
            run={run.data}
            buildOnly={run.data.mode === "build_only"}
          />
          <Collapsible className="text-xs">
            <CollapsibleTrigger className="font-medium text-primary">
              运行摘要
            </CollapsibleTrigger>
            <CollapsibleContent>
              <dl className="mt-3 grid gap-3 rounded bg-muted/60 p-3">
                <Fact label="Delivery Run">{run.data.run.id}</Fact>
                <Fact label="触发来源">
                  {run.data.trigger.eventType} · {run.data.trigger.gitRef} ·{" "}
                  <Timestamp value={run.data.trigger.receivedAt} />
                </Fact>
                <Fact label="关联构建">{run.data.run.buildId}</Fact>
                {run.data.run.releaseId && (
                  <Fact label="关联发布">{run.data.run.releaseId}</Fact>
                )}
                {run.data.run.reasonCode && (
                  <Fact label="停止原因">{run.data.run.reasonCode}</Fact>
                )}
              </dl>
            </CollapsibleContent>
          </Collapsible>
        </div>
      )}
    </article>
  );
}
