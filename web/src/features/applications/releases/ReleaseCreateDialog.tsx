import { useMutation, useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { errorText, type Application, type DeploymentTarget } from "@/api/http";
import { listBuilds } from "@/features/applications/api";
import {
  acceptRelease,
  releaseQueryKeys,
} from "@/features/applications/releases/api";
import { CursorPagination } from "@/shared/CursorPagination";
import { QueryNotice } from "@/shared/OverviewUI";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useCommandKey } from "@/features/applications/releases/use-command-key";

/** 发布必须显式选择产物/不可变引用并确认目标。失败与关闭后保留草稿及幂等键，
 * 避免网络结果未知时重新打开对话框，重复接纳相同的发布；编辑载荷才形成新命令。
 */
export function ReleaseCreateDialog({
  target,
  application,
  open,
  onOpenChange,
  onAccepted,
}: {
  target: DeploymentTarget;
  application: Application;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onAccepted: (id: string) => void;
}) {
  const commandKey = useCommandKey();
  const [source, setSource] = useState("artifact");
  const [cursor, setCursor] = useState<string>();
  const [artifactId, setArtifactId] = useState("");
  const [reference, setReference] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [validation, setValidation] = useState("");
  const builds = useQuery({
    queryKey: releaseQueryKeys.artifacts(application.id, cursor),
    queryFn: () => listBuilds(application.id, cursor),
    enabled: open,
  });
  const artifacts = builds.error
    ? []
    : (builds.data?.items ?? []).flatMap((item) =>
        item.buildOperation.status === "succeeded" && item.imageArtifact
          ? [item.imageArtifact]
          : [],
      );
  const selected = artifacts.find((artifact) => artifact.id === artifactId);
  const mutation = useMutation({
    mutationFn: (body: { imageReference: string; imageArtifactId?: string }) =>
      acceptRelease(
        target.id,
        body,
        commandKey.forPayload({ targetId: target.id, ...body }),
      ),
    onSuccess(result) {
      commandKey.clear();
      setReference("");
      setArtifactId("");
      setConfirmed(false);
      setValidation("");
      onOpenChange(false);
      onAccepted(result.release.id);
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    if (mutation.isPending) return;
    const imageReference =
      source === "artifact" ? selected?.imageReference : reference.trim();
    if (
      !imageReference ||
      !/^[^\s@]+@sha256:[a-f0-9]{64}$/.test(imageReference)
    ) {
      setValidation(
        "请选择成功构建的产物，或输入包含 sha256 Digest 的不可变镜像引用。",
      );
      return;
    }
    if (!confirmed) {
      setValidation("请确认目标与发布影响。");
      return;
    }
    setValidation("");
    mutation.mutate({
      imageReference,
      ...(source === "artifact" && selected
        ? { imageArtifactId: selected.id }
        : {}),
    });
  }
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!mutation.isPending) {
          setConfirmed(false);
          onOpenChange(next);
        }
      }}
    >
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>新建发布</DialogTitle>
          <DialogDescription>
            为 {application.name} 创建一条新的发布。接纳后由 Worker
            执行，不会立即变更为“部署成功”。
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-5">
          <div className="rounded border bg-muted/50 p-3 text-sm">
            <strong>{target.stage}</strong>
            <p className="mt-1 break-all text-xs text-muted-foreground">
              {target.clusterRef} / {target.namespace}
            </p>
            <p className="mt-1 text-xs text-muted-foreground">
              期望 {target.replicas} 个副本，容器端口 {target.containerPort}
            </p>
          </div>
          <fieldset disabled={mutation.isPending} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="release-source">镜像来源</Label>
              <select
                id="release-source"
                className="runtime-select w-full"
                value={source}
                onChange={(event) => {
                  setSource(event.target.value);
                  setConfirmed(false);
                  setValidation("");
                }}
              >
                {<option value="artifact">成功构建产物</option>}
                <option value="reference">不可变镜像引用（高级）</option>
              </select>
            </div>
            {source === "artifact" ? (
              <>
                {builds.isPending || builds.error ? (
                  <QueryNotice
                    error={builds.error}
                    retry={() => void builds.refetch()}
                  />
                ) : (
                  <>
                    <Label htmlFor="release-artifact">选择产物</Label>
                    <select
                      id="release-artifact"
                      className="runtime-select w-full"
                      value={artifactId}
                      onChange={(event) => {
                        setArtifactId(event.target.value);
                        setConfirmed(false);
                      }}
                    >
                      <option value="">请选择本页成功构建的产物</option>
                      {artifacts.map((artifact) => (
                        <option key={artifact.id} value={artifact.id}>
                          {artifact.buildId.slice(0, 8)} /{" "}
                          {artifact.digest.slice(0, 19)}
                        </option>
                      ))}
                    </select>
                    {artifacts.length === 0 && (
                      <p className="text-xs text-muted-foreground">
                        本页构建没有可选产物，可继续翻页或使用高级引用。
                      </p>
                    )}
                    <CursorPagination
                      cursor={cursor}
                      nextCursor={builds.data.nextCursor}
                      onChange={(next) => {
                        setCursor(next);
                        setArtifactId("");
                        setConfirmed(false);
                      }}
                    />
                  </>
                )}
                {selected && (
                  <p className="runtime-code">{selected.imageReference}</p>
                )}
              </>
            ) : (
              <div className="space-y-2">
                <Label htmlFor="release-reference">镜像引用</Label>
                <Input
                  id="release-reference"
                  value={reference}
                  onChange={(event) => {
                    setReference(event.target.value);
                    setConfirmed(false);
                  }}
                  placeholder="registry.example.com/app@sha256:…"
                  autoCapitalize="none"
                  spellCheck={false}
                  aria-describedby="release-validation"
                />
                <p className="text-xs text-muted-foreground">
                  不接受可变 tag。服务端会再次校验引用、应用归属和权限。
                </p>
              </div>
            )}
            <label className="flex items-start gap-3 rounded border p-3 text-sm leading-6">
              <input
                type="checkbox"
                className="mt-1 size-4 shrink-0 accent-primary"
                checked={confirmed}
                onChange={(event) => setConfirmed(event.target.checked)}
              />
              我确认发布到 {target.stage}，这可能替换该目标当前运行的版本。
            </label>
          </fieldset>
          {validation && (
            <p
              id="release-validation"
              role="alert"
              className="text-sm text-destructive"
            >
              {validation}
            </p>
          )}
          {mutation.error && (
            <div
              role="alert"
              className="rounded border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
            >
              <p>{errorText(mutation.error)}</p>
              <p className="mt-2 text-xs">
                未确认接纳结果。保持相同输入重试会复用幂等键；关闭窗口不会清除此草稿。
              </p>
            </div>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => {
                setConfirmed(false);
                onOpenChange(false);
              }}
            >
              取消
            </Button>
            <Button
              type="submit"
              disabled={
                mutation.isPending ||
                !confirmed ||
                (source === "artifact" && (!selected || builds.isFetching))
              }
            >
              {mutation.isPending ? "正在接纳…" : `确认发布到 ${target.stage}`}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
