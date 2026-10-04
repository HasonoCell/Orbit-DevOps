import { errorText } from "@/api/http";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { zodResolver } from "@hookform/resolvers/zod";
import { useForm } from "react-hook-form";
import { useNavigate } from "react-router-dom";
import { z } from "zod";
import { useCreateBuild } from "./mutations";

const schema = z.object({
  repositoryUrl: z
    .string()
    .trim()
    .refine((value) => {
      try {
        const url = new URL(value);
        return (
          url.protocol === "https:" &&
          !!url.hostname &&
          !url.username &&
          !url.password
        );
      } catch {
        return false;
      }
    }, "仅接受不含凭据的 HTTPS 地址"),
  sourceCommit: z
    .string()
    .trim()
    .regex(
      /^(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$/,
      "请输入完整的 40 或 64 位 Commit SHA",
    ),
  dockerfilePath: z.string().trim().min(1, "请输入 Dockerfile 路径").max(512),
  contextPath: z.string().trim().min(1, "请输入构建上下文路径").max(512),
});
type BuildValues = z.infer<typeof schema>;

export function BuildCreateDialog({
  projectId,
  applicationId,
  open,
  onOpenChange,
}: {
  projectId: string;
  applicationId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const navigate = useNavigate();
  const form = useForm<BuildValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      repositoryUrl: "",
      sourceCommit: "",
      dockerfilePath: "Dockerfile",
      contextPath: ".",
    },
    mode: "onBlur",
  });
  const mutation = useCreateBuild(applicationId, (accepted) => {
    onOpenChange(false);
    navigate(
      `/projects/${projectId}/applications/${applicationId}/builds/${accepted.build.id}`,
    );
  });
  // 网络结果不明时保留草稿和幂等键；用户修改输入才产生新命令。
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!mutation.isPending) onOpenChange(next);
      }}
    >
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>手动构建</DialogTitle>
          <DialogDescription>
            指定不可变 Commit。创建后由 Worker 执行构建。
          </DialogDescription>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
        >
          <div className="space-y-2">
            <Label htmlFor="build-repository">Git Clone URL</Label>
            <Input
              id="build-repository"
              type="url"
              placeholder="https://github.com/HasonoCell/Yuuki.git"
              autoCapitalize="none"
              spellCheck={false}
              {...form.register("repositoryUrl")}
              aria-invalid={!!form.formState.errors.repositoryUrl}
            />
            {form.formState.errors.repositoryUrl && (
              <p role="alert" className="text-sm text-destructive">
                {form.formState.errors.repositoryUrl.message}
              </p>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="build-commit">Commit SHA</Label>
            <Input
              id="build-commit"
              placeholder="完整的 40 或 64 位 SHA"
              autoCapitalize="none"
              spellCheck={false}
              {...form.register("sourceCommit")}
              aria-invalid={!!form.formState.errors.sourceCommit}
            />
            {form.formState.errors.sourceCommit && (
              <p role="alert" className="text-sm text-destructive">
                {form.formState.errors.sourceCommit.message}
              </p>
            )}
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="build-dockerfile">Dockerfile 路径</Label>
              <Input
                id="build-dockerfile"
                {...form.register("dockerfilePath")}
                aria-invalid={!!form.formState.errors.dockerfilePath}
              />
              {form.formState.errors.dockerfilePath && (
                <p role="alert" className="text-sm text-destructive">
                  {form.formState.errors.dockerfilePath.message}
                </p>
              )}
            </div>
            <div className="space-y-2">
              <Label htmlFor="build-context">构建上下文</Label>
              <Input
                id="build-context"
                {...form.register("contextPath")}
                aria-invalid={!!form.formState.errors.contextPath}
              />
              {form.formState.errors.contextPath && (
                <p role="alert" className="text-sm text-destructive">
                  {form.formState.errors.contextPath.message}
                </p>
              )}
            </div>
          </div>
          {mutation.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>
                {errorText(mutation.error)}。保留相同输入重试会复用幂等键。
              </AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => onOpenChange(false)}
            >
              取消
            </Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? "正在创建…" : "创建构建"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
