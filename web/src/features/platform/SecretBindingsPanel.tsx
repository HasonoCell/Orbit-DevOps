import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ApiError, errorText } from "@/api/http";
import type { components } from "@/api/schema";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RecentAuthPrompt } from "@/features/auth/RecentAuthPrompt";
import { QueryNotice, Timestamp } from "@/shared/OverviewUI";
import { useCommandKey } from "@/shared/use-command-key";
import {
  listSecretBindings,
  registerSecretBinding,
  revokeSecretBinding,
} from "./api";

type Binding = components["schemas"]["AccessSecretBinding"];

function bindingError(error: unknown) {
  if (error instanceof ApiError) {
    if (error.code === "invalid_tls_secret_binding")
      return "登记失败。请核对 Project ID、域名，以及该 Namespace 中已有的 TLS Secret 是否覆盖该域名。";
    if (error.code === "tls_secret_binding_conflict")
      return "该授权已存在或登记状态已变化，请刷新列表后核对。";
  }
  return errorText(error);
}

export function SecretBindingsPanel() {
  const [offset, setOffset] = useState(0);
  const [projectId, setProjectId] = useState("");
  const [hostname, setHostname] = useState("");
  const [secretName, setSecretName] = useState("");
  const [removing, setRemoving] = useState<Binding | null>(null);
  const [notice, setNotice] = useState("");
  const registerKey = useCommandKey();
  const revokeKey = useCommandKey();
  const queryClient = useQueryClient();
  const list = useQuery({
    queryKey: ["platform-secret-bindings", offset],
    queryFn: () => listSecretBindings(offset),
    retry: false,
  });
  const register = useMutation({
    mutationFn: () => {
      const body = {
        projectId: projectId.trim(),
        hostname: hostname.trim(),
        secretName: secretName.trim(),
      };
      return registerSecretBinding(body, registerKey.forPayload(body));
    },
    onSuccess(binding) {
      registerKey.clear();
      setNotice(`已登记 ${binding.hostname} / ${binding.secretName}。`);
      void queryClient.invalidateQueries({
        queryKey: ["platform-secret-bindings"],
      });
      void queryClient.invalidateQueries({
        queryKey: ["access-secret-binding-options", binding.projectId],
      });
    },
  });
  const revoke = useMutation({
    mutationFn: () =>
      revokeSecretBinding(
        removing!.id,
        revokeKey.forPayload({ bindingId: removing!.id }),
      ),
    onSuccess(binding) {
      revokeKey.clear();
      setRemoving(null);
      setNotice(
        `已撤销 ${binding.hostname} / ${binding.secretName}。正在引用它的入口会重新调和。`,
      );
      void queryClient.invalidateQueries({
        queryKey: ["platform-secret-bindings"],
      });
      void queryClient.invalidateQueries({
        queryKey: ["access-secret-binding-options", binding.projectId],
      });
      void queryClient.invalidateQueries({ queryKey: ["access-host"] });
      void queryClient.invalidateQueries({ queryKey: ["access-status"] });
    },
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    setNotice("");
    if (projectId.trim() && hostname.trim() && secretName.trim())
      register.mutate();
  }
  return (
    <div className="space-y-5">
      <section className="workbench-panel max-w-3xl">
        <header className="panel-heading">
          <div>
            <h2>登记 TLS Secret</h2>
            <p className="mt-1 text-xs text-muted-foreground">
              只登记已有 Secret 的名称和授权范围，不上传证书或私钥
            </p>
          </div>
        </header>
        <form
          onSubmit={submit}
          className="grid gap-4 p-5 text-sm sm:grid-cols-2"
        >
          <div className="space-y-2">
            <Label htmlFor="binding-project">Project ID</Label>
            <Input
              id="binding-project"
              value={projectId}
              onChange={(event) => setProjectId(event.target.value)}
              placeholder="项目 UUID"
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="binding-hostname">域名</Label>
            <Input
              id="binding-hostname"
              value={hostname}
              onChange={(event) => setHostname(event.target.value)}
              placeholder="payment.example.com"
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="binding-secret">Secret 名称</Label>
            <Input
              id="binding-secret"
              value={secretName}
              onChange={(event) => setSecretName(event.target.value)}
              placeholder="payment-tls"
              required
            />
          </div>
          <div className="flex items-end">
            <Button disabled={register.isPending} type="submit">
              登记授权
            </Button>
          </div>
          {register.error &&
            !(
              register.error instanceof ApiError &&
              register.error.code === "recent_authentication_required"
            ) && (
              <p role="alert" className="sm:col-span-2 text-destructive">
                {bindingError(register.error)}
              </p>
            )}
        </form>
        <div className="px-5 pb-5">
          <RecentAuthPrompt error={register.error} />
        </div>
      </section>
      {notice && (
        <p role="status" className="rounded border bg-accent p-4 text-sm">
          {notice}
        </p>
      )}
      <section className="workbench-panel">
        <header className="panel-heading">
          <h2>已有授权</h2>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void list.refetch()}
          >
            刷新
          </Button>
        </header>
        {list.isPending || list.error ? (
          <div className="p-5">
            <QueryNotice error={list.error} retry={() => void list.refetch()} />
            <RecentAuthPrompt error={list.error} />
          </div>
        ) : (
          <>
            {list.data.length ? (
              <div className="divide-y">
                {list.data.slice(0, 20).map((binding) => (
                  <article
                    key={binding.id}
                    className="flex flex-wrap items-center justify-between gap-3 p-5 text-sm"
                  >
                    <div>
                      <p className="font-medium">
                        {binding.hostname}{" "}
                        <span className="ml-2 rounded bg-muted px-2 py-0.5 text-xs">
                          {binding.state === "active" ? "有效" : "已撤销"}
                        </span>
                      </p>
                      <p className="mt-1 text-muted-foreground">
                        {binding.secretName} · {binding.clusterRef}/
                        {binding.namespace}
                      </p>
                      <p className="mt-1 break-all font-mono text-xs text-muted-foreground">
                        Project {binding.projectId}
                      </p>
                    </div>
                    <div className="flex items-center gap-3">
                      <Timestamp value={binding.updatedAt} />
                      {binding.state === "active" && (
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => {
                            setRemoving(binding);
                            revoke.reset();
                            register.reset();
                          }}
                        >
                          撤销
                        </Button>
                      )}
                    </div>
                  </article>
                ))}
              </div>
            ) : (
              <p className="p-5 text-sm text-muted-foreground">暂无授权</p>
            )}
            {(offset > 0 || list.data.length > 20) && (
              <div className="flex items-center justify-end gap-2 border-t p-4 text-sm">
                <span>第 {offset / 20 + 1} 页</span>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={offset === 0}
                  onClick={() => setOffset(offset - 20)}
                >
                  上一页
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={list.data.length <= 20}
                  onClick={() => setOffset(offset + 20)}
                >
                  下一页
                </Button>
              </div>
            )}
          </>
        )}
        {removing && (
          <div className="m-5 space-y-3 rounded border bg-muted/40 p-4 text-sm">
            <p>
              确认撤销 {removing.hostname} / {removing.secretName}{" "}
              的授权？引用它的入口将失效。
            </p>
            <div className="flex gap-2">
              <Button
                variant="destructive"
                disabled={revoke.isPending}
                onClick={() => revoke.mutate()}
              >
                确认撤销
              </Button>
              <Button variant="ghost" onClick={() => setRemoving(null)}>
                取消
              </Button>
            </div>
            {revoke.error &&
              !(
                revoke.error instanceof ApiError &&
                revoke.error.code === "recent_authentication_required"
              ) && (
                <p role="alert" className="text-destructive">
                  {bindingError(revoke.error)}
                </p>
              )}
            <RecentAuthPrompt error={revoke.error} />
          </div>
        )}
      </section>
    </div>
  );
}
