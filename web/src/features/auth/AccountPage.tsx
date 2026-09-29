import { Link, useOutletContext } from "react-router-dom";
import type { CurrentPrincipal } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { AccountSessions } from "./AccountSessions";
import { AccountIdentities } from "./AccountIdentities";

export function AccountPage() {
  const principal = useOutletContext<CurrentPrincipal>();
  return (
    <div className="space-y-6">
      <div>
        <p className="text-sm text-muted-foreground">个人账号</p>
        <h1 className="mt-1 text-3xl font-semibold tracking-tight">账号信息</h1>
      </div>
      <Card>
        <CardHeader>
          <CardTitle>当前身份</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 text-sm">
          <Info label="显示名称" value={principal.user?.displayName ?? "—"} />
          <Info label="用户 ID" value={principal.user?.id ?? "—"} />
          <Info
            label="平台角色"
            value={
              principal.user?.platformRole === "platform_admin"
                ? "平台管理员"
                : "普通用户"
            }
          />
          <Info
            label="账号状态"
            value={principal.user?.status === "active" ? "正常" : "已停用"}
          />
        </CardContent>
      </Card>
      <AccountSessions />
      <AccountIdentities />
      <Card>
        <CardHeader>
          <CardTitle>账号安全</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="mb-4 text-sm text-muted-foreground">
            可以修改已有的本地密码；仅使用 OIDC
            登录的账号也可以首次设置本地登录名和密码。成功后所有现有会话都会被撤销。
          </p>
          <Button variant="outline" asChild>
            <Link to="/account/password">管理本地密码</Link>
          </Button>
        </CardContent>
      </Card>
    </div>
  );
}

function Info({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 border-b pb-3 last:border-b-0 last:pb-0">
      <span className="w-24 shrink-0 text-muted-foreground">{label}</span>
      <span className="break-all font-medium">{value}</span>
    </div>
  );
}
