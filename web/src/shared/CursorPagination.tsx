import { Button } from "@/components/ui/button";

export function CursorPagination({
  cursor,
  nextCursor,
  onChange,
  className = "",
}: {
  cursor?: string;
  nextCursor?: string;
  onChange: (cursor?: string) => void;
  className?: string;
}) {
  if (!cursor && !nextCursor) return null;
  return (
    <div className={`flex justify-end gap-2 ${className}`}>
      {cursor && (
        <Button
          type="button"
          variant="outline"
          onClick={() => onChange(undefined)}
        >
          返回第一页
        </Button>
      )}
      {nextCursor && (
        <Button
          type="button"
          variant="outline"
          onClick={() => onChange(nextCursor)}
        >
          下一页
        </Button>
      )}
    </div>
  );
}
