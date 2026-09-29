import { useQuery } from "@tanstack/react-query";
import { Link, useParams, useSearchParams } from "react-router-dom";
import type { DeploymentTarget } from "@/api/http";
import {
  listReleaseHistory,
  releaseQueryKeys,
} from "@/features/applications/releases/api";
import { CursorPagination } from "@/shared/CursorPagination";
import {
  QueryNotice,
  QuietEmpty,
  StatusPill,
  Timestamp,
} from "@/shared/OverviewUI";
import { operationStatus } from "@/shared/overview-status";

export function ReleaseHistory({
  target,
  applicationId,
}: {
  target: DeploymentTarget;
  applicationId: string;
}) {
  const { projectId = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const cursor = params.get("releaseCursor") ?? undefined;
  const query = useQuery({
    queryKey: releaseQueryKeys.history(applicationId, target.id, cursor),
    queryFn: () => listReleaseHistory(target.id, cursor),
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
                  <Link
                    className="inline-block font-medium text-primary hover:underline"
                    to={`/projects/${projectId}/applications/${applicationId}/releases/${item.release.id}`}
                  >
                    查看发布详情
                  </Link>
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
