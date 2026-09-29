import { useQuery } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { QueryNotice, Timestamp } from "@/shared/OverviewUI";
import { accessKeys, listHosts } from "./api";

export function AccessHostsPanel({
  projectId,
  canManage,
}: {
  projectId: string;
  canManage: boolean;
}) {
  const [params, setParams] = useSearchParams();
  const rawOffset = Number(params.get("hostOffset") ?? 0);
  const offset =
    Number.isInteger(rawOffset) && rawOffset >= 0 && rawOffset <= 10000
      ? rawOffset
      : 0;
  const hosts = useQuery({
    queryKey: accessKeys.hosts(projectId, offset),
    queryFn: () => listHosts(projectId, offset),
  });
  function changeOffset(next: number) {
    setParams((previous) => {
      const updated = new URLSearchParams(previous);
      if (next) updated.set("hostOffset", String(next));
      else updated.delete("hostOffset");
      return updated;
    });
  }
  return (
    <section className="workbench-panel mt-5">
      <header className="panel-heading">
        <div>
          <h2>访问域名</h2>
          <p className="mt-1 text-xs text-muted-foreground">
            入口状态与 DNS 验证分别观察
          </p>
        </div>
        {canManage && (
          <Button asChild>
            <Link to={`/projects/${projectId}/access-hosts/new`}>添加域名</Link>
          </Button>
        )}
      </header>
      {hosts.isPending || hosts.error ? (
        <QueryNotice error={hosts.error} retry={() => void hosts.refetch()} />
      ) : (
        <>
          {hosts.data.length ? (
            <div className="divide-y">
              {hosts.data.slice(0, 20).map((host) => (
                <article
                  className="flex flex-wrap items-center justify-between gap-3 p-5 text-sm"
                  key={host.id}
                >
                  <div>
                    <Link
                      className="font-medium text-primary hover:underline"
                      to={`/projects/${projectId}/access-hosts/${host.id}`}
                    >
                      {host.hostname}
                    </Link>
                    <p className="mt-1 text-xs text-muted-foreground">
                      {host.tlsMode === "http_only" ? "HTTP" : "TLS"} ·{" "}
                      {host.lifecycle === "deleting" ? "清理中" : "已声明"}
                    </p>
                  </div>
                  <Timestamp value={host.updatedAt} />
                </article>
              ))}
            </div>
          ) : (
            <p className="p-5 text-sm text-muted-foreground">暂无访问域名</p>
          )}
          {(offset > 0 || hosts.data.length > 20) && (
            <div className="flex justify-end gap-2 border-t p-4">
              <Button
                variant="outline"
                disabled={offset === 0}
                onClick={() => changeOffset(Math.max(0, offset - 20))}
              >
                上一页
              </Button>
              <Button
                variant="outline"
                disabled={hosts.data.length <= 20 || offset >= 9980}
                onClick={() => changeOffset(offset + 20)}
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
