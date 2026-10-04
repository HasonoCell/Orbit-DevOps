import { Button } from "@/components/ui/button";
import { accessQueries } from "@/features/projects/access/api";
import { QueryNotice } from "@/shared/OverviewUI";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router-dom";

export function TargetAccessRoutes({
  projectId,
  targetId,
}: {
  projectId: string;
  targetId: string;
}) {
  const [offset, setOffset] = useState(0);
  const routes = useQuery({
    ...accessQueries.targetRoutes(targetId, offset),
  });
  return (
    <section className="workbench-panel mt-5 max-w-3xl">
      <header className="panel-heading">
        <h2>关联入口</h2>
      </header>
      {routes.isPending || routes.error ? (
        <QueryNotice error={routes.error} retry={() => void routes.refetch()} />
      ) : (
        <>
          {routes.data.length ? (
            <div className="divide-y">
              {routes.data.slice(0, 20).map((route) => (
                <div
                  key={route.routeId}
                  className="flex flex-wrap items-center justify-between gap-3 p-5 text-sm"
                >
                  <Link
                    className="break-all font-medium text-primary hover:underline"
                    to={`/projects/${projectId}/access-hosts/${route.hostId}`}
                  >
                    {route.hostname}
                    {route.pathPrefix}
                  </Link>
                  <span className="text-xs text-muted-foreground">
                    {route.routeLifecycle === "deleting" ||
                    route.hostLifecycle === "deleting"
                      ? "清理中"
                      : "已声明"}
                  </span>
                </div>
              ))}
            </div>
          ) : (
            <p className="p-5 text-sm text-muted-foreground">暂无关联入口</p>
          )}
          {(offset > 0 || routes.data.length > 20) && (
            <div className="flex justify-end gap-2 border-t p-4">
              <Button
                variant="outline"
                disabled={offset === 0}
                onClick={() => setOffset(Math.max(0, offset - 20))}
              >
                上一页
              </Button>
              <Button
                variant="outline"
                disabled={routes.data.length <= 20 || offset >= 9980}
                onClick={() => setOffset(offset + 20)}
              >
                下一页
              </Button>
            </div>
          )}
        </>
      )}
    </section>
  );
}
