import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useCommandKey } from "@/shared/use-command-key";
import {
  changePipelineState,
  createPipeline,
  pipelineKeys,
  reconcileRun,
  updatePipeline,
  type PipelineInput,
} from "./pipeline-api";

type Detail = Awaited<ReturnType<typeof createPipeline>>;
export function useWritePipeline(
  applicationId: string,
  pipelineId: string,
  expectedRevision: number | undefined,
  onAccepted: (detail: Detail) => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: (body: PipelineInput) => {
      const token = key.forPayload({
        applicationId,
        pipelineId,
        body,
        expectedRevision,
      });
      if (pipelineId === "new")
        return createPipeline(applicationId, body, token);
      if (expectedRevision === undefined)
        throw new Error("请先读取当前 Pipeline 修订");
      return updatePipeline(
        pipelineId,
        {
          expectedRevision,
          endpointKey: body.endpointKey,
          repositoryUrl: body.repositoryUrl,
          branch: body.branch,
          dockerfilePath: body.dockerfilePath,
          contextPath: body.contextPath,
          mode: body.mode,
          deploymentTargetId: body.deploymentTargetId,
        },
        token,
      );
    },
    onSuccess(detail) {
      key.clear();
      client.setQueryData(pipelineKeys.detail(detail.pipeline.id), detail);
      void client.invalidateQueries({
        queryKey: pipelineKeys.lists(applicationId),
      });
      onAccepted(detail);
    },
  });
}

export function usePipelineState(
  applicationId: string,
  pipelineId: string,
  revision: number,
  onAccepted: () => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: (action: "enable" | "disable") =>
      changePipelineState(
        pipelineId,
        action,
        key.forPayload({ pipelineId, action, revision }),
      ),
    onSuccess(detail) {
      key.clear();
      client.setQueryData(pipelineKeys.detail(pipelineId), detail);
      void client.invalidateQueries({
        queryKey: pipelineKeys.lists(applicationId),
      });
      onAccepted();
    },
  });
}

export function useReconcileRun(
  pipelineId: string,
  runId: string,
  onAccepted: () => void,
) {
  const client = useQueryClient();
  const key = useCommandKey();
  return useMutation({
    mutationFn: () => reconcileRun(runId, key.forPayload({ runId })),
    onSuccess(run) {
      key.clear();
      client.setQueryData(pipelineKeys.run(runId), run);
      void client.invalidateQueries({
        queryKey: pipelineKeys.detail(pipelineId),
      });
      onAccepted();
    },
  });
}
