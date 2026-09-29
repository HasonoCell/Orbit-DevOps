import { useQuery } from "@tanstack/react-query";
import { Hammer } from "lucide-react";
import { useSearchParams } from "react-router-dom";
import { listBuilds, overviewQueryKeys } from "@/features/applications/api";
import { CursorPagination } from "@/shared/CursorPagination";
import {
  Fact,
  Panel,
  QueryNotice,
  QuietEmpty,
  StatusPill,
  Timestamp,
} from "@/shared/OverviewUI";
import {
  isActiveOperation,
  operationStatus,
  pollInterval,
} from "@/shared/overview-status";

export function BuildSummary({
  applicationId,
  until,
}: {
  applicationId: string;
  until: number;
}) {
  const [params, setParams] = useSearchParams();
  const cursor = params.get("buildCursor") ?? undefined;
  const builds = useQuery({
    queryKey: overviewQueryKeys.builds(applicationId, cursor),
    queryFn: () => listBuilds(applicationId, cursor),
    refetchInterval: (query) =>
      pollInterval(
        until,
        query.state.error,
        query.state.data?.items.some((item) =>
          isActiveOperation(item.buildOperation.status),
        ) ?? false,
      ),
    refetchIntervalInBackground: false,
  });
  return (
    <Panel
      title="构建记录"
      icon={<Hammer className="size-4" />}
      subtitle={cursor ? "历史记录" : "最近 5 条"}
    >
      {builds.isPending || builds.error ? (
        <QueryNotice error={builds.error} retry={() => void builds.refetch()} />
      ) : (
        <>
          {builds.data.items.length === 0 ? (
            <QuietEmpty>暂无构建记录</QuietEmpty>
          ) : (
            <div className="divide-y">
              {builds.data.items.map(
                ({ build, buildOperation, imageArtifact }) => (
                  <details key={build.id} className="group px-5 py-4">
                    <summary className="flex cursor-pointer list-none flex-wrap items-center justify-between gap-3 [&::-webkit-details-marker]:hidden">
                      <div className="min-w-0">
                        <div className="flex items-center gap-2">
                          <span className="font-mono text-sm font-medium">
                            {build.sourceCommit.slice(0, 8)}
                          </span>
                          <StatusPill
                            {...operationStatus[buildOperation.status]}
                          />
                        </div>
                        <p className="mt-1.5 text-xs text-muted-foreground">
                          Build {build.id.slice(0, 8)} ·{" "}
                          <Timestamp value={build.createdAt} />
                        </p>
                      </div>
                      <span className="text-xs text-primary group-open:hidden">
                        展开详情
                      </span>
                      <span className="hidden text-xs text-primary group-open:inline">
                        收起详情
                      </span>
                    </summary>
                    <dl className="mt-4 grid gap-4 rounded-md bg-muted/70 p-4">
                      <Fact label="代码仓库">{build.repositoryUrl}</Fact>
                      <Fact label="构建配置">
                        {build.dockerfilePath} · {build.contextPath} ·{" "}
                        {build.platform}
                      </Fact>
                      <Fact label="Build ID">{build.id}</Fact>
                      <Fact label="镜像产物">
                        {imageArtifact?.imageReference ?? "暂无产物"}
                      </Fact>
                      {buildOperation.errorSummary && (
                        <Fact label="执行原因">
                          {buildOperation.errorSummary}
                        </Fact>
                      )}
                    </dl>
                  </details>
                ),
              )}
            </div>
          )}
          <CursorPagination
            className="px-5 pb-4"
            cursor={cursor}
            nextCursor={builds.data.nextCursor}
            onChange={(next) =>
              setParams((previous) => {
                const updated = new URLSearchParams(previous);
                if (next) updated.set("buildCursor", next);
                else updated.delete("buildCursor");
                return updated;
              })
            }
          />
        </>
      )}
    </Panel>
  );
}
