import { errorText, type Application, type DeploymentTarget } from "@/api/http";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectItem } from "@/components/ui/select";
import { QueryNotice } from "@/shared/OverviewUI";
import { useQueries, useQuery } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { buildQueries } from "../builds/api";
import { releaseQueries } from "./api";
import { useCreateRelease } from "./mutations";

type ImageArtifact = components["schemas"]["ImageArtifact"];

/** 发布必须显式选择产物/不可变引用并确认目标。失败与关闭后保留草稿及幂等键，
 * 避免网络结果未知时重新打开对话框，重复接纳相同的发布；编辑载荷才形成新命令。
 */
export function ReleaseCreateDialog({
  target,
  application,
  initialSource,
  open,
  onOpenChange,
  onAccepted,
}: {
  target: DeploymentTarget;
  application: Application;
  initialSource?: { kind: "build" | "release"; id: string };
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onAccepted: (id: string) => void;
}) {
  const [source, setSource] = useState(
    initialSource?.kind === "release" ? "reference" : "artifact",
  );
  const [pageCursors, setPageCursors] = useState<Array<string | undefined>>([
    undefined,
  ]);
  // 产物不可变：草稿保存选中快照，加载更多或后台刷新不撤销用户的选择。
  const [selected, setSelected] = useState<ImageArtifact>();
  const [reference, setReference] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [validation, setValidation] = useState("");
  const [prefillDone, setPrefillDone] = useState(false);
  const sourceBuild = useQuery({
    ...buildQueries.detail(initialSource?.id ?? ""),
    enabled: open && initialSource?.kind === "build",
  });
  const sourceRelease = useQuery({
    ...releaseQueries.detail(initialSource?.id ?? ""),
    enabled: open && initialSource?.kind === "release",
  });
  const suggestedArtifact =
    initialSource?.kind === "build" &&
    sourceBuild.data?.build.projectId === application.projectId &&
    sourceBuild.data.build.applicationId === application.id &&
    sourceBuild.data.buildOperation.status === "succeeded" &&
    sourceBuild.data.imageArtifact?.projectId === application.projectId &&
    sourceBuild.data.imageArtifact.applicationId === application.id
      ? sourceBuild.data.imageArtifact
      : undefined;
  const suggestedRelease =
    initialSource?.kind === "release" &&
    sourceRelease.data?.release.targetSnapshot.projectId ===
      application.projectId &&
    sourceRelease.data.release.targetSnapshot.applicationId === application.id
      ? sourceRelease.data.release
      : undefined;
  useEffect(() => {
    if (prefillDone) return;
    if (suggestedArtifact && !selected) {
      setSelected(suggestedArtifact);
      setPrefillDone(true);
    } else if (suggestedRelease && !reference) {
      setReference(suggestedRelease.imageReference);
      setPrefillDone(true);
    }
  }, [prefillDone, suggestedArtifact, suggestedRelease, selected, reference]);
  // 只订阅用户已加载的游标页，与概览共用同一缓存，构建完成后同步看到产物。
  const builds = useQueries({
    queries: pageCursors.map((cursor) => ({
      ...buildQueries.list(application.id, cursor),
      enabled: open && source === "artifact",
    })),
  });
  // 首批更新可能改变后续游标：只展示仍连贯的页，下一次加载从新游标继续。
  // 已选产物另存于草稿，不受页链重建影响，也不自动扫描新的历史页。
  let pageCount = 1;
  while (
    pageCount < pageCursors.length &&
    builds[pageCount - 1].data?.nextCursor === pageCursors[pageCount]
  ) {
    pageCount += 1;
  }
  const pages = builds.slice(0, pageCount);
  const firstPage = pages[0];
  const lastPage = pages[pages.length - 1];
  const failedPage = pages.find((page) => page.error);
  const fetching = pages.some((page) => page.isFetching);
  const loadingMore = pages.length > 1 && lastPage.isPending;
  const nextCursor = lastPage.data?.nextCursor;
  const hasMore =
    nextCursor && !pageCursors.slice(0, pageCount).includes(nextCursor);
  const loadedArtifacts = pages
    .flatMap((page) => page.data?.items ?? [])
    .flatMap((item) =>
      item.build.projectId === application.projectId &&
      item.build.applicationId === application.id &&
      item.buildOperation.status === "succeeded" &&
      item.imageArtifact?.projectId === application.projectId &&
      item.imageArtifact.applicationId === application.id
        ? [item.imageArtifact]
        : [],
    );
  // 游标页可能重叠，来源产物也可能已在列表中；每个产物只展示一次。
  const artifacts = [
    ...new Map(
      [
        ...(suggestedArtifact ? [suggestedArtifact] : []),
        ...(selected ? [selected] : []),
        ...loadedArtifacts,
      ].map((artifact) => [artifact.id, artifact]),
    ).values(),
  ];
  const mutation = useCreateRelease(target.id, (result) => {
    setReference("");
    setSelected(undefined);
    setConfirmed(false);
    setValidation("");
    onOpenChange(false);
    onAccepted(result.release.id);
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
      <DialogContent
        aria-describedby={undefined}
        className="max-h-[90dvh] overflow-y-auto sm:max-w-xl"
      >
        <DialogHeader>
          <DialogTitle>新建发布</DialogTitle>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-5">
          <div className="rounded border bg-muted/50 p-3 text-sm">
            <strong>
              {target.stage === "production" ? "生产目标" : "开发目标"}
            </strong>
            <p className="mt-1 break-all text-xs text-muted-foreground">
              {target.clusterRef} / {target.namespace}
            </p>
            <p className="mt-1 text-xs text-muted-foreground">
              期望 {target.replicas} 个副本，容器端口 {target.containerPort}
            </p>
          </div>
          {initialSource && (
            <div className="rounded border p-3 text-sm">
              <p className="font-medium">
                来源：{initialSource.kind === "build" ? "构建" : "历史发布"}{" "}
                {initialSource.id}
              </p>
              {(sourceBuild.error || sourceRelease.error) && (
                <QueryNotice
                  error={sourceBuild.error ?? sourceRelease.error}
                  retry={() =>
                    void (initialSource.kind === "build"
                      ? sourceBuild.refetch()
                      : sourceRelease.refetch())
                  }
                />
              )}
              {initialSource.kind === "build" &&
                !sourceBuild.isPending &&
                !sourceBuild.error &&
                !suggestedArtifact && (
                  <p role="alert" className="mt-2 text-destructive">
                    此构建没有属于当前应用的成功产物，请重新选择。
                  </p>
                )}
              {initialSource.kind === "release" &&
                !sourceRelease.isPending &&
                !sourceRelease.error &&
                !suggestedRelease && (
                  <p role="alert" className="mt-2 text-destructive">
                    此发布不属于当前应用，请重新选择镜像。
                  </p>
                )}
              {suggestedRelease &&
                target.stage === "production" &&
                suggestedRelease.targetSnapshot.stage === "development" && (
                  <p className="mt-2 text-amber-800">
                    将开发发布的镜像晋级生产。
                  </p>
                )}
            </div>
          )}
          <fieldset disabled={mutation.isPending} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="release-source">镜像来源</Label>
              <Select
                id="release-source"
                className="w-full"
                value={source}
                onValueChange={(value) => {
                  setSource(value);
                  setPrefillDone(true);
                  setConfirmed(false);
                  setValidation("");
                }}
              >
                {<SelectItem value="artifact">成功构建产物</SelectItem>}
                <SelectItem value="reference">
                  不可变镜像引用（高级）
                </SelectItem>
              </Select>
            </div>
            {source === "artifact" ? (
              <>
                {firstPage.isPending && !suggestedArtifact && <QueryNotice />}
                {failedPage && (
                  <QueryNotice
                    error={failedPage.error}
                    retry={() => {
                      if (fetching || mutation.isPending) return;
                      void failedPage.refetch({ cancelRefetch: false });
                    }}
                  />
                )}
                {(firstPage.data || suggestedArtifact || selected) && (
                  <>
                    <Label htmlFor="release-artifact">选择产物</Label>
                    <Select
                      id="release-artifact"
                      className="w-full"
                      value={selected?.id ?? ""}
                      placeholder={
                        artifacts.length ? "选择构建产物" : "暂无产物"
                      }
                      emptyValueAsPlaceholder
                      disabled={mutation.isPending || artifacts.length === 0}
                      onValueChange={(value) => {
                        setSelected(
                          artifacts.find((artifact) => artifact.id === value),
                        );
                        setPrefillDone(true);
                        setConfirmed(false);
                        setValidation("");
                      }}
                    >
                      {artifacts.map((artifact) => (
                        <SelectItem key={artifact.id} value={artifact.id}>
                          {artifact.buildId.slice(0, 8)} /{" "}
                          {artifact.digest.slice(0, 19)}
                        </SelectItem>
                      ))}
                    </Select>
                    {(hasMore || loadingMore) && !failedPage && (
                      <div className="flex justify-end">
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          disabled={fetching || mutation.isPending}
                          onClick={() => {
                            if (!nextCursor || fetching) return;
                            setPageCursors((current) => [
                              ...current.slice(0, pageCount),
                              nextCursor,
                            ]);
                          }}
                        >
                          {loadingMore ? "加载中…" : "加载更多"}
                        </Button>
                      </div>
                    )}
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
                    setPrefillDone(true);
                    setConfirmed(false);
                  }}
                  placeholder="registry.example.com/app@sha256:…"
                  autoCapitalize="none"
                  spellCheck={false}
                  aria-describedby={
                    validation ? "release-validation" : undefined
                  }
                />
              </div>
            )}
            <label
              htmlFor="release-confirmation"
              className="flex items-start gap-3 rounded border p-3 text-sm leading-6"
            >
              <Checkbox
                id="release-confirmation"
                className="mt-1"
                checked={confirmed}
                disabled={mutation.isPending}
                onCheckedChange={(checked) => setConfirmed(checked === true)}
              />
              我确认发布到{target.stage === "production" ? "生产" : "开发"}
              环境，这可能替换该目标当前运行的版本。
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
              <p className="mt-2 text-xs">接纳结果未确认，请保持原输入重试。</p>
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
                (source === "artifact" && !selected)
              }
            >
              {mutation.isPending
                ? "正在接纳…"
                : `确认发布到${target.stage === "production" ? "生产" : "开发"}环境`}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
