import { Server } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import type { DiagnosticReport } from "@/features/applications/api";
import { QuietEmpty, StatusPill, Timestamp } from "@/shared/OverviewUI";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";

/** Pod 完整信息在受控抽屉展开；不截断错误证据，不把缓存日志当作实时日志。 */
export function PodList({ report }: { report: DiagnosticReport }) {
  return (
    <section className="workbench-panel">
      <header className="panel-heading">
        <h2>
          Pods{" "}
          <span className="ml-1 font-normal text-muted-foreground">
            {report.workloadObservation.pods.length}
          </span>
        </h2>
        <Server className="size-4 text-muted-foreground" aria-hidden="true" />
      </header>
      <div className="divide-y">
        {report.workloadObservation.pods.map((pod) => (
          <article key={pod.uid} className="pod-row">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <Sheet>
                <SheetTrigger asChild>
                  <Button
                    type="button"
                    variant="link"
                    className="h-auto min-w-0 shrink cursor-pointer justify-start p-0 text-left font-mono text-xs font-normal whitespace-normal [overflow-wrap:anywhere] max-md:min-h-11"
                  >
                    {pod.name}
                  </Button>
                </SheetTrigger>
                <SheetContent className="overflow-y-auto sm:max-w-xl">
                  <SheetHeader>
                    <SheetTitle>Pod 详情</SheetTitle>
                    <SheetDescription>
                      只读观测于{" "}
                      <Timestamp
                        value={report.workloadObservation.metadata.observedAt}
                      />
                    </SheetDescription>
                  </SheetHeader>
                  <div className="space-y-5 p-5">
                    <p className="runtime-code">{pod.name}</p>
                    <dl className="grid grid-cols-2 gap-3 text-xs">
                      <dt>UID</dt>
                      <dd className="break-all">{pod.uid}</dd>
                      <dt>创建时间</dt>
                      <dd>
                        <Timestamp value={pod.createdAt} />
                      </dd>
                    </dl>
                    {pod.containers.map((container) => (
                      <section
                        key={container.name}
                        className="rounded border p-4 text-xs"
                      >
                        <h3 className="font-semibold">{container.name}</h3>
                        <p className="mt-2">
                          {container.state} ·{" "}
                          {container.ready ? "Ready" : "Not Ready"} · 重启{" "}
                          {container.restartCount}
                        </p>
                        <p className="mt-2 break-words">
                          {container.reason} {container.message}
                        </p>
                        {container.exitCode !== undefined && (
                          <p>退出码 {container.exitCode}</p>
                        )}
                        {container.previousTermination && (
                          <Collapsible className="mt-3">
                            <CollapsibleTrigger className="text-primary">
                              上次退出
                            </CollapsibleTrigger>
                            <CollapsibleContent>
                              <p className="mt-2 break-words">
                                {container.previousTermination.reason} ·{" "}
                                {container.previousTermination.exitCode} ·{" "}
                                {container.previousTermination.message}
                              </p>
                            </CollapsibleContent>
                          </Collapsible>
                        )}
                      </section>
                    ))}
                  </div>
                </SheetContent>
              </Sheet>
              <StatusPill
                label={pod.reason || pod.phase}
                tone={pod.ready ? "success" : "warning"}
              />
            </div>
            <div className="mt-3 flex flex-wrap gap-x-5 gap-y-2 text-xs text-muted-foreground">
              <span>{pod.ready ? "Ready" : "Not Ready"}</span>
              <span>
                重启{" "}
                {pod.containers.reduce(
                  (sum, container) => sum + container.restartCount,
                  0,
                )}
              </span>
              {pod.containers
                .filter((container) => container.reason)
                .map((container) => (
                  <span
                    key={container.name}
                    className="break-all text-amber-900"
                  >
                    {container.name}：{container.reason}
                  </span>
                ))}
            </div>
          </article>
        ))}
      </div>
      {report.workloadObservation.pods.length === 0 && (
        <QuietEmpty>
          {report.workloadObservation.metadata.status === "complete"
            ? "此次观测未返回 Pod。"
            : "Pod 观测不完整，不能确认工作负载不存在。"}
        </QuietEmpty>
      )}
    </section>
  );
}
