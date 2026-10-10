import type { Application } from "@/api/http";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { QueryNotice } from "@/shared/OverviewUI";
import { useQueries } from "@tanstack/react-query";
import { useId, useState } from "react";
import { buildQueries } from "../builds/api";

type ImageArtifact = components["schemas"]["ImageArtifact"];

/** 游标只描述已访问的构建页；选中快照由发布草稿持有，不受翻页或缓存刷新影响。 */
export function ArtifactPicker({
  application,
  selected,
  open,
  disabled,
  onSelect,
}: {
  application: Application;
  selected?: ImageArtifact;
  open: boolean;
  disabled: boolean;
  onSelect: (artifact: ImageArtifact) => void;
}) {
  const id = useId();
  const [pageCursors, setPageCursors] = useState<Array<string | undefined>>([
    undefined,
  ]);
  const [pageIndex, setPageIndex] = useState(0);
  // 与概览共享规范查询；只订阅用户访问过的页，不扫描历史来推测总页数。
  const pages = useQueries({
    queries: pageCursors.map((cursor) => ({
      ...buildQueries.list(application.id, cursor),
      enabled: open,
    })),
  });
  // 后台刷新可能改变游标链。仅保留连贯前缀，失效页面回落到最近的有效页；
  // 下一次点击才读取新游标，已选产物与发布确认均不随页链重建而重置。
  let pageCount = 1;
  while (
    pageCount < pageCursors.length &&
    pages[pageCount - 1].data?.nextCursor === pageCursors[pageCount]
  ) {
    pageCount += 1;
  }
  const activeIndex = Math.min(pageIndex, pageCount - 1);
  const page = pages[activeIndex];
  const nextCursor = page.data?.nextCursor;
  const hasVisitedNext = activeIndex + 1 < pageCount;
  const canReadNext =
    !!nextCursor && !pageCursors.slice(0, pageCount).includes(nextCursor);
  const canGoNext =
    !disabled &&
    !page.isPending &&
    !page.error &&
    (hasVisitedNext || (canReadNext && !page.isFetching));
  const artifacts = [
    ...new Map(
      (page.data?.items ?? [])
        .flatMap((item) =>
          item.build.projectId === application.projectId &&
          item.build.applicationId === application.id &&
          item.buildOperation.status === "succeeded" &&
          item.imageArtifact?.projectId === application.projectId &&
          item.imageArtifact.applicationId === application.id
            ? [item.imageArtifact]
            : [],
        )
        .map((artifact) => [artifact.id, artifact]),
    ).values(),
  ];
  // 长历史仍保持紧凑，只展示当前页附近最多三个已访问页码。
  const firstVisibleIndex = Math.max(
    0,
    Math.min(activeIndex - 1, pageCount - 3),
  );
  const visiblePages = Array.from(
    { length: Math.min(3, pageCount) },
    (_, offset) => firstVisibleIndex + offset,
  );

  function nextPage() {
    if (!canGoNext) return;
    if (!hasVisitedNext && nextCursor) {
      setPageCursors([...pageCursors.slice(0, pageCount), nextCursor]);
    }
    setPageIndex(activeIndex + 1);
  }

  return (
    <div className="space-y-3">
      <p id={`${id}-label`} className="text-sm font-medium">
        选择产物
      </p>
      <RadioGroup
        aria-labelledby={`${id}-label`}
        aria-busy={page.isFetching}
        orientation="vertical"
        value={selected?.id ?? ""}
        disabled={disabled}
        onValueChange={(value) => {
          const artifact = artifacts.find((item) => item.id === value);
          if (artifact && !disabled) onSelect(artifact);
        }}
        className="min-h-60 content-start gap-0 overflow-hidden rounded-lg border"
      >
        {page.error ? (
          <div className="p-3">
            <QueryNotice
              error={page.error}
              retry={() => {
                if (page.isFetching || disabled) return;
                void page.refetch({ cancelRefetch: false });
              }}
            />
          </div>
        ) : page.isPending ? (
          <div className="p-3">
            <QueryNotice />
          </div>
        ) : artifacts.length ? (
          artifacts.map((artifact) => (
            <label
              key={artifact.id}
              htmlFor={`${id}-${artifact.id}`}
              className="flex min-h-12 cursor-pointer items-center gap-3 border-b px-3 py-3 text-sm last:border-b-0 hover:bg-muted/50 has-disabled:cursor-not-allowed has-disabled:opacity-50 has-focus-visible:bg-muted has-data-[state=checked]:bg-primary/5"
            >
              <RadioGroupItem id={`${id}-${artifact.id}`} value={artifact.id} />
              <span className="min-w-0 break-all font-mono">
                {artifact.buildId.slice(0, 8)} / {artifact.digest.slice(0, 19)}
              </span>
            </label>
          ))
        ) : (
          <p className="p-3 text-sm text-muted-foreground">暂无产物</p>
        )}
      </RadioGroup>
      <nav
        aria-label="产物分页"
        className="flex items-center justify-between gap-1"
      >
        <Button
          type="button"
          variant="outline"
          className="h-11 px-2"
          disabled={disabled || activeIndex === 0}
          onClick={() => setPageIndex(activeIndex - 1)}
        >
          上一页
        </Button>
        <div className="flex gap-1">
          {visiblePages.map((index) => (
            <Button
              key={index}
              type="button"
              variant={index === activeIndex ? "default" : "ghost"}
              className="size-11 p-0"
              aria-label={`第 ${index + 1} 页`}
              aria-current={index === activeIndex ? "page" : undefined}
              disabled={disabled}
              onClick={() => setPageIndex(index)}
            >
              {index + 1}
            </Button>
          ))}
        </div>
        <Button
          type="button"
          variant="outline"
          className="h-11 px-2"
          disabled={!canGoNext}
          onClick={nextPage}
        >
          下一页
        </Button>
      </nav>
      {selected && (
        <section
          aria-label="已选产物"
          className="space-y-2 rounded-lg border border-primary/30 bg-primary/5 p-3"
        >
          <p className="break-all text-sm font-medium">
            已选 {selected.buildId.slice(0, 8)} / {selected.digest.slice(0, 19)}
          </p>
          <p className="runtime-code">{selected.imageReference}</p>
        </section>
      )}
    </div>
  );
}
