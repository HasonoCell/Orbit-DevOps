import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import { client, requireData, type DeploymentTarget } from "@/api/http";
import { CursorPagination } from "@/components/CursorPagination";
import {
  QueryNotice,
  QuietEmpty,
  StatusPill,
  Timestamp,
} from "@/components/overview/OverviewUI";
import { operationStatus } from "@/lib/overview-status";

export function ReleaseHistory({
  target,
  applicationId,
}: {
  target: DeploymentTarget;
  applicationId: string;
}) {
  const [params, setParams] = useSearchParams();
  const cursor = params.get("releaseCursor") ?? undefined;
  const query = useQuery({
    queryKey: [
      "application-overview",
      applicationId,
      "history",
      target.id,
      cursor,
    ],
    queryFn: async () =>
      requireData(
        await client.GET(
          "/api/v1/deployment-targets/{deploymentTargetId}/releases",
          {
            params: {
              path: { deploymentTargetId: target.id },
              query: { limit: 5, cursor },
            },
          },
        ),
        "查询发布历史",
      ),
  });
  return (
    <section className="workbench-panel">
      <header className="panel-heading">
        <h2>发布记录</h2>
        <span className="text-xs text-muted-foreground">{target.stage}</span>
      </header>
      {query.isPending || query.error ? (
        <QueryNotice error={query.error} retry={() => void query.refetch()} />
      ) : (
        <>
          {query.data.items.length ? (
            <div className="divide-y">
              {query.data.items.map((item) => (
                <article
                  key={item.release.id}
                  className="space-y-3 p-5 text-xs"
                >
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <span className="break-all font-mono">
                      {item.release.id}
                    </span>
                    <StatusPill
                      {...operationStatus[item.releaseOperation.status]}
                    />
                  </div>
                  <Timestamp value={item.release.createdAt} />
                  <details>
                    <summary className="cursor-pointer text-primary">
                      镜像与执行结果
                    </summary>
                    <p className="runtime-code mt-3">
                      {item.release.imageReference}
                    </p>
                    {item.releaseOperation.errorSummary && (
                      <p className="mt-2 break-words text-destructive">
                        {item.releaseOperation.errorSummary}
                      </p>
                    )}
                  </details>
                </article>
              ))}
            </div>
          ) : (
            <QuietEmpty>暂无发布记录。</QuietEmpty>
          )}
          <CursorPagination
            className="p-4"
            cursor={cursor}
            nextCursor={query.data.nextCursor}
            onChange={(next) =>
              setParams((previous) => {
                const updated = new URLSearchParams(previous);
                if (next) updated.set("releaseCursor", next);
                else updated.delete("releaseCursor");
                return updated;
              })
            }
          />
        </>
      )}
    </section>
  );
}
