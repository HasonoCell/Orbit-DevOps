import { createHash } from "node:crypto";
import { diagnostic } from "../tests/fixtures/overview.ts";
import { choice, ok, text, type Handler } from "./http.ts";
import {
  copy,
  fail,
  find,
  id,
  now,
  pageOf,
  type DemoStore,
  type Operation,
  type Outcome,
  type Schema,
} from "./state.ts";

function operationFields(actor: string) {
  return {
    id: id(),
    createdBy: actor,
    idempotencyKey: id(),
    status: "pending" as const,
    attemptCount: 0,
    automaticRetryCount: 0,
    recoveryRequired: false,
    queuedAt: now(),
    availableAt: now(),
    createdAt: now(),
    updatedAt: now(),
    attempts: [],
  };
}
export function createBuild(
  s: DemoStore,
  appId: string,
  input: Schema["CreateBuildRequest"],
  actor: string,
): Schema["BuildAcceptance"] {
  const app = find(s.applications, appId);
  const repository = new URL(input.repositoryUrl);
  if (
    repository.protocol !== "https:" ||
    repository.hostname !== "github.com" ||
    repository.username ||
    repository.password ||
    !/^(?:[a-f\d]{40}|[a-f\d]{64})$/i.test(input.sourceCommit)
  )
    fail(
      400,
      "invalid_build_source",
      "需要 GitHub HTTPS 仓库和完整 Commit SHA",
    );
  const build: Schema["Build"] = {
    id: id(),
    projectId: app.projectId,
    applicationId: appId,
    ...input,
    platform: "linux/amd64",
    destinationRepository: `registry.example.test/${find(s.projects, app.projectId).slug}/${app.slug}`,
    createdBy: actor,
    createdAt: now(),
  };
  const acceptance: Schema["BuildAcceptance"] = {
    build,
    buildOperation: { ...operationFields(actor), buildId: build.id },
  };
  s.builds.unshift(acceptance);
  s.jobs.set(acceptance.buildOperation.id, {
    kind: "build",
    operationId: acceptance.buildOperation.id,
    outcome: s.nextBuild,
    started: Date.now(),
  });
  s.nextBuild = "success";
  return acceptance;
}
export function createRelease(
  s: DemoStore,
  targetId: string,
  input: Schema["CreateReleaseRequest"],
  actor: string,
  snapshot?: Schema["ReleaseTargetSnapshot"],
  rollbackOfReleaseId?: string,
): Schema["ReleaseDetail"] {
  const target = find(s.targets, targetId);
  const app = find(s.applications, target.applicationId);
  if (!/@sha256:[a-f\d]{64}$/i.test(input.imageReference))
    fail(
      400,
      "invalid_image_reference",
      "发布需要包含 sha256 Digest 的镜像引用",
    );
  if (input.imageArtifactId) {
    const artifact = s.builds.find(
      (b) => b.imageArtifact?.id === input.imageArtifactId,
    )?.imageArtifact;
    if (
      !artifact ||
      artifact.applicationId !== app.id ||
      artifact.projectId !== app.projectId ||
      artifact.imageReference !== input.imageReference
    )
      fail(409, "artifact_conflict", "产物不属于当前应用或镜像不匹配");
  }
  const release: Schema["Release"] = {
    id: id(),
    deploymentTargetId: targetId,
    ...input,
    targetSnapshot: snapshot ?? {
      projectId: app.projectId,
      applicationId: app.id,
      stage: target.stage,
      clusterRef: target.clusterRef,
      namespace: target.namespace,
      replicas: target.replicas,
      containerPort: target.containerPort,
    },
    ...(rollbackOfReleaseId ? { rollbackOfReleaseId } : {}),
    createdBy: actor,
    createdAt: now(),
  };
  const detail: Schema["ReleaseDetail"] = {
    release,
    releaseOperation: {
      ...operationFields(actor),
      type: "release.deploy",
      releaseId: release.id,
      deploymentTargetId: targetId,
    },
    snapshotDifferences: [],
    auditTimeline: [
      {
        id: id(),
        actorId: actor,
        actorKind: "user",
        action: rollbackOfReleaseId ? "release.rollback" : "release.create",
        targetType: "release",
        targetId: release.id,
        summary: {},
        createdAt: now(),
      },
    ],
  };
  s.releases.unshift(detail);
  s.jobs.set(detail.releaseOperation.id, {
    kind: "release",
    operationId: detail.releaseOperation.id,
    outcome: s.nextRelease,
    started: Date.now(),
  });
  s.nextRelease = "success";
  return detail;
}
function artifact(s: DemoStore, buildId: string) {
  const b = s.builds.find((item) => item.build.id === buildId)!;
  if (b.imageArtifact) return;
  // 仅为界面生成确定格式的虚构 OCI 身份，不来自真实构建或 Manifest。
  const digest =
    "sha256:" +
    createHash("sha256")
      .update(b.build.sourceCommit + b.build.id)
      .digest("hex");
  b.imageArtifact = {
    id: id(),
    buildId,
    projectId: b.build.projectId,
    applicationId: b.build.applicationId,
    repository: b.build.destinationRepository,
    digest,
    imageReference: `${b.build.destinationRepository}@${digest}`,
    platform: b.build.platform,
    createdBy: b.build.createdBy,
    createdAt: now(),
  };
}
function finish(s: DemoStore, operation: Operation, outcome: Outcome) {
  const status =
    outcome === "unknown"
      ? "attention_required"
      : outcome === "failed"
        ? "failed"
        : "succeeded";
  operation.status = status;
  operation.updatedAt = now();
  operation.finishedAt = now();
  operation.recoveryRequired = outcome === "unknown";
  if (outcome !== "success") {
    operation.errorCode =
      outcome === "unknown" ? "outcome_unknown" : "mock_executor_failed";
    operation.errorSummary =
      outcome === "unknown"
        ? "模拟 Worker 失联，执行结果尚未确认"
        : "模拟执行失败，请检查构建或部署配置后重试";
    operation.retryDisposition =
      outcome === "unknown" ? "unknown_outcome" : "non_retryable";
  } else {
    delete operation.errorCode;
    delete operation.errorSummary;
    delete operation.retryDisposition;
  }
  const attempt = operation.attempts.at(-1);
  if (attempt?.status === "running")
    Object.assign(attempt, {
      status: outcome === "unknown" ? "outcome_unknown" : status,
      finishedAt: now(),
      errorCode: operation.errorCode,
      errorSummary: operation.errorSummary,
    });
  if (outcome === "success") {
    if ("buildId" in operation) artifact(s, operation.buildId);
    else s.runtime.set(operation.deploymentTargetId, operation.releaseId);
  }
  s.jobs.delete(operation.id);
}

