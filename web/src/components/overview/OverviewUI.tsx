import type { ReactNode } from "react";
import { AlertCircle, LoaderCircle } from "lucide-react";
import { errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import type { Status } from "@/lib/overview-status";

const toneClass = {
  neutral: "bg-slate-100 text-slate-600",
  success: "bg-emerald-50 text-emerald-700",
  progress: "bg-blue-50 text-blue-700",
  warning: "bg-amber-50 text-amber-800",
  danger: "bg-red-50 text-red-700",
};

export function StatusPill({ label, tone }: Status) {
  return <span className={`inline-flex max-w-full items-center gap-1.5 rounded px-2 py-1 text-xs font-medium ${toneClass[tone]}`}><span className="size-1.5 shrink-0 rounded-full bg-current" />{label}</span>;
}

export function Panel({ title, icon, children, subtitle }: { title: string; icon: ReactNode; children: ReactNode; subtitle?: string }) {
  return <section aria-label={title} className="min-w-0 overflow-hidden rounded-lg border"><header className="flex items-center gap-2.5 border-b bg-muted/40 px-5 py-4"><span className="text-muted-foreground">{icon}</span><h2 className="text-sm font-semibold">{title}</h2>{subtitle && <span className="ml-auto text-xs text-muted-foreground">{subtitle}</span>}</header>{children}</section>;
}

export function QueryNotice({ error, retry }: { error?: unknown; retry?: () => void }) {
  if (error) return <div role="alert" className="space-y-2 p-5 text-sm"><p className="flex items-start gap-2 text-destructive"><AlertCircle className="mt-0.5 size-4 shrink-0" />{errorText(error)}</p>{retry && <Button size="sm" variant="outline" onClick={retry}>重试</Button>}</div>;
  return <div role="status" className="flex items-center gap-2 p-5 text-sm text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />正在加载…</div>;
}

export function QuietEmpty({ children }: { children: ReactNode }) {
  return <p className="px-5 py-10 text-center text-sm leading-6 text-muted-foreground">{children}</p>;
}

export function Fact({ label, children }: { label: string; children: ReactNode }) {
  return <div className="min-w-0"><dt className="text-xs text-muted-foreground">{label}</dt><dd className="mt-1.5 break-all text-sm">{children}</dd></div>;
}

export function Timestamp({ value }: { value: string }) {
  return <time dateTime={value} title={new Date(value).toLocaleString("zh-CN")}>{new Date(value).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false })}</time>;
}
