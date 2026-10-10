import { errorText } from "@/api/http";
import type { components } from "@/api/schema";
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
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { type ReleaseCommand } from "./commands-api";
import { useReleaseCommand } from "./mutations";

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
  onAccepted,
}: {
  release: Release;
  operation: Operation;
  projectId: string;
  applicationId: string;
  canDevelop: boolean;
  canResolveUnknown: boolean;
  onAccepted: () => void;
}) {
  const [selected, setSelected] = useState<Action | null>(null);
  const [reason, setReason] = useState("");
  const [validation, setValidation] = useState("");
  const navigate = useNavigate();
  const mutation = useReleaseCommand(
    release.deploymentTargetId,
    release.id,
    operation.id,
    (result, variables) => {
      setSelected(null);
      setReason("");
      setValidation("");
      if (variables.action === "rollback" && "release" in result) {
        navigate(
          `/projects/${projectId}/applications/${applicationId}/releases/${result.release.id}`,
        );
      } else if ("status" in result) onAccepted();
    },
  );
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
              {selected === "retry" && "重试此执行；结果未知时先核验外部状态。"}
              {selected === "cancel" &&
                "取消排队发布，或请求停止当前执行；不会回退已写入的资源。"}
              {selected === "reconcile" &&
                "读取 Kubernetes 状态并更新执行结论。"}
              {selected === "force-fail" &&
                "结束未知执行并解除阻塞；不会撤销已有集群写入。"}
              {selected === "rollback" &&
                "按此历史快照创建新发布，替换目标当前运行版本。"}
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
              <AlertDescription>{errorText(mutation.error)}</AlertDescription>
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