/** 使用时间驱动同一份记录；查询频率不决定任务进度，不依赖真实 Worker、队列或网络。 */
export function advance(s: DemoStore) {
  for (const job of s.jobs.values()) {
    const operation =
      job.kind === "build"
        ? s.builds.find((b) => b.buildOperation.id === job.operationId)
            ?.buildOperation
        : s.releases.find((r) => r.releaseOperation.id === job.operationId)
            ?.releaseOperation;
    if (!operation) {
      s.jobs.delete(job.operationId);
      continue;
    }
    if (operation.status === "cancel_requested") {
      operation.status = "canceled";
      operation.finishedAt = now();
      operation.updatedAt = now();
      const last = operation.attempts.at(-1);
      if (last?.status === "running") {
        last.status = "canceled";
        last.finishedAt = now();
      }
      s.jobs.delete(job.operationId);
      continue;
    }
    // 同 Target 的活动操作保持串行；等待时不消耗模拟执行时间。
    if (
      "deploymentTargetId" in operation &&
      [...s.jobs.values()]
        .slice(0, [...s.jobs.values()].indexOf(job))
        .some(
          (other) =>
            other !== job &&
            other.kind === "release" &&
            s.releases.some(
              (r) =>
                r.releaseOperation.id === other.operationId &&
                r.release.deploymentTargetId === operation.deploymentTargetId,
            ),
        )
    ) {
      job.started = Date.now();
      continue;
    }
    if (
      "deploymentTargetId" in operation &&
      s.releases.some(
        (r) =>
          r.releaseOperation.id !== operation.id &&
          r.release.deploymentTargetId === operation.deploymentTargetId &&
          r.releaseOperation.status === "attention_required",
      )
    ) {
      job.started = Date.now();
      continue;
    }
    const elapsed = Date.now() - job.started;
    if (operation.status === "pending" && elapsed >= s.stepMs) {
      operation.status = "running";
      operation.startedAt = now();
      operation.updatedAt = now();
      operation.attemptCount++;
      operation.attempts.push({
        id: id(),
        number: operation.attemptCount,
        workerId: `mock-${job.kind}-worker`,
        status: "running",
        startedAt: now(),
      });
    }
    if (
      operation.status === "running" &&
      job.outcome !== "hold" &&
      elapsed >= 2 * s.stepMs
    )
      finish(s, operation, job.outcome);
  }
  for (const detail of s.runs) {
    const { run } = detail;
    if (run.phase === "superseded" || run.phase === "blocked") continue;
    const b = s.builds.find((item) => item.build.id === run.buildId)!;
    const status = b.buildOperation.status;
    if (status !== "succeeded") {
      detail.status =
        status === "failed"
          ? "build_failed"
          : status === "canceled"
            ? "build_canceled"
            : status === "attention_required"
              ? "attention_required"
              : "building";
      detail.activeStage = "build";
      continue;
    }
    run.imageArtifactId = b.imageArtifact?.id;
    if (detail.mode === "build_only") {
      detail.status = "candidate_ready";
      run.phase = "completed";
      run.finishedAt ??= now();
      delete detail.activeStage;
      continue;
    }
    if (!run.releaseId) {
      const p = find(
        s.pipelines.map((p) => p.pipeline),
        run.deliveryPipelineId,
      );
      if (
        !p.enabled ||
        p.currentRevision !== run.pipelineRevision ||
        p.activationGeneration !== run.activationGeneration
      ) {
        run.phase = "superseded";
        detail.status = "superseded";
        run.reasonCode = "pipeline_changed";
        delete detail.activeStage;
        continue;
      }
      if (run.phase === "build_created") {
        run.phase = "artifact_ready";
        run.phaseVersion++;
        run.updatedAt = now();
        detail.status = "verifying_source";
        detail.activeStage = "source_verification";
        continue;
      }
      if (Date.now() - Date.parse(run.updatedAt) < s.stepMs) continue;
      const spec = s.runSpecs.get(run.id)!;
      const r = createRelease(
        s,
        spec.deploymentTargetId!,
        {
          imageReference: b.imageArtifact!.imageReference,
          imageArtifactId: b.imageArtifact!.id,
        },
        p.createdBy,
      );
      run.releaseId = r.release.id;
      run.phase = "release_created";
      run.phaseVersion++;
      run.updatedAt = now();
    }
    const r = s.releases.find((item) => item.release.id === run.releaseId)!;
    detail.activeStage = "release";
    detail.status =
      r.releaseOperation.status === "succeeded"
        ? "succeeded"
        : r.releaseOperation.status === "failed"
          ? "release_failed"
          : r.releaseOperation.status === "canceled"
            ? "release_canceled"
            : r.releaseOperation.status === "attention_required"
              ? "attention_required"
              : "releasing";
    if (
      ["succeeded", "release_failed", "release_canceled"].includes(
        detail.status,
      )
    ) {
      run.phase = "completed";
      run.finishedAt ??= now();
      delete detail.activeStage;
    }
  }
}

