import { zodResolver } from "@hookform/resolvers/zod";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";

const targetSchema = z.object({
  replicas: z
    .number()
    .int("请输入整数")
    .min(1, "至少需要 1 个副本")
    .max(5, "最多 5 个副本"),
  containerPort: z
    .number()
    .int("请输入整数")
    .min(1, "端口应在 1–65535 之间")
    .max(65535, "端口应在 1–65535 之间"),
});

export type TargetValues = z.infer<typeof targetSchema>;

export function useTargetForm(values: TargetValues) {
  return useForm<TargetValues>({
    resolver: zodResolver(targetSchema),
    defaultValues: values,
    mode: "onBlur",
  });
}

export function TargetFields({
  form,
  disabled = false,
}: {
  form: ReturnType<typeof useTargetForm>;
  disabled?: boolean;
}) {
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <div className="space-y-2">
        <Label htmlFor="target-replicas">期望副本</Label>
        <Input
          id="target-replicas"
          type="number"
          min={1}
          max={5}
          disabled={disabled}
          {...form.register("replicas", { valueAsNumber: true })}
          aria-invalid={!!form.formState.errors.replicas}
        />
        {form.formState.errors.replicas && (
          <p role="alert" className="text-sm text-destructive">
            {form.formState.errors.replicas.message}
          </p>
        )}
      </div>
      <div className="space-y-2">
        <Label htmlFor="target-container-port">容器端口</Label>
        <Input
          id="target-container-port"
          type="number"
          min={1}
          max={65535}
          disabled={disabled}
          {...form.register("containerPort", { valueAsNumber: true })}
          aria-invalid={!!form.formState.errors.containerPort}
        />
        {form.formState.errors.containerPort && (
          <p role="alert" className="text-sm text-destructive">
            {form.formState.errors.containerPort.message}
          </p>
        )}
      </div>
    </div>
  );
}
