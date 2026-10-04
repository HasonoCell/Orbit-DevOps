import { errorText } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectItem } from "@/components/ui/select";
import { QueryNotice } from "@/shared/OverviewUI";
import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { accessQueries } from "./api";
import { HostTlsMode, hostInput } from "./HostTlsMode";
import { useWriteHost } from "./mutations";

import type { Host } from "./types";

/** 创建表单只持有输入草稿与选择状态；服务端写入和缓存刷新归 mutation hook。 */
export function HostCreate({
  projectId,
  projectName,
}: {
  projectId: string;
  projectName: string;
}) {
  const [hostname, setHostname] = useState("");
  const [tlsMode, setTlsMode] = useState<Host["tlsMode"]>("http_only");
  const [policyKey, setPolicyKey] = useState("");
  const [bindingId, setBindingId] = useState("");
  const [bindingOffset, setBindingOffset] = useState(0);
  const [validation, setValidation] = useState("");
  const navigate = useNavigate();
  const options = useQuery({
    ...accessQueries.options(projectId),
  });
  const bindings = useQuery({
    ...accessQueries.secretBindings(projectId, hostname.trim(), bindingOffset),
    enabled: tlsMode === "existing_secret" && hostname.trim().length >= 4,
  });
  const mutation = useWriteHost(projectId, undefined, (created) => {
    navigate(`/projects/${projectId}/access-hosts/${created.id}`);
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    const value = hostname.trim();
    if (!value || value.includes("/") || value.includes(":")) {
      setValidation("请输入完整域名，例如 payment.example.com。");
      return;
    }
    if (
      tlsMode === "managed" &&
      !options.data?.issuerPolicies.some((policy) => policy.key === policyKey)
    ) {
      setValidation("请选择当前可用的 Issuer Policy。");
      return;
    }
    if (
      tlsMode === "existing_secret" &&
      !bindings.data?.some((item) => item.id === bindingId)
    ) {
      setValidation("请选择当前域名可用的 TLS Secret 授权。");
      return;
    }
    setValidation("");
    mutation.mutate(hostInput(value, tlsMode, policyKey, bindingId));
  }
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div>
          <Link
            className="text-xs text-muted-foreground hover:text-primary"
            to={`/projects/${projectId}?view=access`}
          >
            {projectName} / 访问入口
          </Link>
          <h1 className="mt-2">添加访问域名</h1>
        </div>
      </div>
      <section className="workbench-panel max-w-3xl">
        <header className="panel-heading">
          <h2>入口配置</h2>
        </header>
        <form onSubmit={submit} className="space-y-5 p-5 text-sm">
          <div className="space-y-2">
            <Label htmlFor="access-hostname">域名</Label>
            <Input
              id="access-hostname"
              value={hostname}
              onChange={(event) => {
                setHostname(event.target.value);
                setBindingId("");
                setBindingOffset(0);
              }}
              placeholder="payment.example.com"
            />
          </div>
          <HostTlsMode
            id="access-tls-mode"
            label="TLS 模式"
            value={tlsMode}
            managedAvailable={!!options.data?.issuerPolicies.length}
            onChange={(value) => {
              setTlsMode(value);
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
              {!options.data.issuerPolicies.length && (
                <p className="text-xs text-muted-foreground">
                  当前没有可用的托管证书 Policy。
                </p>
              )}
              {tlsMode === "managed" && (
                <div className="space-y-2">
                  <Label htmlFor="access-issuer">Issuer Policy</Label>
                  <Select
                    id="access-issuer"
                    className="w-full"
                    value={policyKey}
                    onValueChange={(value) => setPolicyKey(value)}
                  >
                    <SelectItem value="">请选择</SelectItem>
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
          {tlsMode === "existing_secret" && (
            <div className="space-y-2">
              <Label htmlFor="access-secret-binding">TLS Secret 授权</Label>
              {hostname.trim().length < 4 ? (
                <p className="text-xs text-muted-foreground">
                  请先输入完整域名。
                </p>
              ) : bindings.isPending || bindings.error ? (
                <QueryNotice
                  error={bindings.error}
                  retry={() => void bindings.refetch()}
                />
              ) : (
                <>
                  <Select
                    id="access-secret-binding"
                    className="w-full"
                    value={bindingId}
                    onValueChange={(value) => setBindingId(value)}
                  >
                    <SelectItem value="">请选择</SelectItem>
                    {bindings.data.slice(0, 20).map((item) => (
                      <SelectItem key={item.id} value={item.id}>
                        {item.secretName} · {item.clusterRef}/{item.namespace}
                      </SelectItem>
                    ))}
                  </Select>
                  {!bindings.data.length && (
                    <p className="text-xs text-muted-foreground">
                      当前域名没有已授权的 TLS Secret。
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
          <p className="text-muted-foreground">
            创建后可配置 PathPrefix 路由。证书控制器与 DNS
            验证在详情中单独观察。
          </p>
          {validation && (
            <p role="alert" className="text-destructive">
              {validation}
            </p>
          )}
          {mutation.error && (
            <p role="alert" className="text-destructive">
              {errorText(mutation.error)}
              。结果未知时，相同输入重试会复用幂等键。
            </p>
          )}
          <Button
            type="submit"
            disabled={
              mutation.isPending ||
              (tlsMode === "managed" && !policyKey) ||
              (tlsMode === "existing_secret" && !bindingId)
            }
          >
            {mutation.isPending ? "正在创建…" : "创建域名"}
          </Button>
        </form>
      </section>
    </div>
  );
}
