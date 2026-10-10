import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Clock3 } from "lucide-react";
import { useState } from "react";
import { useNavigate, useOutletContext } from "react-router-dom";
import {
  getCurrentPrincipal,
  logout,
  principalQueryKey,
} from "@/features/auth/api";
import { ApiError, errorText, type CurrentPrincipal } from "@/api/http";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";

export function AdmissionPage() {
  const principal = useOutletContext<CurrentPrincipal>();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [checkNotice, setCheckNotice] = useState("");
  const check = useMutation({
    mutationFn: getCurrentPrincipal,
    onSuccess(current) {
      if (current.kind === "user") {
        queryClient.setQueryData(principalQueryKey, current);
        navigate("/projects", { replace: true });
      } else {
        setCheckNotice("仍在等待准入。管理员处理后，请重新登录。");
      }
    },
    onError(error) {
      if (error instanceof ApiError && error.status === 401) {
        setCheckNotice("待准入会话已结束，请重新登录以获取审核结果。");
      }
    },
  });
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
          <p>{principal.externalIdentity?.displayName ?? "当前外部身份"}</p>
          {checkNotice && <p role="status">{checkNotice}</p>}
          {check.error &&
            !(
              check.error instanceof ApiError && check.error.status === 401
            ) && (
              <p role="alert" className="text-destructive">
                {errorText(check.error)}
              </p>
            )}
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              onClick={() => check.mutate()}
              disabled={check.isPending}
            >
              检查状态
            </Button>
            {check.error instanceof ApiError && check.error.status === 401 && (
              <Button
                variant="outline"
                onClick={() => {
                  queryClient.removeQueries({ queryKey: principalQueryKey });
                  navigate("/login", { replace: true });
                }}
              >
                前往登录
              </Button>
            )}
          </div>
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
