import {
  AlertTriangle,
  Box,
  ChevronRight,
  RefreshCw,
  Search,
} from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import type { Project } from "@/api/http";
import {
  summaryIssues,
  type ApplicationSummary,
  type WorkbenchPage,
} from "@/features/projects/workbench/api";
import { StatusPill, Timestamp } from "@/shared/OverviewUI";
import { Button } from "@/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { operationStatus, runStatus } from "@/shared/overview-status";
import { ProjectPagination } from "./ProjectPagination";

/** 汇总仅覆盖当前应用页的有界样本；刷新失败后不继续呈现旧的成功状态。 */
export function ProjectWorkbench({
  project,
  page,
  cursor,
  isFetching,
  onRefresh,
}: {
  project: Project;
  page: WorkbenchPage;
  cursor?: string;
  isFetching: boolean;
  onRefresh: () => void;
}) {
  const [params, setParams] = useSearchParams();
  const rows = page.items;
  const q = params.get("q") ?? "";
  const fault = params.get("attention") === "1";
  const delivery = params.get("view") === "delivery";
  const issues = rows.filter((row) => summaryIssues(row).length > 0);
  const visible = rows.filter(
    (row) =>
      (row.application.name + " " + row.application.slug)
        .toLowerCase()
        .includes(q.toLowerCase()) &&
      (!fault || summaryIssues(row).length > 0),
  );
  const href = (id: string) => `/projects/${project.id}/applications/${id}`;
  function update(key: string, value: string) {
    setParams(
      (previous) => {
        const next = new URLSearchParams(previous);
        if (value) next.set(key, value);
        else next.delete(key);
        return next;
      },
      { replace: key === "q" },
    );
  }
  return (
    <>
      <div className="workbench-summary">
        <span>
          本页应用 <strong>{page.items.length}</strong>
        </span>
        <span>
          本页待处理 <strong>{issues.length}</strong>
        </span>
        <Button
          className="ml-auto"
          variant="outline"
          size="sm"
          disabled={isFetching}
          onClick={onRefresh}
        >
          <RefreshCw className="size-3.5" aria-hidden="true" />
          刷新摘要
        </Button>
      </div>
      <div className="workbench-columns">
        <section className="workbench-panel min-w-0">
          <header className="panel-heading">
            <h2>{delivery ? "本页自动交付" : "应用"}</h2>
          </header>
          {delivery ? (
            <DeliveryFeed rows={rows} href={href} />
          ) : (
            <>
              <div className="workbench-toolbar">
                <label
                  htmlFor="application-search"
                  className="text-xs text-muted-foreground"
                >
                  搜索
                </label>
                <div className="relative min-w-0 flex-1 sm:max-w-xs">
                  <Search
                    aria-hidden="true"
                    className="absolute left-3 top-2.5 size-4 text-muted-foreground"
                  />
                  <Input
                    id="application-search"
                    aria-label="筛选当前页应用"
                    placeholder="应用名称或标识…"
                    className="bg-white pl-9"
                    value={q}
                    onChange={(event) => update("q", event.target.value)}
                  />
                </div>
                <Button
                  variant={fault ? "secondary" : "outline"}
                  size="sm"
                  aria-pressed={fault}
                  onClick={() => update("attention", fault ? "" : "1")}
                >
                  <AlertTriangle aria-hidden="true" className="size-3.5" />
                  仅看待处理
                </Button>
              </div>
              <table className="workbench-table">
                <thead>
                  <tr>
                    <th>应用名称</th>
                    <th>最近发布</th>
                    <th>部署目标</th>
                    <th>最近构建</th>
                    <th>
                      <span className="sr-only">进入应用</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {visible.map((row) => {
                    const app = row.application;
                    return (
                      <tr
                        key={app.id}
                        className={
                          summaryIssues(row).length ? "attention-row" : ""
                        }
                      >
                        <td>
                          <Link className="application-link" to={href(app.id)}>
                            <Box
                              aria-hidden="true"
                              className="size-8 shrink-0 rounded border p-1.5 text-muted-foreground"
                            />
                            <span>
                              {app.name}
                              <small>{app.slug}</small>
                            </span>
                          </Link>
                        </td>
                        <td>
                          <span className="mobile-cell-label">最近发布</span>
                          <ReleaseStates summary={row} />
                        </td>
                        <td>
                          <span className="mobile-cell-label">部署目标</span>
                          <div className="flex flex-wrap gap-1">
                            {row.targets.length ? (
                              row.targets.map((item) => (
                                <Link
                                  key={item.id}
                                  className="target-tag"
                                  to={`${href(app.id)}?target=${item.id}`}
                                >
                                  {item.stage}
                                </Link>
                              ))
                            ) : (
                              <span className="text-muted-foreground">
                                未配置
                              </span>
                            )}
                          </div>
                        </td>
                        <td>
                          <span className="mobile-cell-label">最近构建</span>
                          {row.build ? (
                            <>
                              <StatusPill
                                {...operationStatus[row.build.status]}
                              />
                              <small className="mt-1 block text-xs text-muted-foreground">
                                <Timestamp value={row.build.createdAt} />
                              </small>
                            </>
                          ) : (
                            <span className="text-muted-foreground">
                              暂无构建
                            </span>
                          )}
                        </td>
                        <td className="table-arrow">
                          <ChevronRight
                            aria-hidden="true"
                            className="size-3 text-muted-foreground"
                          />
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              {visible.length === 0 && (
                <p className="p-8 text-center text-sm text-muted-foreground">
                  本页无匹配结果
                </p>
              )}
            </>
          )}
          <div className="flex flex-wrap items-center justify-between gap-3 border-t p-4">
            <p className="text-xs text-muted-foreground">
              本页 {page.items.length} 个应用
              {!delivery && ` · 匹配 ${visible.length} 个`}
            </p>
            <ProjectPagination
              projectId={project.id}
              cursor={cursor}
              nextCursor={page.nextCursor}
              onChange={(next, pageNumber) =>
                setParams((previous) => {
                  const updated = new URLSearchParams(previous);
                  if (next) updated.set("cursor", next);
                  else updated.delete("cursor");
                  if (pageNumber > 1) updated.set("page", String(pageNumber));
                  else updated.delete("page");
                  return updated;
                })
              }
            />
          </div>
        </section>
        <aside className="workbench-aside">
          <section
            className="attention-panel"
            data-attention={issues.length > 0}
          >
            <h2 className="flex items-center gap-2">
              <AlertTriangle className="size-4" aria-hidden="true" />
              本页待处理
            </h2>
            {issues.length ? (
              issues.map((row) => (
                <div className="mt-4" key={row.application.id}>
                  <p className="text-sm font-semibold">
                    {row.application.name}
                  </p>
                  <ul className="my-2 space-y-1 text-xs text-muted-foreground">
                    {summaryIssues(row).map((issue, index) => (
                      <li key={index}>{issue}</li>
                    ))}
                  </ul>
                  <Link
                    className="text-xs font-medium text-destructive underline-offset-4 hover:underline"
                    to={`${href(row.application.id)}?view=delivery`}
                  >
                    查看交付与诊断
                  </Link>
                </div>
              ))
            ) : (
              <p className="mt-3 text-xs leading-6 text-muted-foreground">
                本页暂无待处理项
              </p>
            )}
          </section>
          {!delivery && (
            <section>
              <h2 className="mb-2 text-sm font-semibold">本页最近交付</h2>
              <DeliveryFeed rows={rows} href={href} compact />
            </section>
          )}
          <section className="border-t pt-5">
            <h2 className="text-sm font-semibold">项目资料</h2>
            <dl className="mt-4 grid grid-cols-[72px_minmax(0,1fr)] gap-3 text-xs">
              <dt className="text-muted-foreground">项目标识</dt>
              <dd className="break-all">{project.slug}</dd>
              <dt className="text-muted-foreground">创建时间</dt>
              <dd>{new Date(project.createdAt).toLocaleDateString("zh-CN")}</dd>
            </dl>
            <Collapsible className="mt-3 text-xs">
              <CollapsibleTrigger className="text-primary">
                项目 ID
              </CollapsibleTrigger>
              <CollapsibleContent>
                <p className="mt-2 break-all font-mono">{project.id}</p>
              </CollapsibleContent>
            </Collapsible>
          </section>
        </aside>
      </div>
    </>
  );
}

function ReleaseStates({ summary }: { summary: ApplicationSummary }) {
  if (!summary.targets.length)
    return <span className="text-muted-foreground">未配置目标</span>;
  return (
    <div className="space-y-1.5">
      {summary.targets.map((item) => (
        <div key={item.id} className="flex flex-wrap items-center gap-1">
          <span className="text-xs text-muted-foreground">{item.stage}</span>
          {item.releaseStatus ? (
            <StatusPill {...operationStatus[item.releaseStatus]} />
          ) : (
            <span className="text-xs text-muted-foreground">未发布</span>
          )}
        </div>
      ))}
    </div>
  );
}

function DeliveryFeed({
  rows,
  href,
  compact = false,
}: {
  rows: ApplicationSummary[];
  href: (id: string) => string;
  compact?: boolean;
}) {
  const records = rows
    .flatMap((row) =>
      row.pipelines.flatMap((pipeline) =>
        pipeline.runStatus && pipeline.runCreatedAt
          ? [
              {
                app: row.application,
                pipeline,
                runStatus: pipeline.runStatus,
                runCreatedAt: pipeline.runCreatedAt,
              },
            ]
          : [],
      ),
    )
    .sort((a, b) => b.runCreatedAt.localeCompare(a.runCreatedAt));
  const displayed = compact ? records.slice(0, 3) : records;
  return (
    <div className="divide-y">
      {displayed.map((item) => (
        <article key={item.pipeline.id} className="space-y-2 px-4 py-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h3 className="break-all text-sm font-medium">{item.app.name}</h3>
            <StatusPill {...runStatus[item.runStatus]} />
          </div>
          <p className="break-all text-xs text-muted-foreground">
            {item.pipeline.name}
          </p>
          <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
            <Timestamp value={item.runCreatedAt} />
            <Link
              className="text-primary hover:underline"
              to={`${href(item.app.id)}?view=delivery`}
            >
              交付详情
            </Link>
          </div>
        </article>
      ))}
      {!records.length && (
        <p className="p-4 text-xs leading-6 text-muted-foreground">
          本页暂无交付记录
        </p>
      )}
    </div>
  );
}
