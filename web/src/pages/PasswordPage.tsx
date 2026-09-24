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
  currentPassword: z.string().min(1, "请输入当前密码"),
  newPassword: z.string().min(12, "新密码至少需要 12 个字符"),
  confirmPassword: z.string().min(1, "请再次输入新密码"),
}).refine((value) => value.newPassword === value.confirmPassword, { path: ["confirmPassword"], message: "两次输入的新密码不一致" });
type PasswordValues = z.infer<typeof passwordSchema>;

export function PasswordPage() {
  const principal = useOutletContext<CurrentPrincipal>();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const form = useForm<PasswordValues>({ resolver: zodResolver(passwordSchema) });
  const mutation = useMutation({
    mutationFn: (values: PasswordValues) => changePassword(values.currentPassword, values.newPassword),
    onSuccess() {
      // 服务端已撤销全部设备的旧 Session，必须重新登录，不能沿用缓存中的 Principal。
      queryClient.removeQueries({ queryKey: principalQueryKey });
      navigate("/login", { replace: true });
    },
  });
  return <main className="flex min-h-dvh items-center justify-center p-6"><Card className="w-full max-w-lg"><CardHeader><h1 className="text-xl font-semibold">{principal.mustChangePassword ? "设置新密码" : "修改密码"}</h1><CardDescription>{principal.mustChangePassword ? "临时密码只能用于完成此步骤。修改后需重新登录。" : "修改后所有会话将被撤销，需要重新登录。"}</CardDescription></CardHeader><CardContent><form className="space-y-5" onSubmit={form.handleSubmit((values) => mutation.mutate(values))}>
    <PasswordField id="current-password" label="当前密码" autoComplete="current-password" register={form.register("currentPassword")} error={form.formState.errors.currentPassword?.message} />
    <PasswordField id="new-password" label="新密码" autoComplete="new-password" register={form.register("newPassword")} error={form.formState.errors.newPassword?.message} />
    <PasswordField id="confirm-password" label="确认新密码" autoComplete="new-password" register={form.register("confirmPassword")} error={form.formState.errors.confirmPassword?.message} />
    {mutation.error && <Alert variant="destructive" role="alert"><AlertDescription>{errorText(mutation.error)}</AlertDescription></Alert>}
    <div className="flex items-center justify-between gap-4">{!principal.mustChangePassword && <Link className="text-sm text-primary hover:underline" to="/account">返回账号</Link>}<Button className="ml-auto" disabled={mutation.isPending} type="submit">{mutation.isPending ? "正在修改…" : "确认修改"}</Button></div>
  </form></CardContent></Card></main>;
}

function PasswordField({ id, label, autoComplete, register, error }: { id: string; label: string; autoComplete: string; register: UseFormRegisterReturn; error?: string }) {
  return <div className="space-y-2"><Label htmlFor={id}>{label}</Label><Input id={id} type="password" autoComplete={autoComplete} {...register} aria-invalid={!!error} />{error && <p role="alert" className="text-sm text-destructive">{error}</p>}</div>;
}
