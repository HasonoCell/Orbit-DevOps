import {
  getApplication,
  getDeploymentTarget,
  getProjectPermissions,
  updateDeploymentTarget,
} from "@/api/catalog";
import { errorText } from "@/api/http";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { EmptyState, ErrorPanel, LoadingPage } from "@/shared/PageState";
import { useCommandKey } from "@/shared/use-command-key";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { TargetAccessRoutes } from "./TargetAccessRoutes";
import { TargetFields, useTargetForm, type TargetValues } from "./TargetForm";

export function TargetPage() {
  const { projectId = "", applicationId = "", targetId = "" } = useParams();
  const application = useQuery({
    queryKey: ["application", applicationId],
    queryFn: () => getApplication(applicationId),
  });
  const target = useQuery({
    queryKey: ["deployment-target", targetId],
    queryFn: () => getDeploymentTarget(targetId),
  });
  const permissions = useQuery({
    queryKey: ["project-permissions", projectId],
    queryFn: () => getProjectPermissions(projectId),
  });
  if (application.isPending || target.isPending || permissions.isPending)
    return <LoadingPage label="正在加载部署目标" />;
  if (application.error)
    return (
      <ErrorPanel
        title="无法加载应用"
        error={application.error}
        onRetry={() => void application.refetch()}
      />
    );
  if (target.error)
    return (
      <ErrorPanel
        title="无法加载部署目标"
        error={target.error}
        onRetry={() => void target.refetch()}
      />
    );
  if (permissions.error)
    return (
      <ErrorPanel
        title="无法确认配置权限"
        error={permissions.error}
        onRetry={() => void permissions.refetch()}
      />
    );
  if (
    application.data.projectId !== projectId ||
    target.data.applicationId !== applicationId
  ) {
    return (
      <EmptyState
        title="部署目标不属于当前应用"
        description="请从目标所属的应用重新进入。"
      />
    );
  }
  return (
    <TargetContent
      key={targetId}
      projectId={projectId}
      applicationId={applicationId}
      targetId={targetId}
      target={target.data}
      canDevelop={permissions.data.allowed.includes("develop")}
    />
  );
}

function TargetContent({
  projectId,
  applicationId,
  targetId,
  target,
  canDevelop,
}: {
  projectId: string;
  applicationId: string;
  targetId: string;
  target: Awaited<ReturnType<typeof getDeploymentTarget>>;
  canDevelop: boolean;
}) {
  const form = useTargetForm({
    replicas: target.replicas,
    containerPort: target.containerPort,
  });
  const [saved, setSaved] = useState(false);
  const commandKey = useCommandKey();
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: (values: TargetValues) =>
      updateDeploymentTarget(targetId, values, commandKey.forPayload(values)),
    onSuccess(updated) {
      commandKey.clear();
      form.reset({
        replicas: updated.replicas,
        containerPort: updated.containerPort,
      });
      queryClient.setQueryData(["deployment-target", targetId], updated);
      void queryClient.invalidateQueries({
        queryKey: ["deployment-targets", applicationId],
      });
      setSaved(true);
    },
  });
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div>
          <Link
            className="text-xs text-muted-foreground hover:text-primary"
            to={`/projects/${projectId}/applications/${applicationId}`}
          >
            应用 / 部署目标
          </Link>
          <h1 className="mt-2">
            {target.stage === "production" ? "生产目标" : "开发目标"}
          </h1>
        </div>
      </div>
      <section className="workbench-panel max-w-3xl">
        <header className="panel-heading">
          <h2>目标配置</h2>
        </header>
        <div className="space-y-6 p-5">
          <dl className="grid gap-4 text-sm sm:grid-cols-2">
            <div>
              <dt className="text-muted-foreground">集群</dt>
              <dd className="mt-1 break-all font-medium">
                {target.clusterRef}
              </dd>
            </div>
            <div>
              <dt className="text-muted-foreground">Namespace</dt>
              <dd className="mt-1 break-all font-medium">{target.namespace}</dd>
            </div>
          </dl>
          <p className="text-sm text-muted-foreground">
            修改副本或容器端口只会保存期望配置，将在下一次发布中应用。
          </p>
          {canDevelop ? (
            <form
              className="space-y-5"
              onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
            >
              <TargetFields form={form} disabled={mutation.isPending} />
              {mutation.error && (
                <Alert variant="destructive" role="alert">
                  <AlertDescription>
                    {errorText(mutation.error)}
                  </AlertDescription>
                </Alert>
              )}
              {saved && (
                <p role="status" className="text-sm text-emerald-700">
                  配置已保存，将在下一次发布中应用
                </p>
              )}
              <Button
                type="submit"
                disabled={mutation.isPending || !form.formState.isDirty}
                onClick={() => setSaved(false)}
              >
                {mutation.isPending ? "正在保存…" : "保存配置"}
              </Button>
            </form>
          ) : (
            <p className="text-sm">当前角色仅可查看部署目标。</p>
          )}
        </div>
      </section>
      {canDevelop && (
        <div className="mt-5 max-w-3xl rounded-lg border bg-card p-5 text-sm">
          <h2 className="font-semibold">应用新配置</h2>
          <p className="mt-2 text-muted-foreground">
            保存配置不会修改集群。选择镜像并创建新的发布后，Worker
            才会按新快照执行。
          </p>
          <Button className="mt-4" asChild>
            <Link
              to={`/projects/${projectId}/applications/${applicationId}?view=delivery&target=${targetId}&createRelease=1`}
            >
              选择镜像并发布
            </Link>
          </Button>
        </div>
      )}
      <TargetAccessRoutes projectId={projectId} targetId={targetId} />
      <Collapsible className="mt-6 text-xs text-muted-foreground">
        <CollapsibleTrigger>目标标识</CollapsibleTrigger>
        <CollapsibleContent>
          <p className="mt-2 break-all">{target.id}</p>
        </CollapsibleContent>
      </Collapsible>
    </div>
  );
}
