import { ArrowUpRight, Boxes, Search } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import { CursorPagination } from "@/shared/CursorPagination";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";

type Resource = { id: string; name: string; slug: string; createdAt: string };

/** 后端只提供游标分页，因此筛选明确限定于当前页，并保留 URL 中的分页上下文。 */
export function ResourceList({
  items,
  nextCursor,
  kind,
  href,
}: {
  items: Resource[];
  nextCursor?: string;
  kind: string;
  href: (item: Resource) => string;
}) {
  const [params, setParams] = useSearchParams();
  const filter = params.get("q") ?? "";
  const visible = items.filter((item) =>
    (item.name + " " + item.slug).toLowerCase().includes(filter.toLowerCase()),
  );
  function update(key: string, value?: string) {
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
    <section aria-label={kind + "列表"} className="min-w-0">
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div className="relative w-full sm:max-w-80">
          <Search className="pointer-events-none absolute top-2.5 left-3 size-4 text-muted-foreground" />
          <Input
            className="h-9 pl-9"
            aria-label={"筛选当前页" + kind}
            placeholder={"筛选当前页" + kind + "…"}
            value={filter}
            onChange={(event) => update("q", event.target.value)}
          />
        </div>
        <span className="text-xs text-muted-foreground">
          本页 {items.length} 个{kind}
          {filter && ` · 匹配 ${visible.length} 个`}
        </span>
      </div>
      <div className="overflow-hidden rounded-lg border">
        <div className="grid grid-cols-[minmax(0,1fr)_100px] gap-4 border-b bg-muted/70 px-5 py-3 text-xs font-medium text-muted-foreground sm:grid-cols-[minmax(0,1fr)_140px_150px]">
          <span>{kind}名称</span>
          <span className="hidden sm:block">创建时间</span>
          <span className="text-right">资源 ID</span>
        </div>
        {visible.map((item) => (
          <Link
            key={item.id}
            to={href(item)}
            className="group grid grid-cols-[minmax(0,1fr)_100px] items-center gap-4 border-b px-5 py-5 transition-colors last:border-0 hover:bg-muted/60 focus-visible:bg-accent focus-visible:outline-none sm:grid-cols-[minmax(0,1fr)_140px_150px]"
          >
            <div className="flex min-w-0 items-center gap-3">
              <span className="grid size-10 shrink-0 place-items-center rounded-lg border bg-muted/50 text-muted-foreground">
                <Boxes className="size-5" />
              </span>
              <div className="min-w-0">
                <div className="truncate text-sm font-semibold group-hover:text-primary">
                  {item.name}
                </div>
                <div className="mt-1 truncate font-mono text-xs text-muted-foreground">
                  {item.slug}
                </div>
              </div>
            </div>
            <time
              className="hidden text-xs text-muted-foreground sm:block"
              dateTime={item.createdAt}
            >
              {new Date(item.createdAt).toLocaleDateString("zh-CN")}
            </time>
            <span className="flex min-w-0 items-center justify-end gap-3">
              <span
                className="truncate font-mono text-xs text-muted-foreground"
                title={item.id}
              >
                {item.id.slice(0, 8)}
              </span>
              <ArrowUpRight className="size-4 shrink-0 text-muted-foreground group-hover:text-primary" />
            </span>
          </Link>
        ))}
        {visible.length === 0 && (
          <div className="px-5 py-12 text-center text-sm text-muted-foreground">
            <p>{filter ? "当前页没有匹配的结果" : "这一页没有资源"}</p>
            {filter && (
              <Button variant="link" onClick={() => update("q")}>
                清除筛选
              </Button>
            )}
          </div>
        )}
      </div>
      <CursorPagination
        className="mt-4"
        cursor={params.get("cursor") ?? undefined}
        nextCursor={nextCursor}
        onChange={(cursor) => update("cursor", cursor)}
      />
    </section>
  );
}
