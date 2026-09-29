import { useOutletContext, useSearchParams } from "react-router-dom";
import type { CurrentPrincipal } from "@/api/http";
import { EmptyState } from "@/shared/PageState";
import { SecretBindingsPanel } from "./SecretBindingsPanel";
import { UsersPanel } from "./UsersPanel";
import { AdmissionsPanel } from "./AdmissionsPanel";

export function PlatformPage() {
  const principal = useOutletContext<CurrentPrincipal>();
  const [params, setParams] = useSearchParams();
  const view =
    params.get("view") === "secrets"
      ? "secrets"
      : params.get("view") === "admissions"
        ? "admissions"
        : "users";
  if (principal.user?.platformRole !== "platform_admin") {
    return (
      <EmptyState
        title="无平台管理权限"
        description="请使用平台管理员账号访问。"
      />
    );
  }
  return (
    <div className="workbench-page">
      <div className="workbench-heading">
        <div>
          <p className="text-xs text-muted-foreground">平台管理</p>
          <h1 className="mt-1">安全与身份</h1>
        </div>
      </div>
      <nav className="workbench-tabs mb-5" aria-label="平台管理视图">
        {[
          ["users", "平台用户"],
          ["admissions", "OIDC 准入"],
          ["secrets", "TLS Secret 授权"],
        ].map(([key, label]) => (
          <button
            key={key}
            aria-pressed={view === key}
            onClick={() => setParams({ view: key })}
          >
            {label}
          </button>
        ))}
      </nav>
      {view === "users" ? (
        <UsersPanel />
      ) : view === "admissions" ? (
        <AdmissionsPanel />
      ) : (
        <SecretBindingsPanel />
      )}
    </div>
  );
}
