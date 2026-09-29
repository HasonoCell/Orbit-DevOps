import { AlertCircle, FolderOpen } from "lucide-react";
import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { errorText } from "@/api/http";

export function LoadingPage({ label = "正在加载" }: { label?: string }) {
  return (
    <div
      className="mx-auto flex min-h-72 w-full max-w-4xl flex-col gap-4 p-8"
      role="status"
      aria-label={label}
    >
      <Skeleton className="h-8 w-52" />
      <Skeleton className="h-28 w-full" />
      <Skeleton className="h-28 w-full" />
    </div>
  );
}

export function ErrorPanel({
  title,
  error,
  onRetry,
}: {
  title: string;
  error: unknown;
  onRetry?: () => void;
}) {
  return (
    <Card className="border-destructive/30 bg-destructive/5">
      <CardContent className="flex items-start gap-3 py-5">
        <AlertCircle
          className="mt-0.5 size-5 shrink-0 text-destructive"
          aria-hidden="true"
        />
        <div className="min-w-0 flex-1">
          <h2 className="font-semibold">{title}</h2>
          <p className="mt-1 text-sm text-muted-foreground">
            {errorText(error)}
          </p>
          {onRetry && (
            <Button className="mt-4" variant="outline" onClick={onRetry}>
              重试
            </Button>
          )}
        </div>
      </CardContent>
    </Card>
  );
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string;
  description: string;
  action?: ReactNode;
}) {
  return (
    <div className="flex min-h-52 flex-col items-center justify-center rounded-xl border border-dashed bg-card px-6 py-10 text-center">
      <FolderOpen className="size-8 text-muted-foreground" aria-hidden="true" />
      <h2 className="mt-4 font-semibold">{title}</h2>
      <p className="mt-1 max-w-lg text-sm text-muted-foreground">
        {description}
      </p>
      {action && <div className="mt-5">{action}</div>}
    </div>
  );
}
