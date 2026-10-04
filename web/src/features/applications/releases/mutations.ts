import {
  useMutation,
  useQueryClient,
  type QueryClient,
} from "@tanstack/react-query";
import { useCommandKey } from "@/shared/use-command-key";
import { acceptRelease, releaseQueryKeys } from "./api";
import {
  rollbackRelease,
  runReleaseCommand,
  type ReleaseCommand,
} from "./commands-api";

export function invalidateRelease(
  client: QueryClient,
  targetId: string,
  releaseId: string,
) {
  void client.invalidateQueries({
    queryKey: releaseQueryKeys.detail(releaseId),
  });
  void client.invalidateQueries({ queryKey: releaseQueryKeys.lists(targetId) });
  void client.invalidateQueries({
    queryKey: releaseQueryKeys.diagnostics(releaseId),
  });
}

export function useCreateRelease(
  targetId: string,
  onAccepted: (result: Awaited<ReturnType<typeof acceptRelease>>) => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: (body: Parameters<typeof acceptRelease>[1]) =>
      acceptRelease(targetId, body, key.forPayload({ targetId, ...body })),
    onSuccess(accepted) {
      key.clear();
      invalidateRelease(client, targetId, accepted.release.id);
      onAccepted(accepted);
    },
  });
}

export type ReleaseAction = ReleaseCommand | "rollback";
type CommandResult =
  | Awaited<ReturnType<typeof rollbackRelease>>
  | Awaited<ReturnType<typeof runReleaseCommand>>;
type CommandInput = { action: ReleaseAction; reason?: string };
export function useReleaseCommand(
  targetId: string,
  releaseId: string,
  operationId: string,
  onAccepted: (result: CommandResult, variables: CommandInput) => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: ({ action, reason }: CommandInput): Promise<CommandResult> => {
      const idempotencyKey = key.forPayload({
        releaseId,
        operationId,
        action,
        reason,
      });
      return action === "rollback"
        ? rollbackRelease(releaseId, idempotencyKey)
        : runReleaseCommand(operationId, action, idempotencyKey, reason);
    },
    onSuccess(result, variables) {
      key.clear();
      if ("release" in result) {
        invalidateRelease(client, targetId, result.release.id);
      } else {
        client.setQueryData(releaseQueryKeys.operation(operationId), result);
        invalidateRelease(client, targetId, releaseId);
      }
      onAccepted(result, variables);
    },
  });
}
