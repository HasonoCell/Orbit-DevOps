import { useOutletContext, useSearchParams } from "react-router-dom";
import type { CurrentPrincipal } from "@/api/http";
import { EmptyState } from "@/shared/PageState";
import { ViewNavigation } from "@/shared/ViewNavigation";
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
      <ViewNavigation
        label="平台管理视图"
        value={view}
        className="mb-5"
        items={[
          ["users", "平台用户"],
          ["admissions", "OIDC 准入"],
          ["secrets", "TLS Secret 授权"],
        ]}
        onValueChange={(key) => setParams({ view: key })}
      />
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
