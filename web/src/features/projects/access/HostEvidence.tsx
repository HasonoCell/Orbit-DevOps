import { Fact, Timestamp } from "@/shared/OverviewUI";

import type { HostStatus } from "./types";

/** 分别展示调和、路由、证书与 DNS 证据，不能合并为单一健康结论。 */
export function HostEvidence({ status }: { status: HostStatus }) {
  const ready = (value: string) =>
    value === "ready"
      ? "就绪"
      : value === "not_ready"
        ? "未就绪"
        : value === "not_applicable"
          ? "不适用"
          : "未知";
  const sync = {
    pending: "待调和",
    applying: "应用中",
    applied: "已应用",
    retrying: "重试中",
    attention_required: "需要处理",
    deleting: "清理中",
  }[status.sync.state];
  const dns = {
    verified: "已验证",
    mismatch: "不匹配",
    unavailable: "不可用",
    not_configured: "未配置",
  }[status.dns.state];
  return (
    <div className="space-y-5 p-5 text-sm">
      <dl className="grid gap-5 sm:grid-cols-2">
        <Fact label="入口调和">
          {sync} · 修订 {status.sync.appliedRevision}/
          {status.sync.desiredRevision}
          {status.sync.lastErrorCode && (
            <span className="block text-destructive">
              {status.sync.lastErrorCode}
            </span>
          )}
        </Fact>
        <Fact label="Gateway / Listener">
          {ready(status.controller.gatewayState)} /{" "}
          {ready(status.controller.listenerState)}
        </Fact>
        <Fact label="证书 / Secret">
          {ready(status.controller.certificateState)} /{" "}
          {ready(status.controller.secretState)}
          {status.controller.certificateNotAfter && (
            <span className="block">
              有效期至{" "}
              <Timestamp value={status.controller.certificateNotAfter} />
            </span>
          )}
        </Fact>
        <Fact label="DNS 验证">
          {dns}
          {status.dns.errorCode && (
            <span className="block text-destructive">
              {status.dns.errorCode}
            </span>
          )}
        </Fact>
        <Fact label="Gateway 地址">
          {status.controller.addresses.join(", ") || "尚无地址"}
        </Fact>
        <Fact label="DNS 答案">
          {status.dns.answers.join(", ") || "尚无答案"}
        </Fact>
      </dl>
      {status.controller.routes.length > 0 && (
        <div>
          <h3 className="font-medium">HTTPRoute 控制器</h3>
          <ul className="mt-2 space-y-1 text-xs text-muted-foreground">
            {status.controller.routes.map((route) => (
              <li key={route.routeId}>
                {route.routeId} · Accepted {ready(route.accepted)} ·
                ResolvedRefs {ready(route.resolvedRefs)}
              </li>
            ))}
          </ul>
        </div>
      )}
      <p className="border-t pt-4 text-xs text-muted-foreground">
        Gateway/HTTPRoute、证书与 DNS 是不同观测。这里未主动验证公网 HTTP/HTTPS
        请求。
      </p>
    </div>
  );
}
