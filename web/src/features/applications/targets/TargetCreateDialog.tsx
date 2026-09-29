import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { createDeploymentTarget } from "@/api/catalog";
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
import { overviewQueryKeys } from "@/features/applications/api";
import { TargetFields, useTargetForm, type TargetValues } from "./TargetForm";
import { useCommandKey } from "@/shared/use-command-key";

export function TargetCreateDialog({
  projectId,
  applicationId,
  stage,
  open,
  onOpenChange,
}: {
  projectId: string;
  applicationId: string;
  stage: "development" | "production";
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const form = useTargetForm({ replicas: 1, containerPort: 8080 });
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const commandKey = useCommandKey();
  const mutation = useMutation({
    mutationFn: (values: TargetValues) =>
      createDeploymentTarget(
        applicationId,
        { stage, ...values },
        commandKey.forPayload({ stage, ...values }),
      ),
    onSuccess(created) {
      commandKey.clear();
      void queryClient.invalidateQueries({
        queryKey: overviewQueryKeys.targets(applicationId),
      });
      onOpenChange(false);
      navigate(
        `/projects/${projectId}/applications/${applicationId}/targets/${created.id}`,
      );
    },
  });
  function changeOpen(next: boolean) {
    if (mutation.isPending) return;
    if (!next) {
      mutation.reset();
      commandKey.clear();
      form.reset();
    }
    onOpenChange(next);
  }
  return (
    <Dialog open={open} onOpenChange={changeOpen}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            创建{stage === "production" ? "生产" : "开发"}部署目标
          </DialogTitle>
          <DialogDescription>
            集群与 Namespace 由服务端确定。创建后再选择镜像发布。
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-5"
          onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
        >
          <TargetFields form={form} disabled={mutation.isPending} />
          {mutation.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{errorText(mutation.error)}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => changeOpen(false)}
            >
              取消
            </Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "正在创建…" : "创建部署目标"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
