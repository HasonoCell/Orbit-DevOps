import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  getApplication,
  getProjectPermissions,
  listDeploymentTargets,
} from "@/api/catalog";
import { ApiError, errorText, type DeploymentTarget } from "@/api/http";
import type { components } from "@/api/schema";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { overviewQueryKeys } from "@/features/applications/api";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { Fact, Timestamp } from "@/shared/OverviewUI";
import { useCommandKey } from "@/shared/use-command-key";
import {
  changePipelineState,
  createPipeline,
  getPipeline,
  pipelineKeys,
  updatePipeline,
  type PipelineInput,
} from "./pipeline-api";

type Detail = components["schemas"]["DeliveryPipelineDetail"];
type Mode = components["schemas"]["DeliveryMode"];

export function PipelinePage() {
  const { projectId = "", applicationId = "", pipelineId = "" } = useParams();
  const creating = pipelineId === "new";
  const application = useQuery({
    queryKey: ["application", applicationId],
    queryFn: () => getApplication(applicationId),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", projectId],
    queryFn: () => getProjectPermissions(projectId),
  });
  const targets = useQuery({
    queryKey: overviewQueryKeys.targets(applicationId),
    queryFn: () => listDeploymentTargets(applicationId),
  });
  const detail = useQuery({
    queryKey: pipelineKeys.detail(pipelineId),
    queryFn: () => getPipeline(pipelineId),
    enabled: !creating,
  });
  if (
    application.isPending ||
    permissions.isPending ||
    targets.isPending ||
    (!creating && detail.isPending)
  )
    return <LoadingPage label="正在加载自动交付配置" />;
  if (application.error)
    return (
      <ErrorPanel
        title="无法加载应用"
        error={application.error}
        onRetry={() => void application.refetch()}
      />
    );
  if (permissions.error)
    return (
      <ErrorPanel
        title="无法确认项目权限"
        error={permissions.error}
        onRetry={() => void permissions.refetch()}
      />
    );
  if (targets.error)
    return (
      <ErrorPanel
        title="无法加载部署目标"
        error={targets.error}
        onRetry={() => void targets.refetch()}
      />
    );
  if (!creating && detail.error)
    return (
      <ErrorPanel
        title="无法加载 Pipeline"
        error={detail.error}
        onRetry={() => void detail.refetch()}
      />
    );
  if (
    application.data.projectId !== projectId ||
    (!creating &&
      (detail.data?.pipeline.projectId !== projectId ||
        detail.data.pipeline.applicationId !== applicationId))
  )
    return (
      <EmptyState
        title="Pipeline 不属于当前应用"
        description="请从它所属的应用重新进入。"
      />
    );
  const canDevelop = permissions.data.allowed.includes("develop");
  if (creating && !canDevelop)
    return (
      <EmptyState
        title="当前角色不能创建 Pipeline"
        description="请联系项目管理员确认开发权限。"
      />
    );
  return (
    <PipelineEditor
      key={`${pipelineId}-${detail.data?.revision.revision ?? "new"}`}
      projectId={projectId}
      applicationId={applicationId}
      pipelineId={pipelineId}
      detail={detail.data}
      targets={targets.data}
      canDevelop={canDevelop}
      onRefresh={() => void detail.refetch()}
    />
  );
}

