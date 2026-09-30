import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useSearchParams, Link } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { CursorPagination } from "@/shared/CursorPagination";
import { QueryNotice, StatusPill, Timestamp } from "@/shared/OverviewUI";
import { pollInterval, runStatus } from "@/shared/overview-status";
import { DeliveryStages } from "./DeliveryStages";
import { listRuns, pipelineKeys } from "./pipeline-api";

export function PipelineRuns({
  projectId,
  applicationId,
  pipelineId,
}: {
  projectId: string;
  applicationId: string;
  pipelineId: string;
}) {
  const [params, setParams] = useSearchParams();
  const [until, setUntil] = useState(() => Date.now() + 5 * 60_000);
  const cursor = params.get("runCursor") ?? undefined;
  const runs = useQuery({
    queryKey: pipelineKeys.runs(pipelineId, cursor),
    queryFn: () => listRuns(pipelineId, cursor),
    refetchInterval: (query) => pollInterval(until, query.state.error),
    refetchIntervalInBackground: false,
  });
  return (
    <section className="workbench-panel mt-5">
      <header className="panel-heading">
        <h2>运行历史</h2>
        <Button
          size="sm"
          variant="outline"
          onClick={() => {
            setUntil(Date.now() + 5 * 60_000);
            void runs.refetch();
          }}
        >
          刷新运行
        </Button>
      </header>
      {runs.isPending || runs.error ? (
        <QueryNotice error={runs.error} retry={() => void runs.refetch()} />
      ) : (
        <>
          {runs.data.items.length ? (
            <div className="divide-y">
              {runs.data.items.map((item) => (
                <article key={item.run.id} className="space-y-3 p-5 text-sm">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <Link
                      className="font-medium text-primary hover:underline"
                      to={`/projects/${projectId}/applications/${applicationId}/pipelines/${pipelineId}/runs/${item.run.id}`}
                    >
                      {item.run.sourceCommit.slice(0, 8)} · 查看运行
                    </Link>
                    <StatusPill {...runStatus[item.status]} />
                  </div>
                  <p className="text-xs text-muted-foreground">
                    {item.trigger.eventType} ·{" "}
                    <Timestamp value={item.trigger.receivedAt} />
                  </p>
                  <DeliveryStages
                    run={item}
                    buildOnly={item.mode === "build_only"}
                  />
                </article>
              ))}
            </div>
          ) : (
            <p className="p-5 text-sm text-muted-foreground">暂无交付运行</p>
          )}
          <CursorPagination
            className="p-4"
            cursor={cursor}
            nextCursor={runs.data.nextCursor}
            onChange={(next) =>
              setParams((previous) => {
                const updated = new URLSearchParams(previous);
                if (next) updated.set("runCursor", next);
                else updated.delete("runCursor");
                return updated;
              })
            }
          />
        </>
      )}
    </section>
  );
}
