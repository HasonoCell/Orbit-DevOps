import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
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
import { releaseQueryKeys } from "./api";
import {
  rollbackRelease,
  runReleaseCommand,
  type ReleaseCommand,
} from "./commands-api";

type Release = components["schemas"]["Release"];
type Operation = components["schemas"]["ReleaseOperation"];
type ReleaseAcceptance = components["schemas"]["ReleaseAcceptance"];
type Action = ReleaseCommand | "rollback";

const labels: Record<Action, string> = {
  retry: "重试执行",
  cancel: "取消执行",
  reconcile: "重新对账",
  "force-fail": "强制失败",
  rollback: "回滚到此发布",
};

export function ReleaseCommands({
  release,
  operation,
  projectId,
  applicationId,
  canDevelop,
  canResolveUnknown,
}: {
  release: Release;
  operation: Operation;
  projectId: string;
  applicationId: string;
  canDevelop: boolean;
  canResolveUnknown: boolean;
}) {
  const [selected, setSelected] = useState<Action | null>(null);
  const [reason, setReason] = useState("");
  const [validation, setValidation] = useState("");
  const commandKey = useCommandKey();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const mutation = useMutation({
    mutationFn: ({
      action,
      reason,
    }: {
      action: Action;
      reason?: string;
    }): Promise<Operation | ReleaseAcceptance> => {
      const key = commandKey.forPayload({
        releaseId: release.id,
        operationId: operation.id,
        action,
        reason,
      });
      return action === "rollback"
        ? rollbackRelease(release.id, key)
        : runReleaseCommand(operation.id, action, key, reason);
    },
    onSuccess(result, variables) {
      commandKey.clear();
      setSelected(null);
      setReason("");
      setValidation("");
      if (variables.action === "rollback" && "release" in result) {
        navigate(
          `/projects/${projectId}/applications/${applicationId}/releases/${result.release.id}`,
        );
      } else if ("status" in result) {
        queryClient.setQueryData(
          releaseQueryKeys.operation(operation.id),
          result,
        );
        void queryClient.invalidateQueries({
          queryKey: releaseQueryKeys.detail(release.id),
        });
        void queryClient.invalidateQueries({
          queryKey: releaseQueryKeys.diagnostics(release.id),
        });
      }
    },
  });
  const available: Action[] = [];
  if (canDevelop) {
    if (operation.status === "failed") available.push("retry");
    if (operation.status === "pending" || operation.status === "running")
      available.push("cancel");
    if (operation.status === "attention_required") available.push("reconcile");
    available.push("rollback");
  }
  if (canResolveUnknown && operation.status === "attention_required") {
    available.push("retry", "force-fail");
  }
  if (available.length === 0) return null;
  function submit() {
    if (!selected || mutation.isPending) return;
    const trimmed = reason.trim();
    if (
      selected === "force-fail" &&
      (trimmed.length < 1 || trimmed.length > 512)
    ) {
      setValidation("请输入 1–512 字的结束原因");
      return;
    }
    setValidation("");
    mutation.mutate({
      action: selected,
      ...(selected === "force-fail" ? { reason: trimmed } : {}),
    });
  }
  return (
    <section className="workbench-panel mt-5">
      <header className="panel-heading">
        <h2>执行操作</h2>
        <span className="text-xs text-muted-foreground">按状态与权限显示</span>
      </header>
      <div className="flex flex-wrap gap-2 p-5">
        {available.map((action) => (
          <Button
            key={action}
            variant="outline"
            onClick={() => {
              mutation.reset();
              setSelected(action);
            }}
          >
            {labels[action]}
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
                "重新排队同一 Operation。结果未知时，服务端会先确认外部状态是否安全。"}
              {selected === "cancel" &&
                "取消排队中的发布，或请求 Worker 停止正在执行的发布。"}
              {selected === "reconcile" &&
                "只读检查 Kubernetes 权威状态，并更新执行结论。"}
              {selected === "force-fail" &&
                "人工结束结果未知的执行，解除目标阻塞；不会撤销可能已发生的集群写入。"}
              {selected === "rollback" &&
                "以此历史 Release 的不可变快照创建新 Release，替换目标当前运行版本。接纳不等于部署完成。"}
            </DialogDescription>
          </DialogHeader>
          <p className="break-all rounded bg-muted p-3 text-xs">
            目标：{release.targetSnapshot.stage} / {release.deploymentTargetId}
            <br />
            来源 Release：{release.id}
          </p>
          {selected === "force-fail" && (
            <div className="space-y-2">
              <Label htmlFor="release-fail-reason">结束原因</Label>
              <Input
                id="release-fail-reason"
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
