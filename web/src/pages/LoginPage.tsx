import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowRight, LockKeyhole } from "lucide-react";
import { useForm } from "react-hook-form";
import { Link, Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { z } from "zod";
import { getCurrentPrincipal, listAuthProviders, loginLocal, principalQueryKey, startOIDCLogin } from "@/api/auth";
import { ApiError, errorText } from "@/api/http";
import { ErrorPanel } from "@/components/PageState";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

const loginSchema = z.object({
  loginName: z.string().trim().min(3, "登录名至少需要 3 个字符"),
  password: z.string().min(1, "请输入密码"),
});
type LoginValues = z.infer<typeof loginSchema>;

// next 只允许本站路径，避免登录后跳转到外部地址或回到登录页。
export function safeNextPath(next: string | null): string {
  if (!next || !next.startsWith("/") || next.startsWith("//") || next.includes("\\") || next.startsWith("/login") || next.startsWith("/auth/callback")) {
    return "/projects";
  }
  return next;
}

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
      queryClient.setQueryData(principalQueryKey, result);
      navigate(result.kind === "pending" ? "/admission" : result.mustChangePassword ? "/account/password" : next, { replace: true });
    },
  });
  const oidc = useMutation({
    mutationFn: startOIDCLogin,
    onSuccess(authorizationUrl) { window.location.assign(authorizationUrl); },
  });

  if (principal.isSuccess) {
    return <Navigate to={principal.data.kind === "pending" ? "/admission" : principal.data.mustChangePassword ? "/account/password" : next} replace />;
  }
  const methods = providers.data?.filter((provider) => provider.available) ?? [];
  const localAvailable = methods.some((provider) => provider.type === "local");
  const oidcProviders = methods.filter((provider) => provider.type === "oidc");

  return <div className="grid min-h-dvh bg-background lg:grid-cols-[minmax(0,1fr)_minmax(420px,0.85fr)]">
    <div className="hidden flex-col justify-between bg-sidebar p-12 text-sidebar-foreground lg:flex">
      <Link to="/projects" className="flex items-center gap-3 text-lg font-semibold"><span className="grid size-9 place-items-center rounded-xl bg-primary text-primary-foreground">O</span> Orbit DevOps</Link>
      <div className="max-w-lg"><p className="mb-4 text-sm font-semibold tracking-[0.18em] text-indigo-300 uppercase">Delivery control</p><h1 className="text-4xl font-semibold leading-tight">从代码到运行状态，<br />每一步都有据可查。</h1><p className="mt-6 text-base leading-7 text-slate-300">在一个工作区里定位项目、追踪构建、发布与访问入口。</p></div>
      <p className="text-sm text-slate-400">Orbit DevOps · 管理工作区</p>
    </div>
    <main className="flex min-h-dvh items-center justify-center px-5 py-12 sm:px-10">
      <div className="w-full max-w-md">
        <div className="mb-8 flex items-center gap-3 lg:hidden"><span className="grid size-9 place-items-center rounded-xl bg-primary text-primary-foreground">O</span><strong>Orbit DevOps</strong></div>
        <Card className="border-border/80 shadow-sm"><CardHeader className="space-y-2 pb-4"><CardTitle className="text-2xl">登录工作区</CardTitle><CardDescription className="text-sm">使用本地账号，或选择已配置的身份提供方。</CardDescription></CardHeader>
          <CardContent className="space-y-5">
            {providers.isPending && <p role="status" className="text-sm text-muted-foreground">正在查询登录方式…</p>}
            {providers.error && <ErrorPanel title="无法查询登录方式" error={providers.error} onRetry={() => void providers.refetch()} />}
            {!providers.isPending && !providers.error && methods.length === 0 && <Alert><LockKeyhole className="size-4" /><AlertDescription>当前未开放登录方式，请联系管理员。</AlertDescription></Alert>}
            {localAvailable && <form className="space-y-4" onSubmit={form.handleSubmit((values) => login.mutate(values))}>
              <div className="space-y-2"><Label htmlFor="login-name">登录名</Label><Input id="login-name" autoComplete="username" {...form.register("loginName")} aria-invalid={!!form.formState.errors.loginName} /><FieldError text={form.formState.errors.loginName?.message} /></div>
              <div className="space-y-2"><Label htmlFor="login-password">密码</Label><Input id="login-password" type="password" autoComplete="current-password" {...form.register("password")} aria-invalid={!!form.formState.errors.password} /><FieldError text={form.formState.errors.password?.message} /></div>
              {login.error && <Alert variant="destructive" role="alert"><AlertDescription>{login.error instanceof ApiError && login.error.status === 401 ? "登录名或密码不正确。" : errorText(login.error)}</AlertDescription></Alert>}
              <Button className="h-10 w-full" disabled={login.isPending} type="submit">{login.isPending ? "正在登录…" : "使用本地账号登录"}<ArrowRight className="size-4" /></Button>
            </form>}
            {oidcProviders.length > 0 && <div className="space-y-3 border-t pt-5"><p className="text-sm text-muted-foreground">其他登录方式</p>{oidcProviders.map((provider) => <Button className="h-10 w-full" variant="outline" key={provider.id} disabled={oidc.isPending} onClick={() => oidc.mutate(provider.id)}>{provider.displayName}</Button>)}{oidc.error && <p role="alert" className="text-sm text-destructive">{errorText(oidc.error)}</p>}</div>}
          </CardContent>
        </Card>
      </div>
    </main>
  </div>;
}

function FieldError({ text }: { text?: string }) {
  return text ? <p role="alert" className="text-sm text-destructive">{text}</p> : null;
}
