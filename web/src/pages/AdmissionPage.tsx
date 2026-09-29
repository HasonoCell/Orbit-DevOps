import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Clock3 } from "lucide-react";
import { useNavigate, useOutletContext } from "react-router-dom";
import { logout, principalQueryKey } from "@/api/auth";
import { errorText, type CurrentPrincipal } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";

export function AdmissionPage() {
  const principal = useOutletContext<CurrentPrincipal>();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const exit = useMutation({
    mutationFn: logout,
    onSuccess() {
      queryClient.removeQueries({ queryKey: principalQueryKey });
      navigate("/login", { replace: true });
    },
  });
  return (
    <main className="flex min-h-dvh items-center justify-center p-6">
      <Card className="w-full max-w-lg">
        <CardHeader>
          <div className="mb-3 grid size-11 place-items-center rounded-xl bg-amber-100 text-amber-800">
            <Clock3 className="size-5" />
          </div>
          <h1 className="text-xl font-semibold">等待管理员准入</h1>
        </CardHeader>
        <CardContent className="space-y-5 text-sm text-muted-foreground">
          <p>
            {principal.externalIdentity?.displayName ?? "当前外部身份"}
            尚未关联正式用户。管理员完成审核后，请重新登录。
          </p>
          <p>此状态下不能访问项目或执行发布命令。</p>
          {exit.error && (
            <p role="alert" className="text-destructive">
              {errorText(exit.error)}
            </p>
          )}
          <Button
            variant="outline"
            onClick={() => exit.mutate()}
            disabled={exit.isPending}
          >
            退出当前会话
          </Button>
        </CardContent>
      </Card>
    </main>
  );
}