export function triggerPush(
  s: DemoStore,
  pipelineId: string,
  sourceCommit = createHash("sha1").update(id()).digest("hex"),
) {
  const p = find(
    s.pipelines.map((p) => ({ ...p, id: p.pipeline.id })),
    pipelineId,
  );
  if (!p.pipeline.enabled) fail(409, "pipeline_disabled", "请先启用 Pipeline");
  const b = createBuild(
    s,
    p.pipeline.applicationId,
    {
      repositoryUrl: p.revision.repositoryUrl,
      sourceCommit,
      dockerfilePath: p.revision.dockerfilePath,
      contextPath: p.revision.contextPath,
    },
    p.pipeline.createdBy,
  );
  const detail: Schema["DeliveryRunDetail"] = {
    run: {
      id: id(),
      deliveryPipelineId: pipelineId,
      pipelineRevision: p.revision.revision,
      activationGeneration: p.pipeline.activationGeneration,
      sourceCommit,
      repositoryUrl: p.revision.repositoryUrl,
      phase: "build_created",
      phaseVersion: 1,
      buildId: b.build.id,
      createdAt: now(),
      updatedAt: now(),
    },
    mode: p.revision.mode,
    status: "building",
    activeStage: "build",
    trigger: {
      eventType: "push",
      repositoryFullName: p.revision.repositoryFullName,
      gitRef: p.revision.gitRef,
      forced: false,
      receivedAt: now(),
    },
  };
  s.runs.unshift(detail);
  s.runSpecs.set(detail.run.id, copy(p.revision));
  return detail;
}
function differences(s: DemoStore, release: Schema["Release"]) {
  const t = find(s.targets, release.deploymentTargetId);
  return (["replicas", "containerPort"] as const)
    .filter((key) => t[key] !== release.targetSnapshot[key])
    .map((key) => ({
      field: key,
      releaseValue: String(release.targetSnapshot[key]),
      currentValue: String(t[key]),
    }));
}
function report(
  s: DemoStore,
  r: Schema["ReleaseDetail"],
): Schema["ReleaseDiagnosticReport"] {
  const d = diagnostic();
  const currentId = s.runtime.get(r.release.deploymentTargetId);
  const current = s.releases.find((item) => item.release.id === currentId);
  d.release = r.release;
  d.releaseOperation = r.releaseOperation;
  d.targetDifferences = differences(s, r.release);
  d.generatedAt = now();
  d.runtimeReleaseRelation = currentId
    ? currentId === r.release.id
      ? "matches"
      : "different"
    : "absent";
  d.workloadObservation.metadata.observedAt = now();
  d.eventObservation.metadata.observedAt = now();
  if (!current || s.evidence === "controller_unavailable") {
    delete d.workloadObservation.deployment;
    delete d.workloadObservation.service;
    d.workloadObservation.pods = [];
    if (s.evidence === "controller_unavailable") {
      d.workloadObservation.metadata.status = "unavailable";
      d.workloadObservation.metadata.errorCategories = [
        "mock_controller_unavailable",
      ];
      d.runtimeReleaseRelation = "unknown";
    }
    return d;
  }
  const app = find(
    s.applications,
    current.release.targetSnapshot.applicationId,
  );
  const replicas = current.release.targetSnapshot.replicas;
  Object.assign(d.workloadObservation.deployment!, {
    name: app.slug,
    releaseId: currentId,
    desiredReplicas: replicas,
    updatedReplicas: replicas,
    readyReplicas: s.evidence === "pod_crash" ? 0 : replicas,
    availableReplicas: s.evidence === "pod_crash" ? 0 : replicas,
  });
  d.workloadObservation.service!.name = app.slug;
  d.workloadObservation.service!.ports = [
    {
      name: "http",
      protocol: "TCP",
      port: current.release.targetSnapshot.containerPort,
    },
  ];
  d.workloadObservation.pods = Array.from({ length: replicas }, (_, index) => ({
    name: `${app.slug}-${current.release.id.slice(0, 8)}-${index}`,
    uid: `mock-pod-${currentId}-${index}`,
    createdAt: current.release.createdAt,
    phase: "Running",
    ready: s.evidence !== "pod_crash",
    reason: s.evidence === "pod_crash" ? "CrashLoopBackOff" : "",
    containers: [
      {
        name: "app",
        ready: s.evidence !== "pod_crash",
        restartCount: s.evidence === "pod_crash" ? 5 : 0,
        state: s.evidence === "pod_crash" ? "waiting" : "running",
        reason: s.evidence === "pod_crash" ? "CrashLoopBackOff" : "",
        message: "",
      },
    ],
  }));
  return d;
}

