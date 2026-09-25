import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, Box, ChevronRight, RefreshCw, Search } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import type { ApplicationPage, Project } from "@/api/http";
import { loadWorkbench, summaryErrors, summaryIssues, type ApplicationSummary } from "@/api/workbench";
import { CursorPagination } from "@/components/CursorPagination";
import { ErrorPanel } from "@/components/PageState";
import { StatusPill, Timestamp } from "@/components/overview/OverviewUI";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { operationStatus, runStatus } from "@/lib/overview-status";

/** 汇总仅覆盖当前应用页的有界样本；刷新失败后不继续呈现旧的成功状态。 */
export function ProjectWorkbench({ project, page, cursor }: { project: Project; page: ApplicationPage; cursor?: string }) {
  const [params, setParams] = useSearchParams();
  const query = useQuery({ queryKey: ["project-workbench", project.id, page.items.map(a => a.id)], queryFn: ({ signal }) => loadWorkbench(page.items, signal), staleTime: 30_000 });
  const rows = query.error ? [] : query.data ?? [];
  const byId = new Map(rows.map(row => [row.application.id, row]));
  const q = params.get("q") ?? "";
  const fault = params.get("attention") === "1";
  const delivery = params.get("view") === "delivery";
  const issues = rows.filter(row => summaryIssues(row).length > 0);
  const errors = rows.filter(row => summaryErrors(row).length > 0);
  const visible = page.items.filter(app => (app.name + " " + app.slug).toLowerCase().includes(q.toLowerCase()) && (!fault || (byId.has(app.id) && summaryIssues(byId.get(app.id)!).length > 0)));
  const href = (id: string) => `/projects/${project.id}/applications/${id}`;
  function update(key: string, value: string) { setParams(previous => { const next = new URLSearchParams(previous); if (value) next.set(key, value); else next.delete(key); return next; }, { replace: key === "q" }); }
  return <>
    <div className="workbench-summary"><span>本页应用 <strong>{page.items.length}</strong></span><span>待处理应用 <strong>{query.isPending || query.error ? "—" : issues.length}</strong></span><span className="text-xs text-muted-foreground">仅当前页，不代表项目总量或运行健康</span><Button className="ml-auto" variant="outline" size="sm" disabled={query.isFetching} onClick={() => void query.refetch()}><RefreshCw className="size-3.5" aria-hidden="true" />刷新摘要</Button></div>
    {query.error && <ErrorPanel title="无法加载交付摘要" error={query.error} onRetry={() => void query.refetch()} />}
    {errors.length > 0 && <p role="status" className="mb-4 rounded border border-amber-200 bg-amber-50 p-3 text-sm text-amber-900">{errors.length} 个应用的摘要读取不完整；异常筛选不包含未知状态。可刷新摘要重试。</p>}
    {query.isPending && <p role="status" className="mb-3 text-xs text-muted-foreground">正在加载本页交付摘要…</p>}
    <div className="workbench-columns"><section className="workbench-panel min-w-0">
      <header className="panel-heading"><h2>{delivery ? "本页自动交付" : "应用"}</h2><span className="text-xs text-muted-foreground">数据库记录</span></header>
      {delivery ? <DeliveryFeed rows={rows} href={href} /> : <>
        <div className="workbench-toolbar"><label htmlFor="application-search" className="text-xs text-muted-foreground">搜索</label><div className="relative min-w-0 flex-1 sm:max-w-xs"><Search aria-hidden="true" className="absolute left-3 top-2.5 size-4 text-muted-foreground" /><Input id="application-search" aria-label="筛选当前页应用" placeholder="应用名称或标识…" className="bg-white pl-9" value={q} onChange={event => update("q", event.target.value)} /></div><Button variant={fault ? "secondary" : "outline"} size="sm" aria-pressed={fault} onClick={() => update("attention", fault ? "" : "1")}><AlertTriangle aria-hidden="true" className="size-3.5" />仅看待处理</Button></div>
        <table className="workbench-table"><thead><tr><th>应用名称</th><th>最近发布</th><th>部署目标</th><th>最近构建</th><th><span className="sr-only">进入应用</span></th></tr></thead><tbody>{visible.map(app => { const row = byId.get(app.id); return <tr key={app.id} className={row && summaryIssues(row).length ? "attention-row" : ""}><td><Link className="application-link" to={href(app.id)}><Box aria-hidden="true" className="size-8 shrink-0 rounded border p-1.5 text-muted-foreground" /><span>{app.name}<small>{app.slug}</small></span></Link></td><td><span className="mobile-cell-label">最近发布</span>{row ? <ReleaseStates summary={row} /> : <span className="text-muted-foreground">{query.error ? "读取失败" : "加载中…"}</span>}</td><td><span className="mobile-cell-label">部署目标</span>{row?.targets.error ? <span className="text-destructive">读取失败</span> : row ? <div className="flex flex-wrap gap-1">{row.targets.data?.length ? row.targets.data.map(item => <Link key={item.target.id} className="target-tag" to={`${href(app.id)}?target=${item.target.id}`}>{item.target.stage}</Link>) : <span className="text-muted-foreground">未配置</span>}</div> : "—"}</td><td><span className="mobile-cell-label">最近构建</span>{row?.build.error ? <span className="text-destructive">读取失败</span> : row?.build.data ? <><StatusPill {...operationStatus[row.build.data.buildOperation.status]} /><small className="mt-1 block text-xs text-muted-foreground"><Timestamp value={row.build.data.build.createdAt} /></small></> : <span className="text-muted-foreground">{row ? "暂无构建" : "—"}</span>}</td><td className="table-arrow"><ChevronRight aria-hidden="true" className="size-3 text-muted-foreground" /></td></tr>; })}</tbody></table>
        {visible.length === 0 && <p className="p-8 text-center text-sm text-muted-foreground">{query.isPending && fault ? "待处理状态仍在加载" : "当前页没有匹配的结果"}</p>}
      </>}
      <div className="flex flex-wrap items-center justify-between gap-3 border-t p-4"><p className="text-xs text-muted-foreground">本页 {page.items.length} 个应用{!delivery && ` · 匹配 ${visible.length} 个`}</p><CursorPagination cursor={cursor} nextCursor={page.nextCursor} onChange={next => update("cursor", next ?? "")} /></div>
    </section><aside className="workbench-aside">
      <section className="attention-panel"><h2 className="flex items-center gap-2"><AlertTriangle className="size-4" aria-hidden="true" />本页待处理</h2>{issues.length ? issues.map(row => <div className="mt-4" key={row.application.id}><p className="text-sm font-semibold">{row.application.name}</p><ul className="my-2 space-y-1 text-xs text-muted-foreground">{summaryIssues(row).map((issue, index) => <li key={index}>{issue}</li>)}</ul><Link className="text-xs font-medium text-destructive underline-offset-4 hover:underline" to={`${href(row.application.id)}?view=delivery`}>查看交付与诊断</Link></div>) : <p className="mt-3 text-xs leading-6 text-muted-foreground">{query.isPending ? "正在查询执行结果。" : query.error || errors.length ? "摘要不完整，不能确认是否还有待处理项。" : "已读取的执行记录中没有待处理项；这不是运行健康结论。"}</p>}</section>
      {!delivery && <section><h2 className="mb-2 text-sm font-semibold">本页最近交付</h2><DeliveryFeed rows={rows} href={href} compact /></section>}
      <section className="border-t pt-5"><h2 className="text-sm font-semibold">项目资料</h2><dl className="mt-4 grid grid-cols-[72px_minmax(0,1fr)] gap-3 text-xs"><dt className="text-muted-foreground">项目标识</dt><dd className="break-all">{project.slug}</dd><dt className="text-muted-foreground">创建时间</dt><dd>{new Date(project.createdAt).toLocaleDateString("zh-CN")}</dd></dl><details className="mt-3 text-xs"><summary className="cursor-pointer text-primary">项目 ID</summary><p className="mt-2 break-all font-mono">{project.id}</p></details></section>
    </aside></div>
    <p className="mt-4 text-xs leading-5 text-muted-foreground">摘要按需读取，每个应用最多采样 3 条 Pipeline 的最近运行。{query.dataUpdatedAt > 0 && <>读取于 <Timestamp value={new Date(query.dataUpdatedAt).toISOString()} />。</>}运行健康请进入应用查看 Kubernetes 观测。</p>
  </>;
}

