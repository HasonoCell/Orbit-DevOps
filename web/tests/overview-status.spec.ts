import { expect, test } from "@playwright/test";
import { diagnostic } from "./fixtures/overview";
import { runtimeStatus } from "../src/lib/overview-status";

test("运行摘要不把执行成功、不完整观测或旧版本误判为就绪", () => {
  expect(runtimeStatus(diagnostic()).tone).toBe("success");
  const partial = diagnostic(); partial.workloadObservation.metadata.status = "partial";
  expect(runtimeStatus(partial).label).toBe("运行观测不完整");
  const unavailable = diagnostic(); unavailable.workloadObservation.metadata.status = "unavailable";
  expect(runtimeStatus(unavailable).tone).toBe("neutral");
  const different = diagnostic(); different.runtimeReleaseRelation = "different";
  expect(runtimeStatus(different).label).toBe("运行其他版本");
  const missing = diagnostic(); delete missing.workloadObservation.service;
  expect(runtimeStatus(missing).tone).not.toBe("success");
  const conflicting = diagnostic(); conflicting.workloadObservation.service!.ownershipMatches = false;
  expect(runtimeStatus(conflicting).label).toBe("资源归属冲突");
  const lagging = diagnostic(); lagging.workloadObservation.deployment!.observedGeneration = 1;
  expect(runtimeStatus(lagging).label).toBe("等待控制器观测");
  const unavailableReplicas = diagnostic(); unavailableReplicas.workloadObservation.deployment!.availableReplicas = 1;
  expect(runtimeStatus(unavailableReplicas).tone).not.toBe("success");
  const noPods = diagnostic(); noPods.workloadObservation.pods = [];
  expect(runtimeStatus(noPods).tone).not.toBe("success");
});
