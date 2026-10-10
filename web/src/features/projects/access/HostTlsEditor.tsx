import { errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Select, SelectItem } from "@/components/ui/select";
import { QueryNotice } from "@/shared/OverviewUI";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { accessQueries } from "./api";
import { HostTlsMode, hostInput } from "./HostTlsMode";
import { useWriteHost } from "./mutations";

import type { Host } from "./types";

/** TLS 编辑保留已授权绑定的语义，不能把任意 Secret 名称当作授权。 */
export function HostTlsEditor({
  projectId,
  host,
  onAccepted,
}: {
  projectId: string;
  host: Host;
  onAccepted: () => void;
}) {
  const [mode, setMode] = useState<Host["tlsMode"]>(host.tlsMode);
  const [policyKey, setPolicyKey] = useState(host.issuerPolicyKey ?? "");
  const [bindingId, setBindingId] = useState(host.secretBindingId ?? "");
  const [bindingOffset, setBindingOffset] = useState(0);
  const [validation, setValidation] = useState("");
  const options = useQuery({
    ...accessQueries.options(projectId),
  });
  const bindings = useQuery({
    ...accessQueries.secretBindings(projectId, host.hostname, bindingOffset),
    enabled: mode === "existing_secret",
  });
  useEffect(() => {
    setMode(host.tlsMode);
    setPolicyKey(host.issuerPolicyKey ?? "");
    setBindingId(host.secretBindingId ?? "");
    setBindingOffset(0);
  }, [
    host.updatedAt,
    host.tlsMode,
    host.issuerPolicyKey,
    host.secretBindingId,
  ]);
  const selectedPolicy = options.data?.issuerPolicies.find(
    (policy) => policy.key === policyKey,
  );
  const selectedBinding = bindings.data?.find((item) => item.id === bindingId);
  const bindingSelectable = Boolean(
    selectedBinding ||
      (bindingId === host.secretBindingId &&
        host.secretBindingState === "active"),
  );
  const mutation = useWriteHost(projectId, host.id, () => {
    onAccepted();
    setValidation("");
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    if (mode === "existing_secret" && !bindingSelectable) {
      setValidation("当前 TLS Secret 授权已失效或不在可选列表中，请重新选择。");
      return;
    }
    if (mode === "managed" && !selectedPolicy) {
      setValidation("当前 Issuer Policy 不可用，请选择新 Policy。");
      return;
    }
    setValidation("");
    mutation.mutate(hostInput(host.hostname, mode, policyKey, bindingId));
  }
  return (
    <section className="workbench-panel mb-5 max-w-3xl">
      <header className="panel-heading">
        <h2>TLS 配置</h2>
      </header>
      <form onSubmit={submit} className="space-y-4 p-5 text-sm">
        <HostTlsMode
          id="access-detail-tls-mode"
          label="模式"
          value={mode}
          managedAvailable={!!options.data?.issuerPolicies.length}
          onChange={(value) => {
            setMode(value);
            setPolicyKey("");
            setBindingId("");
            setBindingOffset(0);
          }}
        />
        {options.isPending || options.error ? (
          <QueryNotice
            error={options.error}
            retry={() => void options.refetch()}
          />
        ) : (
          <>
            {mode === "managed" && (
              <div className="space-y-2">
                <Label htmlFor="access-detail-issuer">Issuer Policy</Label>
                <Select
                  id="access-detail-issuer"
                  className="w-full"
                  value={policyKey}
                  onValueChange={(value) => setPolicyKey(value)}
                >
                  <SelectItem value="">请选择</SelectItem>
                  {policyKey && !selectedPolicy && (
                    <SelectItem value={policyKey} disabled>
                      {policyKey}（已不可用）
                    </SelectItem>
                  )}
                  {options.data.issuerPolicies.map((policy) => (
                    <SelectItem key={policy.key} value={policy.key}>
                      {policy.key} · {policy.kind}/{policy.name}
                    </SelectItem>
                  ))}
                </Select>
              </div>
            )}
          </>
        )}
        {mode === "existing_secret" && (
          <div className="space-y-2">
            <Label htmlFor="access-detail-secret-binding">
              TLS Secret 授权
            </Label>
            {bindings.isPending || bindings.error ? (
              <QueryNotice
                error={bindings.error}
                retry={() => void bindings.refetch()}
              />
            ) : (
              <>
                <Select
                  id="access-detail-secret-binding"
                  className="w-full"
                  value={bindingId}
                  onValueChange={(value) => setBindingId(value)}
                >
                  <SelectItem value="">请选择</SelectItem>
                  {bindingId && !selectedBinding && (
                    <SelectItem
                      value={bindingId}
                      disabled={host.secretBindingState !== "active"}
                    >
                      {bindingId}（
                      {host.secretBindingState === "revoked"
                        ? "已撤销"
                        : "当前授权"}
                      ）
                    </SelectItem>
                  )}
                  {bindings.data.slice(0, 20).map((item) => (
                    <SelectItem key={item.id} value={item.id}>
                      {item.secretName} · {item.clusterRef}/{item.namespace}
                    </SelectItem>
                  ))}
                </Select>
                {!bindings.data.length && (
                  <p className="text-sm text-muted-foreground">
                    暂无 TLS Secret 授权
                  </p>
                )}
                {(bindingOffset > 0 || bindings.data.length > 20) && (
                  <div className="flex gap-2">
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      disabled={bindingOffset === 0}
                      onClick={() => {
                        setBindingId("");
                        setBindingOffset(Math.max(0, bindingOffset - 20));
                      }}
                    >
                      上一页
                    </Button>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      disabled={bindings.data.length <= 20}
                      onClick={() => {
                        setBindingId("");
                        setBindingOffset(bindingOffset + 20);
                      }}
                    >
                      下一页
                    </Button>
                  </div>
                )}
              </>
            )}
          </div>
        )}
        {mode === "managed" &&
          policyKey &&
          !options.isPending &&
          !selectedPolicy && (
            <p role="alert" className="text-amber-800">
              当前引用的 Issuer Policy 已不可用；请选择新 Policy。
            </p>
          )}
        {validation && (
          <p role="alert" className="text-destructive">
            {validation}
          </p>
        )}
        {mutation.error && (
          <p role="alert" className="text-destructive">
            {errorText(mutation.error)}
          </p>
        )}
        {mutation.isSuccess && (
          <p role="status" className="text-emerald-700">
            已保存，等待控制器更新。
          </p>
        )}
        <Button
          type="submit"
          disabled={
            mutation.isPending ||
            (mode === "existing_secret" && !bindingSelectable) ||
            (mode === "managed" && !selectedPolicy)
          }
        >
          {mutation.isPending ? "正在保存…" : "保存 TLS 配置"}
        </Button>
      </form>
    </section>
  );
}
