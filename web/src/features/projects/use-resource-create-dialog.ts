import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import type { ResourceValues } from "@/features/projects/ResourceCreateDialog";
import { useCommandKey } from "@/features/applications/releases/use-command-key";

// 项目和应用创建共享同一条命令边界：失败重试保留幂等键，关闭后重新创建则清理草稿之外的命令状态。
export function useResourceCreateDialog<T>(
  createResource: (
    values: ResourceValues,
    idempotencyKey: string,
  ) => Promise<T>,
  onCreated: (resource: T) => void,
) {
  const [open, setOpen] = useState(false);
  const commandKey = useCommandKey();
  const mutation = useMutation({
    mutationFn: (values: ResourceValues) =>
      createResource(values, commandKey.forPayload(values)),
    onSuccess(resource) {
      commandKey.clear();
      setOpen(false);
      onCreated(resource);
    },
  });
  const onOpenChange = (next: boolean) => {
    if (!next) {
      mutation.reset();
      commandKey.clear();
    }
    setOpen(next);
  };
  return {
    open,
    onOpenChange,
    onSubmit: mutation.mutate,
    pending: mutation.isPending,
    error: mutation.error,
  };
}