export const deliveryHandler: Handler = (ctx) => {
  const { path, method, store: s, body, session, url } = ctx;
  const builds = path.match(/^\/api\/v1\/applications\/([^/]+)\/builds$/);
  if (builds) {
    const app = find(s.applications, builds[1]);
    s.authorize(app.projectId, method === "GET" ? "read" : "develop", session);
    if (method === "GET")
      return ok(
        pageOf(
          s.builds.filter((b) => b.build.applicationId === app.id),
          url,
        ),
      );
    if (method === "POST")
      return ok(
        createBuild(
          s,
          app.id,
          {
            repositoryUrl: text(body, "repositoryUrl"),
            sourceCommit: text(body, "sourceCommit"),
            dockerfilePath: text(body, "dockerfilePath", "Dockerfile"),
            contextPath: text(body, "contextPath", "."),
          },
          s.user(session).id,
        ),
        202,
      );
  }
  const build = path.match(/^\/api\/v1\/builds\/([^/]+)$/);
  if (build && method === "GET") {
    const b =
      s.builds.find((b) => b.build.id === build[1]) ??
      fail(404, "not_found", "构建不存在");
    s.authorize(b.build.projectId, "read", session);
    return ok(b);
  }
  const image = path.match(/^\/api\/v1\/image-artifacts\/([^/]+)$/);
  if (image && method === "GET") {
    const a =
      s.builds.find((b) => b.imageArtifact?.id === image[1])?.imageArtifact ??
      fail(404, "not_found", "产物不存在");
    s.authorize(a.projectId, "read", session);
    return ok(a);
  }
  const log = path.match(/^\/api\/v1\/build-attempts\/([^/]+)\/log$/);
  if (log && method === "GET") {
    const b =
      s.builds.find((b) =>
        b.buildOperation.attempts.some((a) => a.id === log[1]),
      ) ?? fail(404, "not_found", "执行尝试不存在");
    s.authorize(b.build.projectId, "read_logs", session);
    return ok({
      buildAttemptId: log[1],
      excerpt: `#1 [internal] load ${b.build.dockerfilePath}\n#2 resolve source ${b.build.sourceCommit}\n#3 RUN go build ./cmd/server\n#4 exporting OCI image\n${b.buildOperation.status === "failed" ? "ERROR: simulated build failed" : b.imageArtifact ? `digest: ${b.imageArtifact.digest}\nDONE` : "building..."}`,
      truncated: false,
    });
  }
  const releases = path.match(
    /^\/api\/v1\/deployment-targets\/([^/]+)\/releases$/,
  );
  if (releases) {
    const t = find(s.targets, releases[1]);
    const app = find(s.applications, t.applicationId);
    s.authorize(app.projectId, method === "GET" ? "read" : "develop", session);
    if (method === "GET")
      return ok(
        pageOf(
          s.releases
            .filter((r) => r.release.deploymentTargetId === t.id)
            .map((r) => ({
              release: r.release,
              releaseOperation: r.releaseOperation,
            })),
          url,
        ),
      );
    if (method === "POST")
      return ok(
        createRelease(
          s,
          t.id,
          {
            imageReference: text(body, "imageReference"),
            ...(body.imageArtifactId
              ? { imageArtifactId: text(body, "imageArtifactId") }
              : {}),
          },
          s.user(session).id,
        ),
        202,
      );
  }
  const release = path.match(
    /^\/api\/v1\/releases\/([^/]+)(?:\/(diagnostics|runtime-logs|rollback))?$/,
  );
  if (release) {
    const r =
      s.releases.find((r) => r.release.id === release[1]) ??
      fail(404, "not_found", "发布不存在");
    s.authorize(
      r.release.targetSnapshot.projectId,
      release[2] === "rollback"
        ? "develop"
        : release[2] === "runtime-logs"
          ? "read_logs"
          : "read",
      session,
    );
    if (method === "GET") {
      if (release[2] === "diagnostics") return ok(report(s, r));
      if (release[2] === "runtime-logs") {
        const podName = url.searchParams.get("podName") ?? "";
        const container = url.searchParams.get("container") ?? "";
        if (
          !report(s, r).workloadObservation.pods.some(
            (p) =>
              p.name === podName &&
              p.containers.some((c) => c.name === container),
          )
        )
          fail(404, "pod_not_found", "Pod 或容器不存在");
        return ok({
          source: "kubernetes",
          releaseId: r.release.id,
          podName,
          container,
          observedAt: now(),
          tailLines: 200,
          previous: false,
          truncated: false,
          content: `${now()} INFO server listening :${r.release.targetSnapshot.containerPort}\n${now()} INFO GET /healthz 200\n${now()} INFO GET /api/payment 200 duration=12ms\n${now()} INFO ready to accept requests`,
        });
      }
      return ok({ ...r, snapshotDifferences: differences(s, r.release) });
    }
    if (release[2] === "rollback" && method === "POST")
      return ok(
        createRelease(
          s,
          r.release.deploymentTargetId,
          {
            imageReference: r.release.imageReference,
            ...(r.release.imageArtifactId
              ? { imageArtifactId: r.release.imageArtifactId }
              : {}),
          },
          s.user(session).id,
          copy(r.release.targetSnapshot),
          r.release.id,
        ),
        202,
      );
  }
  const command = path.match(
    /^\/api\/v1\/(build|release)-operations\/([^/]+)(?:\/(retry|cancel|reconcile|force-fail|fail))?$/,
  );
  if (command) {
    const [, kind, key, action] = command;
    const b =
      kind === "build"
        ? s.builds.find((b) => b.buildOperation.id === key)
        : undefined;
    const r =
      kind === "release"
        ? s.releases.find((r) => r.releaseOperation.id === key)
        : undefined;
    const operation =
      b?.buildOperation ??
      r?.releaseOperation ??
      fail(404, "not_found", "执行不存在");
    const projectId = b?.build.projectId ?? r!.release.targetSnapshot.projectId;
    s.authorize(projectId, "read", session);
    if (!action && method === "GET") return ok(operation);
    if (method !== "POST" || !action) return;
    s.authorize(
      projectId,
      action === "force-fail" ||
        action === "fail" ||
        (action === "retry" && operation.status === "attention_required")
        ? "resolve_unknown"
        : "develop",
      session,
    );
    if (
      action === "retry" &&
      ["failed", "attention_required"].includes(operation.status)
    ) {
      operation.status = "pending";
      operation.recoveryRequired = false;
      operation.availableAt = now();
      operation.queuedAt = now();
      delete operation.finishedAt;
      delete operation.errorCode;
      delete operation.errorSummary;
      delete operation.retryDisposition;
      s.jobs.set(operation.id, {
        kind: kind as "build" | "release",
        operationId: operation.id,
        outcome: "success",
        started: Date.now(),
      });
    } else if (
      action === "cancel" &&
      ["pending", "running"].includes(operation.status)
    ) {
      if (operation.status === "pending") {
        operation.status = "canceled";
        operation.finishedAt = now();
        s.jobs.delete(operation.id);
      } else operation.status = "cancel_requested";
    } else if (
      action === "reconcile" &&
      operation.status === "attention_required"
    )
      finish(s, operation, "success");
    else if (
      (action === "force-fail" || action === "fail") &&
      operation.status === "attention_required"
    ) {
      const reason = text(body, "reason");
      finish(s, operation, "failed");
      operation.errorSummary = reason;
    } else fail(409, "operation_state_conflict", "当前执行状态不允许此命令");
    operation.updatedAt = now();
    if (r)
      r.auditTimeline.push({
        id: id(),
        actorId: s.user(session).id,
        actorKind: "user",
        action: `release.${action}`,
        targetType: "release",
        targetId: r.release.id,
        summary: {},
        createdAt: now(),
      });
    return ok(operation, 202);
  }
  const pipelines = path.match(
    /^\/api\/v1\/(?:applications\/([^/]+)\/delivery-pipelines|delivery-pipelines\/([^/]+)(?:\/(enable|disable|runs))?)$/,
  );
  if (pipelines) {
    const [, appId, pipelineId, action] = pipelines;
    const p = pipelineId
      ? (s.pipelines.find((p) => p.pipeline.id === pipelineId) ??
        fail(404, "not_found", "Pipeline 不存在"))
      : undefined;
    const app = find(s.applications, appId ?? p!.pipeline.applicationId);
    s.authorize(app.projectId, method === "GET" ? "read" : "develop", session);
    if (method === "GET")
      return ok(
        action === "runs"
          ? pageOf(
              s.runs.filter((r) => r.run.deliveryPipelineId === pipelineId),
              url,
            )
          : (p ??
              pageOf(
                s.pipelines.filter((p) => p.pipeline.applicationId === app.id),
                url,
              )),
      );
    if (
      p &&
      method === "POST" &&
      (action === "enable" || action === "disable")
    ) {
      p.pipeline.enabled = action === "enable";
      p.pipeline.activationGeneration++;
      p.pipeline.updatedAt = now();
      return ok(p);
    }
    if ((method === "POST" && appId) || (method === "PUT" && p && !action)) {
      const repositoryUrl = text(body, "repositoryUrl");
      const repository = new URL(repositoryUrl);
      if (
        repository.protocol !== "https:" ||
        repository.hostname !== "github.com" ||
        repository.username ||
        repository.password
      )
        fail(400, "invalid_repository", "需要 GitHub HTTPS 仓库地址");
      const mode = choice(body, "mode", [
        "auto_release",
        "build_only",
      ] as const);
      const targetId =
        mode === "auto_release" ? text(body, "deploymentTargetId") : undefined;
      if (targetId) {
        const t = find(s.targets, targetId);
        if (t.applicationId !== app.id || t.stage !== "development")
          fail(
            409,
            "invalid_pipeline_target",
            "自动交付只能使用当前应用的开发 Target",
          );
      }
      const timestamp = now();
      const revision: Schema["DeliveryPipelineRevision"] = {
        revision: (p?.pipeline.currentRevision ?? 0) + 1,
        provider: "github",
        endpointKey: text(body, "endpointKey"),
        repositoryId: 1,
        repositoryOwnerId: 2,
        repositoryFullName: repository.pathname.replace(/^\/|\.git$|\/$/g, ""),
        repositoryUrl,
        gitRef: `refs/heads/${text(body, "branch")}`,
        dockerfilePath: text(body, "dockerfilePath", "Dockerfile"),
        contextPath: text(body, "contextPath", "."),
        platform: "linux/amd64",
        mode,
        ...(targetId ? { deploymentTargetId: targetId } : {}),
        createdBy: s.user(session).id,
        createdAt: timestamp,
      };
      if (p) {
        p.revision = revision;
        p.pipeline.currentRevision = revision.revision;
        p.pipeline.updatedAt = timestamp;
        return ok(p);
      }
      const created: Schema["DeliveryPipelineDetail"] = {
        pipeline: {
          id: id(),
          projectId: app.projectId,
          applicationId: app.id,
          name: text(body, "name"),
          currentRevision: 1,
          enabled: true,
          activationGeneration: 1,
          createdBy: s.user(session).id,
          createdAt: timestamp,
          updatedAt: timestamp,
        },
        revision,
      };
      s.pipelines.unshift(created);
      return ok(created, 201);
    }
  }
  const runs = path.match(
    /^\/api\/v1\/delivery-runs\/([^/]+)(?:\/(reconcile))?$/,
  );
  if (runs) {
    const r =
      s.runs.find((r) => r.run.id === runs[1]) ??
      fail(404, "not_found", "交付运行不存在");
    const p = s.pipelines.find(
      (p) => p.pipeline.id === r.run.deliveryPipelineId,
    )!;
    s.authorize(
      p.pipeline.projectId,
      method === "GET" ? "read" : "develop",
      session,
    );
    if (method === "POST" && runs[2]) advance(s);
    if (method === "GET" || (runs[2] && method === "POST")) return ok(r);
  }
  // 这里只接收合成 Push；真实 Webhook 验签和仓库校验由后端真实链路验收覆盖。
  const webhook = path.match(/^\/api\/v1\/webhooks\/github\/([^/]+)$/);
  if (webhook && method === "POST") {
    const p =
      s.pipelines.find((p) => p.revision.endpointKey === webhook[1]) ??
      fail(404, "not_found", "模拟 endpoint 不存在");
    return ok(
      triggerPush(
        s,
        p.pipeline.id,
        typeof body.after === "string" ? body.after : undefined,
      ),
      202,
    );
  }
};
