import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { QueryClient } from "@tanstack/react-query";
import { useCommandKey } from "@/shared/use-command-key";
import { buildQueryKeys, createBuild } from "./api";
import { runBuildCommand, type BuildCommand } from "./commands-api";

/** Build 列表同时包含制品，刷新所有相关分页即可，不另建制品选择缓存。 */
export function invalidateBuild(
  client: QueryClient,
  applicationId: string,
  buildId: string,
) {
  void client.invalidateQueries({ queryKey: buildQueryKeys.detail(buildId) });
  void client.invalidateQueries({
    queryKey: buildQueryKeys.lists(applicationId),
  });
}

export function useCreateBuild(
  applicationId: string,
  onAccepted: (result: Awaited<ReturnType<typeof createBuild>>) => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: (input: Parameters<typeof createBuild>[1]) =>
      createBuild(
        applicationId,
        input,
        key.forPayload({ applicationId, ...input }),
      ),
    onSuccess(accepted) {
      key.clear();
      // 接纳结果可能已落后于后台完成；新详情仍读取独立 Operation，不缓存接纳快照为新鲜状态。
      invalidateBuild(client, applicationId, accepted.build.id);
      onAccepted(accepted);
    },
  });
}

export function useBuildCommand(
  applicationId: string,
  buildId: string,
  operationId: string,
  onAccepted: () => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: ({
      command,
      reason,
    }: {
      command: BuildCommand;
      reason?: string;
    }) =>
      runBuildCommand(
        operationId,
        command,
        key.forPayload({ operationId, command, reason }),
        reason,
      ),
    onSuccess(operation) {
      key.clear();
      client.setQueryData(buildQueryKeys.operation(operationId), operation);
      invalidateBuild(client, applicationId, buildId);
      onAccepted();
    },
  });
}
