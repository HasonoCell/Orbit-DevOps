import { Fragment, useEffect, useState } from "react";
import { useOutletContext } from "react-router-dom";
import type { CurrentPrincipal } from "@/api/http";
import { Button } from "@/components/ui/button";

function readCursors(key: string): string[] {
  try {
    const saved: unknown = JSON.parse(sessionStorage.getItem(key) ?? "null");
    if (
      Array.isArray(saved) &&
      saved[0] === "" &&
      saved.slice(1).every((cursor) => typeof cursor === "string" && cursor)
    ) {
      return saved;
    }
  } catch {
    // 浏览器阻止会话存储时，仍可从第一页重新翻页。
  }
  return [""];
}

/** API 只返回下一页游标；页码仅对应本会话已发现的游标。 */
export function ProjectPagination({
  projectId,
  cursor,
  nextCursor,
  onChange,
}: {
  projectId: string;
  cursor?: string;
  nextCursor?: string;
  onChange: (cursor: string | undefined, page: number) => void;
}) {
  const principal = useOutletContext<CurrentPrincipal>();
  const key = `orbit:application-pages:${principal.user?.id ?? "unknown"}:${projectId}`;
  const [cursors, setCursors] = useState(() => readCursors(key));
  const currentIndex = cursors.indexOf(cursor ?? "");

  useEffect(() => {
    setCursors((known) => {
      const index = known.indexOf(cursor ?? "");
      if (index < 0 || known[index + 1] === nextCursor) return known;
      const updated = known.slice(0, index + 1);
      if (nextCursor) updated.push(nextCursor);
      return updated;
    });
  }, [cursor, nextCursor]);

  useEffect(() => {
    try {
      sessionStorage.setItem(key, JSON.stringify(cursors));
    } catch {
      // 存储不可用时，刷新后页码从第一页重新发现。
    }
  }, [key, cursors]);

  if (currentIndex < 0) {
    return (
      <Button
        variant="outline"
        size="sm"
        onClick={() => onChange(undefined, 1)}
      >
        返回第一页
      </Button>
    );
  }
  if (cursors.length === 1 && !nextCursor) return null;
  const visibleIndices = cursors
    .map((_, index) => index)
    .filter(
      (index) =>
        index === 0 ||
        index === cursors.length - 1 ||
        Math.abs(index - currentIndex) <= 1,
    );

  return (
    <nav aria-label="应用分页" className="flex flex-wrap items-center gap-2">
      <Button
        variant="outline"
        size="sm"
        disabled={currentIndex === 0}
        onClick={() =>
          onChange(cursors[currentIndex - 1] || undefined, currentIndex)
        }
      >
        上一页
      </Button>
      {visibleIndices.map((index, position) => (
        <Fragment key={index}>
          {position > 0 && index > visibleIndices[position - 1] + 1 && (
            <span aria-hidden="true" className="px-1 text-muted-foreground">
              …
            </span>
          )}
          <Button
            variant={index === currentIndex ? "default" : "outline"}
            size="sm"
            aria-label={`第 ${index + 1} 页`}
            aria-current={index === currentIndex ? "page" : undefined}
            disabled={index === currentIndex}
            onClick={() => onChange(cursors[index] || undefined, index + 1)}
          >
            {index + 1}
          </Button>
        </Fragment>
      ))}
      <Button
        variant="outline"
        size="sm"
        disabled={!nextCursor || cursors[currentIndex + 1] !== nextCursor}
        onClick={() => onChange(nextCursor, currentIndex + 2)}
      >
        下一页
      </Button>
    </nav>
  );
}
