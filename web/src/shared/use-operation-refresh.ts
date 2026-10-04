import { useEffect, useRef } from "react";

type OperationEvidence = {
  id: string;
  status: string;
  attemptCount: number;
  automaticRetryCount?: number;
  recoveryRequired?: boolean;
  attempts?: readonly {
    id: string;
    number: number;
    status: string;
    result?: unknown;
  }[];
};

/** 心跳时间不属于详情变化；状态、尝试及尝试结果才需要重新读取详情和关联投影。 */
function signature(operation?: OperationEvidence) {
  if (!operation) return "";
  return JSON.stringify([
    operation.id,
    operation.status,
    operation.attemptCount,
    operation.automaticRetryCount,
    operation.recoveryRequired,
    operation.attempts?.map((attempt) => [
      attempt.id,
      attempt.number,
      attempt.status,
      attempt.result,
    ]),
  ]);
}

export function useOperationRefresh(
  operation: OperationEvidence | undefined,
  detail: OperationEvidence | undefined,
  error: unknown,
  refresh: () => void,
) {
  const current = signature(operation);
  const snapshot = signature(detail);
  const handled = useRef("");
  useEffect(() => {
    if (error || !current || !snapshot || handled.current === current) return;
    handled.current = current;
    // 初次独立读取若已经领先详情，也必须刷新，不能只监听下一次状态变化。
    if (current !== snapshot) refresh();
  }, [current, snapshot, error, refresh]);
}