function PipelineEditor({
  projectId,
  applicationId,
  pipelineId,
  detail,
  targets,
  canDevelop,
  onRefresh,
}: {
  projectId: string;
  applicationId: string;
  pipelineId: string;
  detail?: Detail;
  targets: DeploymentTarget[];
  canDevelop: boolean;
  onRefresh: () => void;
}) {
  const creating = pipelineId === "new";
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const commandKey = useCommandKey();
  const [name, setName] = useState(detail?.pipeline.name ?? "");
  const [endpointKey, setEndpointKey] = useState(
    detail?.revision.endpointKey ?? "",
  );
  const [repositoryUrl, setRepositoryUrl] = useState(
    detail?.revision.repositoryUrl ?? "",
  );
  const [branch, setBranch] = useState(
    detail?.revision.gitRef.replace(/^refs\/heads\//, "") ?? "main",
  );
  const [dockerfilePath, setDockerfilePath] = useState(
    detail?.revision.dockerfilePath ?? "Dockerfile",
  );
  const [contextPath, setContextPath] = useState(
    detail?.revision.contextPath ?? ".",
  );
  const [mode, setMode] = useState<Mode>(detail?.revision.mode ?? "build_only");
  const [targetId, setTargetId] = useState(
    detail?.revision.deploymentTargetId ?? "",
  );
  const [validation, setValidation] = useState("");
  const [stateAction, setStateAction] = useState<"enable" | "disable" | null>(
    null,
  );
  const developmentTargets = targets.filter(
    (item) => item.stage === "development",
  );
  const mutation = useMutation({
    mutationFn: (body: PipelineInput) => {
      const key = commandKey.forPayload({
        pipelineId,
        body,
        expectedRevision: detail?.revision.revision,
      });
      return creating
        ? createPipeline(applicationId, body, key)
        : updatePipeline(
            pipelineId,
            {
              expectedRevision: detail!.revision.revision,
              endpointKey: body.endpointKey,
              repositoryUrl: body.repositoryUrl,
              branch: body.branch,
              dockerfilePath: body.dockerfilePath,
              contextPath: body.contextPath,
              mode: body.mode,
              deploymentTargetId: body.deploymentTargetId,
            },
            key,
          );
    },
    onSuccess(updated) {
      commandKey.clear();
      queryClient.setQueryData(
        pipelineKeys.detail(updated.pipeline.id),
        updated,
      );
      void queryClient.invalidateQueries({
        queryKey: overviewQueryKeys.pipelines(applicationId, undefined),
      });
      if (creating)
        navigate(
          `/projects/${projectId}/applications/${applicationId}/pipelines/${updated.pipeline.id}`,
        );
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    const cleanName = name.trim();
    const cleanEndpoint = endpointKey.trim();
    const cleanRepository = repositoryUrl.trim();
    const cleanBranch = branch.trim();
    let parsed: URL;
    try {
      parsed = new URL(cleanRepository);
    } catch {
      setValidation("请输入 GitHub 仓库的 HTTPS Clone URL。");
      return;
    }
    if (creating && !cleanName) {
      setValidation("请输入 Pipeline 名称。");
      return;
    }
    if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/.test(cleanEndpoint)) {
      setValidation(
        "请输入运维预置的 endpoint key（字母、数字、下划线或连字符）。",
      );
      return;
    }
    if (
      parsed.protocol !== "https:" ||
      parsed.hostname !== "github.com" ||
      parsed.username ||
      parsed.password ||
      !cleanBranch
    ) {
      setValidation("请输入无凭据的 GitHub HTTPS 仓库地址和分支名。");
      return;
    }
    if (
      mode === "auto_release" &&
      !developmentTargets.some((item) => item.id === targetId)
    ) {
      setValidation("自动部署只能选择当前应用的开发 Target。");
      return;
    }
    setValidation("");
    mutation.mutate({
      name: cleanName,
      endpointKey: cleanEndpoint,
      repositoryUrl: cleanRepository,
      branch: cleanBranch,
      dockerfilePath: dockerfilePath.trim(),
      contextPath: contextPath.trim(),
      mode,
      ...(mode === "auto_release" ? { deploymentTargetId: targetId } : {}),
    });
  }
  const webhookUrl = detail
    ? new URL(
        `/api/v1/webhooks/github/${encodeURIComponent(detail.revision.endpointKey)}`,
        import.meta.env.VITE_ORBIT_DEVOPS_API_URL || window.location.origin,
      ).toString()
    : "";
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div>
          <Link
            className="text-xs text-muted-foreground hover:text-primary"
            to={`/projects/${projectId}/applications/${applicationId}?view=delivery`}
          >
            应用 / 自动交付
          </Link>
          <h1 className="mt-2">
            {creating ? "配置 Pipeline" : detail!.pipeline.name}
          </h1>
        </div>
        {!creating && (
          <Button variant="outline" onClick={onRefresh}>
            刷新服务端配置
          </Button>
        )}
      </div>
      {detail && (
        <section className="workbench-panel mb-5">
          <header className="panel-heading">
            <h2>接入状态</h2>
            <span className="text-xs">
              Orbit {detail.pipeline.enabled ? "已启用" : "已停用"}
            </span>
          </header>
          <dl className="grid gap-4 p-5 text-sm sm:grid-cols-2">
            <Fact label="当前 Revision">{detail.revision.revision}</Fact>
            <Fact label="最近更新">
              <Timestamp value={detail.pipeline.updatedAt} />
            </Fact>
            <Fact label="GitHub 仓库">
              {detail.revision.repositoryFullName}
            </Fact>
            <Fact label="Webhook URL">
              <span className="break-all font-mono">{webhookUrl}</span>
            </Fact>
          </dl>
          <p className="border-t px-5 py-4 text-xs text-muted-foreground">
            运维需先配置 endpoint key 对应的服务端验签密钥，再在 GitHub
            仓库手动配置 Webhook URL 与相同密钥。Orbit
            已启用仅表示本侧接纳匹配事件，不表示 GitHub 已连接。
          </p>
        </section>
      )}
      <section className="workbench-panel max-w-3xl">
        <header className="panel-heading">
          <h2>{creating ? "新建配置" : "修订配置"}</h2>
        </header>
        <form onSubmit={submit} className="space-y-5 p-5">
          <fieldset
            disabled={!canDevelop || mutation.isPending}
            className="space-y-5"
          >
            {creating && (
              <Field
                label="名称"
                id="pipeline-name"
                value={name}
                setValue={setName}
              />
            )}
            <Field
              label="Endpoint Key"
              id="pipeline-endpoint"
              value={endpointKey}
              setValue={setEndpointKey}
            />
            <Field
              label="GitHub Clone URL"
              id="pipeline-repository"
              value={repositoryUrl}
              setValue={setRepositoryUrl}
            />
            <Field
              label="分支"
              id="pipeline-branch"
              value={branch}
              setValue={setBranch}
            />
            <div className="grid gap-4 sm:grid-cols-2">
              <Field
                label="Dockerfile 路径"
                id="pipeline-dockerfile"
                value={dockerfilePath}
                setValue={setDockerfilePath}
              />
              <Field
                label="构建上下文"
                id="pipeline-context"
                value={contextPath}
                setValue={setContextPath}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="pipeline-mode">交付模式</Label>
              <select
                id="pipeline-mode"
                className="runtime-select w-full"
                value={mode}
                onChange={(event) => setMode(event.target.value as Mode)}
              >
                <option value="build_only">仅构建</option>
                <option value="auto_release">构建后自动部署到开发环境</option>
              </select>
            </div>
            {mode === "auto_release" && (
              <div className="space-y-2">
                <Label htmlFor="pipeline-target">开发 Target</Label>
                <select
                  id="pipeline-target"
                  className="runtime-select w-full"
                  value={targetId}
                  onChange={(event) => setTargetId(event.target.value)}
                >
                  <option value="">请选择开发 Target</option>
                  {developmentTargets.map((item) => (
                    <option key={item.id} value={item.id}>
                      {item.clusterRef} / {item.namespace}
                    </option>
                  ))}
                </select>
                {!developmentTargets.length && (
                  <p className="text-xs text-amber-800">
                    请先创建开发 Target。
                  </p>
                )}
              </div>
            )}
          </fieldset>
          {validation && (
            <p role="alert" className="text-sm text-destructive">
              {validation}
            </p>
          )}
          {mutation.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>
                {mutation.error instanceof ApiError &&
                mutation.error.status === 409
                  ? "配置已变化或存在冲突。请刷新服务端配置后再决定是否修订。"
                  : errorText(mutation.error)}
                <span className="block text-xs">
                  结果未确认时，相同输入再次提交会复用幂等键。
                </span>
              </AlertDescription>
            </Alert>
          )}
          {canDevelop && (
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending
                ? "正在保存…"
                : creating
                  ? "创建 Pipeline"
                  : "保存新 Revision"}
            </Button>
          )}
        </form>
      </section>
      {!creating && detail && (
        <PipelineActivation
          detail={detail}
          pipelineId={pipelineId}
          applicationId={applicationId}
          canDevelop={canDevelop}
          selected={stateAction}
          onSelected={setStateAction}
        />
      )}
    </div>
  );
}