function ReleaseStates({ summary }: { summary: ApplicationSummary }) {
  if (summary.targets.error) return <span className="text-destructive">目标读取失败</span>;
  if (!summary.targets.data?.length) return <span className="text-muted-foreground">未配置目标</span>;
  return <div className="space-y-1.5">{summary.targets.data.map(item => <div key={item.target.id} className="flex flex-wrap items-center gap-1"><span className="text-xs text-muted-foreground">{item.target.stage}</span>{item.release.error ? <span className="text-xs text-destructive">读取失败</span> : item.release.data ? <StatusPill {...operationStatus[item.release.data.releaseOperation.status]} /> : <span className="text-xs text-muted-foreground">未发布</span>}</div>)}</div>;
}

function DeliveryFeed({ rows, href, compact = false }: { rows: ApplicationSummary[]; href: (id: string) => string; compact?: boolean }) {
  const records = rows.flatMap(row => (row.pipelines.data ?? []).flatMap(item => item.run.data ? [{ app: row.application, pipeline: item.pipeline, run: item.run.data }] : [])).sort((a, b) => b.run.run.createdAt.localeCompare(a.run.run.createdAt));
  const displayed = compact ? records.slice(0, 3) : records;
  return <div className="divide-y">{displayed.map(item => <article key={item.run.run.id} className="space-y-2 px-4 py-4"><div className="flex flex-wrap items-center justify-between gap-2"><h3 className="break-all text-sm font-medium">{item.app.name}</h3><StatusPill {...runStatus[item.run.status]} /></div><p className="break-all text-xs text-muted-foreground">{item.pipeline.pipeline.name}</p><div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground"><Timestamp value={item.run.run.createdAt} /><Link className="text-primary hover:underline" to={`${href(item.app.id)}?view=delivery`}>交付详情</Link></div></article>)}{!records.length && <p className="p-4 text-xs leading-6 text-muted-foreground">当前已读取的样本中没有交付运行；不代表整个项目没有历史记录。</p>}</div>;
}
