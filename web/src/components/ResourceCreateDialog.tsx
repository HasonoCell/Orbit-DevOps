import { zodResolver } from "@hookform/resolvers/zod";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { errorText } from "@/api/http";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

const resourceSchema = z.object({
  name: z.string().trim().min(1, "请输入名称").max(120, "名称不能超过 120 个字符"),
  slug: z.string().trim().regex(/^[a-z][a-z0-9-]*$/, "标识需以小写字母开头，只能包含小写字母、数字和连字符"),
});
export type ResourceValues = z.infer<typeof resourceSchema>;

export function ResourceCreateDialog({ kind, open, onOpenChange, onSubmit, pending, error }: {
  kind: "项目" | "应用";
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSubmit: (values: ResourceValues) => void;
  pending: boolean;
  error: unknown;
}) {
  const form = useForm<ResourceValues>({ resolver: zodResolver(resourceSchema), defaultValues: { name: "", slug: "" } });
  return <Dialog open={open} onOpenChange={(next) => { if (!pending) { onOpenChange(next); if (!next) form.reset(); } }}>
    <DialogContent className="sm:max-w-md"><DialogHeader><DialogTitle>创建{kind}</DialogTitle><DialogDescription>名称供人阅读，标识用于稳定识别，不建议频繁更改。</DialogDescription></DialogHeader>
      <form className="space-y-4" onSubmit={form.handleSubmit(onSubmit)}>
        <div className="space-y-2"><Label htmlFor="resource-name">{kind}名称</Label><Input id="resource-name" autoFocus {...form.register("name")} aria-invalid={!!form.formState.errors.name} />{form.formState.errors.name && <p role="alert" className="text-sm text-destructive">{form.formState.errors.name.message}</p>}</div>
        <div className="space-y-2"><Label htmlFor="resource-slug">{kind}标识</Label><Input id="resource-slug" placeholder="yuuki" autoCapitalize="none" spellCheck={false} {...form.register("slug")} aria-invalid={!!form.formState.errors.slug} />{form.formState.errors.slug && <p role="alert" className="text-sm text-destructive">{form.formState.errors.slug.message}</p>}</div>
        {error != null && <Alert variant="destructive" role="alert"><AlertDescription>{errorText(error)}</AlertDescription></Alert>}
        <DialogFooter><Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={pending}>取消</Button><Button type="submit" disabled={pending}>{pending ? "正在创建…" : `创建${kind}`}</Button></DialogFooter>
      </form>
    </DialogContent>
  </Dialog>;
}