function Field({
  label,
  id,
  value,
  setValue,
}: {
  label: string;
  id: string;
  value: string;
  setValue: (value: string) => void;
}) {
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        value={value}
        onChange={(event) => setValue(event.target.value)}
      />
    </div>
  );
}

function PipelineActivation({
  detail,
  pipelineId,
  applicationId,
  canDevelop,
  selected,
  onSelected,
}: {
  detail: Detail;
  pipelineId: string;
  applicationId: string;
  canDevelop: boolean;
  selected: "enable" | "disable" | null;
  onSelected: (value: "enable" | "disable" | null) => void;
}) {
  const queryClient = useQueryClient();
  const commandKey = useCommandKey();
  const mutation = useMutation({
    mutationFn: (action: "enable" | "disable") =>
      changePipelineState(
        pipelineId,
        action,
        commandKey.forPayload({
          pipelineId,
          action,
          revision: detail.revision.revision,
        }),
      ),
    onSuccess(updated) {
      commandKey.clear();
      queryClient.setQueryData(pipelineKeys.detail(pipelineId), updated);
      void queryClient.invalidateQueries({
        queryKey: overviewQueryKeys.pipelines(applicationId, undefined),
      });
      onSelected(null);
    },
  });
  if (!canDevelop) return null;
  const action = detail.pipeline.enabled ? "disable" : "enable";
  return (
    <section className="workbench-panel mt-5 max-w-3xl p-5 text-sm">
      <h2 className="font-semibold">接纳控制</h2>
      <p className="mt-2 text-muted-foreground">
        {detail.pipeline.enabled
          ? "停用后 Orbit 不再接纳此 Pipeline 的新运行，已有运行保留历史。"
          : "启用时将重新核验 GitHub 来源；仍需单独在 GitHub 仓库配置 Webhook。"}
      </p>
      {selected === action ? (
        <div className="mt-4 space-y-3 rounded border p-4">
          <p>
            确认{action === "enable" ? "启用" : "停用"} {detail.pipeline.name}？
          </p>
          {mutation.error && (
            <p role="alert" className="text-destructive">
              {errorText(mutation.error)}
            </p>
          )}
          <div className="flex gap-2">
            <Button
              disabled={mutation.isPending}
              onClick={() => mutation.mutate(action)}
            >
              {mutation.isPending ? "正在提交…" : "确认"}
            </Button>
            <Button
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => onSelected(null)}
            >
              返回
            </Button>
          </div>
        </div>
      ) : (
        <Button
          className="mt-4"
          variant="outline"
          onClick={() => {
            mutation.reset();
            onSelected(action);
          }}
        >
          {action === "enable" ? "启用 Pipeline" : "停用 Pipeline"}
        </Button>
      )}
    </section>
  );
}
