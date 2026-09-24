import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { LockKeyhole } from "lucide-react";
import { useForm } from "react-hook-form";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { z } from "zod";
import { getCurrentPrincipal, listAuthProviders, loginLocal, principalQueryKey, startOIDCLogin } from "@/api/auth";
import { ApiError, errorText } from "@/api/http";
import { ErrorPanel } from "@/components/PageState";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { oidcReturnPathKey, safeNextPath } from "@/lib/return-path";

const loginSchema = z.object({
  loginName: z.string().trim().min(3, "登录名至少需要 3 个字符"),
  password: z.string().min(1, "请输入密码"),
});
type LoginValues = z.infer<typeof loginSchema>;

export function LoginPage() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const next = safeNextPath(searchParams.get("next"));
  const principal = useQuery({ queryKey: principalQueryKey, queryFn: getCurrentPrincipal, retry: false });
  const providers = useQuery({ queryKey: ["auth-providers"], queryFn: listAuthProviders, retry: false });
  const form = useForm<LoginValues>({ resolver: zodResolver(loginSchema), defaultValues: { loginName: "", password: "" } });
  const login = useMutation({
    mutationFn: (values: LoginValues) => loginLocal(values.loginName, values.password),
    onSuccess(result) {
      // 登录者可能已变化；先丢弃上一个会话的资源缓存，再写入新身份。
      queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== "principal" && query.queryKey[0] !== "auth-providers" });
      queryClient.setQueryData(principalQueryKey, result);
      navigate(result.kind === "pending" ? "/admission" : result.mustChangePassword ? "/account/password" : next, { replace: true });
    },
  });
  const oidc = useMutation({
    mutationFn: startOIDCLogin,
    onSuccess(authorizationUrl) {
      // OIDC 经外部站点往返；只暂存已校验的资源路径，不保存凭据。
      sessionStorage.setItem(oidcReturnPathKey, next);
      window.location.assign(authorizationUrl);
    },
  });

  if (principal.isSuccess) {
    return <Navigate to={principal.data.kind === "pending" ? "/admission" : principal.data.mustChangePassword ? "/account/password" : next} replace />;
  }
  const methods = providers.data?.filter((provider) => provider.available) ?? [];
  const localAvailable = methods.some((provider) => provider.type === "local");
  const oidcProviders = methods.filter((provider) => provider.type === "oidc");

  return <div className="flex min-h-dvh flex-col bg-white text-[#24292f]">
    <main className="mx-auto w-full max-w-[448px] flex-1 px-6 pb-16 pt-16 sm:pt-20">
      <div className="mb-10 text-center">
        <OrbitMark />
        <h1 className="mt-6 text-2xl font-semibold tracking-tight">登录 Orbit DevOps</h1>
      </div>

      <div className="space-y-6">
        {providers.isPending && <p role="status" className="text-center text-sm text-muted-foreground">正在查询登录方式…</p>}
        {providers.error && <ErrorPanel title="无法查询登录方式" error={providers.error} onRetry={() => void providers.refetch()} />}
        {!providers.isPending && !providers.error && methods.length === 0 && <Alert><LockKeyhole className="size-4" /><AlertDescription>当前未开放登录方式，请联系管理员。</AlertDescription></Alert>}

        {localAvailable && <form className="space-y-5" onSubmit={form.handleSubmit((values) => login.mutate(values))}>
          <div className="space-y-2">
            <Label className="text-sm font-semibold" htmlFor="login-name">登录名</Label>
            <Input className="h-11 rounded-md border-[#d0d7de] bg-white px-3 text-base shadow-none focus-visible:border-[#0969da] focus-visible:ring-[#0969da]/20" id="login-name" autoComplete="username" {...form.register("loginName")} aria-invalid={!!form.formState.errors.loginName} />
            <FieldError text={form.formState.errors.loginName?.message} />
          </div>
          <div className="space-y-2">
            <Label className="text-sm font-semibold" htmlFor="login-password">密码</Label>
            <Input className="h-11 rounded-md border-[#d0d7de] bg-white px-3 text-base shadow-none focus-visible:border-[#0969da] focus-visible:ring-[#0969da]/20" id="login-password" type="password" autoComplete="current-password" {...form.register("password")} aria-invalid={!!form.formState.errors.password} />
            <FieldError text={form.formState.errors.password?.message} />
          </div>
          {login.error && <Alert variant="destructive" role="alert"><AlertDescription>{login.error instanceof ApiError && login.error.status === 401 ? "登录名或密码不正确。" : errorText(login.error)}</AlertDescription></Alert>}
          <Button className="mt-1 h-11 w-full rounded-md border-[#1f883d] bg-[#1f883d] text-sm font-semibold text-white shadow-none hover:bg-[#1a7f37]" disabled={login.isPending} type="submit">{login.isPending ? "正在登录…" : "登录"}</Button>
        </form>}

        {oidcProviders.length > 0 && <div className="space-y-4">
          {localAvailable && <div className="flex items-center gap-4 text-sm text-[#57606a]" aria-hidden="true"><span className="h-px flex-1 bg-[#d8dee4]" /><span>或</span><span className="h-px flex-1 bg-[#d8dee4]" /></div>}
          <div className="space-y-2.5">
            {oidcProviders.map((provider) => <Button className="h-11 w-full rounded-md border-[#d0d7de] bg-[#f6f8fa] text-sm font-medium text-[#24292f] shadow-none hover:bg-[#eef1f4]" variant="outline" key={provider.id} disabled={oidc.isPending} onClick={() => oidc.mutate(provider.id)}>{provider.displayName}</Button>)}
          </div>
          {oidc.error && <p role="alert" className="text-sm text-destructive">{errorText(oidc.error)}</p>}
        </div>}
      </div>
    </main>
    <footer className="border-t border-[#d8dee4] bg-[#f6f8fa] px-6 py-5 text-center text-xs text-[#57606a]">Orbit DevOps · 仅供授权用户使用</footer>
  </div>;
}

function OrbitMark() {
  return <svg aria-hidden="true" className="mx-auto size-14 text-[#24292f]" fill="none" viewBox="0 0 56 56">
    <circle cx="28" cy="28" r="23" fill="currentColor" />
    <ellipse cx="28" cy="28" rx="17" ry="7" stroke="white" strokeWidth="2.5" transform="rotate(-35 28 28)" />
    <circle cx="28" cy="28" r="4.5" fill="white" />
    <circle cx="40.5" cy="18" r="2.5" fill="white" />
  </svg>;
}

function FieldError({ text }: { text?: string }) {
  return text ? <p role="alert" className="text-sm text-destructive">{text}</p> : null;
}
