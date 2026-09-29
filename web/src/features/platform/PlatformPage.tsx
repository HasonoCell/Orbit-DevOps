import { useOutletContext } from "react-router-dom";
import type { CurrentPrincipal } from "@/api/http";
import { EmptyState } from "@/shared/PageState";
import { SecretBindingsPanel } from "./SecretBindingsPanel";

export function PlatformPage() {
  const principal = useOutletContext<CurrentPrincipal>();
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
      <SecretBindingsPanel />
    </div>
  );
}
