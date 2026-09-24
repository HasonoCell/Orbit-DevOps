import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useForm, type UseFormRegisterReturn } from "react-hook-form";
import { Link, useNavigate, useOutletContext } from "react-router-dom";
import { z } from "zod";
import { changePassword, principalQueryKey } from "@/api/auth";
import { errorText, type CurrentPrincipal } from "@/api/http";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

const passwordSchema = z.object({
  mode: z.enum(["change", "establish"]),
  currentPassword: z.string().optional(),
  loginName: z.string().optional(),
  newPassword: z.string().min(12, "新密码至少需要 12 个字符").max(512, "密码过长"),
  confirmPassword: z.string().min(1, "请再次输入新密码"),
}).superRefine((value, context) => {
  if (value.mode === "change" && !value.currentPassword) {
    context.addIssue({ code: "custom", path: ["currentPassword"], message: "请输入当前密码" });
  }
  if (value.mode === "establish") {
    const loginName = value.loginName?.trim().toLowerCase() ?? "";
    if (loginName.length < 3 || loginName.length > 128 || !/^[a-z0-9][a-z0-9._@+-]*$/.test(loginName)) {
      context.addIssue({ code: "custom", path: ["loginName"], message: "登录名需为 3–128 个字符，以字母或数字开头" });
    }
  }
  if (value.newPassword !== value.confirmPassword) {
    context.addIssue({ code: "custom", path: ["confirmPassword"], message: "两次输入的新密码不一致" });
  }
});
type PasswordValues = z.infer<typeof passwordSchema>;

export function PasswordPage() {
  const principal = useOutletContext<CurrentPrincipal>();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const form = useForm<PasswordValues>({ resolver: zodResolver(passwordSchema), defaultValues: { mode: "change", currentPassword: "", loginName: "", newPassword: "", confirmPassword: "" } });
  const mode = form.watch("mode");
  const mutation = useMutation({
    mutationFn: (values: PasswordValues) => changePassword(values.mode === "change"
      ? { currentPassword: values.currentPassword ?? "", newPassword: values.newPassword }
      : { loginName: values.loginName?.trim().toLowerCase() ?? "", newPassword: values.newPassword }),
    onSuccess() {
      // 两种密码命令成功后都会撤销全部 Session，旧身份不能继续使用。
      queryClient.removeQueries({ queryKey: principalQueryKey });
      navigate("/login", { replace: true });
    },
  });
  const switchMode = (next: PasswordValues["mode"]) => {
    form.setValue("mode", next);
    form.clearErrors();
    mutation.reset();
  };
  return <main className="flex min-h-dvh items-center justify-center p-6"><Card className="w-full max-w-lg"><CardHeader><h1 className="text-xl font-semibold">{principal.mustChangePassword ? "设置新密码" : "管理本地密码"}</h1><CardDescription>{principal.mustChangePassword ? "临时密码只能用于完成改密。修改后需重新登录。" : "选择与当前账号相符的方式。成功后所有会话将被撤销。"}</CardDescription></CardHeader><CardContent><form className="space-y-5" onSubmit={form.handleSubmit((values) => mutation.mutate(values))}>
    {!principal.mustChangePassword && <div role="group" aria-label="本地密码操作" className="flex flex-wrap gap-2"><Button type="button" variant={mode === "change" ? "secondary" : "outline"} onClick={() => switchMode("change")}>修改已有密码</Button><Button type="button" variant={mode === "establish" ? "secondary" : "outline"} onClick={() => switchMode("establish")}>首次设置本地密码</Button></div>}
    {mode === "change" ? <PasswordField id="current-password" label="当前密码" autoComplete="current-password" register={form.register("currentPassword")} error={form.formState.errors.currentPassword?.message} /> : <div className="space-y-2"><Label htmlFor="local-login-name">登录名</Label><Input id="local-login-name" autoComplete="username" autoCapitalize="none" spellCheck={false} {...form.register("loginName")} aria-invalid={!!form.formState.errors.loginName} />{form.formState.errors.loginName && <p role="alert" className="text-sm text-destructive">{form.formState.errors.loginName.message}</p>}<p className="text-xs text-muted-foreground">首次设置需要近期 OIDC 登录；如果认证已过期，请重新登录后再操作。</p></div>}
    <PasswordField id="new-password" label="新密码" autoComplete="new-password" register={form.register("newPassword")} error={form.formState.errors.newPassword?.message} />
    <PasswordField id="confirm-password" label="确认新密码" autoComplete="new-password" register={form.register("confirmPassword")} error={form.formState.errors.confirmPassword?.message} />
    {mutation.error && <Alert variant="destructive" role="alert"><AlertDescription>{errorText(mutation.error)}</AlertDescription></Alert>}
    <div className="flex items-center justify-between gap-4">{!principal.mustChangePassword && <Link className="text-sm text-primary hover:underline" to="/account">返回账号</Link>}<Button className="ml-auto" disabled={mutation.isPending} type="submit">{mutation.isPending ? "正在提交…" : mode === "establish" ? "确认设置" : "确认修改"}</Button></div>
  </form></CardContent></Card></main>;
}

function PasswordField({ id, label, autoComplete, register, error }: { id: string; label: string; autoComplete: string; register: UseFormRegisterReturn; error?: string }) {
  return <div className="space-y-2"><Label htmlFor={id}>{label}</Label><Input id={id} type="password" autoComplete={autoComplete} {...register} aria-invalid={!!error} />{error && <p role="alert" className="text-sm text-destructive">{error}</p>}</div>;
}
