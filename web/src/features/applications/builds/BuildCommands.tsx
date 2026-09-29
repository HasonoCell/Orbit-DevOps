import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import type { components } from "@/api/schema";
import { errorText } from "@/api/http";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useCommandKey } from "@/shared/use-command-key";
import { buildQueryKeys } from "./api";
import { runBuildCommand, type BuildCommand } from "./commands-api";

type Operation = components["schemas"]["BuildOperation"];

const labels: Record<BuildCommand, string> = {
  retry: "重试构建",
  cancel: "取消构建",
  reconcile: "重新对账",
  "force-fail": "强制失败",
};

export function BuildCommands({
  operation,
  buildId,
  canDevelop,
  canResolveUnknown,
}: {
  operation: Operation;
  buildId: string;
  canDevelop: boolean;
  canResolveUnknown: boolean;
}) {
  const [selected, setSelected] = useState<BuildCommand | null>(null);
  const [reason, setReason] = useState("");
  const [validation, setValidation] = useState("");
  const commandKey = useCommandKey();
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: ({
      command,
      reason,
    }: {
      command: BuildCommand;
      reason?: string;
    }) =>
      runBuildCommand(
        operation.id,
        command,
        commandKey.forPayload({ operationId: operation.id, command, reason }),
        reason,
      ),
    onSuccess(updated) {
      commandKey.clear();
      queryClient.setQueryData(buildQueryKeys.operation(operation.id), updated);
      void queryClient.invalidateQueries({
        queryKey: buildQueryKeys.detail(buildId),
      });
      setSelected(null);
      setReason("");
      setValidation("");
    },
  });
  const available: BuildCommand[] = [];
  if (canDevelop) {
    if (operation.status === "failed") available.push("retry");
    if (operation.status === "pending" || operation.status === "running")
      available.push("cancel");
    if (operation.status === "attention_required") available.push("reconcile");
  }
  if (canResolveUnknown && operation.status === "attention_required")
    available.push("force-fail");
  if (available.length === 0) return null;

  function submit() {
    if (!selected || mutation.isPending) return;
    const trimmed = reason.trim();
    if (
      selected === "force-fail" &&
      (trimmed.length === 0 || trimmed.length > 512)
    ) {
      setValidation("请输入 1–512 字的结束原因");
      return;
    }
    setValidation("");
    mutation.mutate({
      command: selected,
      ...(selected === "force-fail" ? { reason: trimmed } : {}),
    });
  }
  return (
    <section className="workbench-panel mt-5">
      <header className="panel-heading">
        <h2>执行操作</h2>
        <span className="text-xs text-muted-foreground">
          按当前状态显示可用命令
        </span>
      </header>
      <div className="flex flex-wrap gap-2 p-5">
        {available.map((command) => (
          <Button
            key={command}
            variant="outline"
            onClick={() => {
              mutation.reset();
              setSelected(command);
            }}
          >
            {labels[command]}
          </Button>
        ))}
      </div>
      <Dialog
        open={selected !== null}
        onOpenChange={(open) => {
          if (!open && !mutation.isPending) setSelected(null);
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{selected && labels[selected]}</DialogTitle>
            <DialogDescription>
              {selected === "retry" &&
                "将使用原 Build 的冻结输入重新排队，不创建新 Build。"}
              {selected === "cancel" &&
                "取消排队中的构建，或请求 Worker 停止运行中的 Job。"}
              {selected === "reconcile" &&
                "先读取已有 Job 的权威结果，再决定是否继续执行。"}
              {selected === "force-fail" &&
                "人工结束结果未知的构建；不会清理旧 Job 证据。"}
            </DialogDescription>
          </DialogHeader>
          <p className="break-all rounded bg-muted p-3 text-xs">
            Build {buildId}
            <br />
            Operation {operation.id}
          </p>
          {selected === "force-fail" && (
            <div className="space-y-2">
              <Label htmlFor="build-force-reason">结束原因</Label>
              <Input
                id="build-force-reason"
                value={reason}
                maxLength={512}
                onChange={(event) => setReason(event.target.value)}
              />
            </div>
          )}
          {validation && (
            <p role="alert" className="text-sm text-destructive">
              {validation}
            </p>
          )}
          {mutation.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>
                {errorText(mutation.error)}
                。结果未确认；保持相同输入重试会复用幂等键。
              </AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => setSelected(null)}
            >
              返回
            </Button>
            <Button disabled={mutation.isPending} onClick={submit}>
              {mutation.isPending
                ? "正在提交…"
                : selected
                  ? `确认${labels[selected]}`
                  : "确认"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  );
}
