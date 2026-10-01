import type { ComponentProps } from "react";
import { Collapsible as CollapsiblePrimitive } from "radix-ui";
import { ChevronRight } from "lucide-react";
import { cn } from "cn";

/** 独立折叠区保留原有详情语义；开关、aria 关联和键盘行为交给 Radix。 */
export function Collapsible({
  className,
  ...props
}: ComponentProps<typeof CollapsiblePrimitive.Root>) {
  return (
    <CollapsiblePrimitive.Root
      {...props}
      data-slot="collapsible"
      className={cn("group/collapsible", className)}
    />
  );
}

export function CollapsibleTrigger({
  className,
  children,
  indicator = true,
  ...props
}: Omit<ComponentProps<typeof CollapsiblePrimitive.Trigger>, "asChild"> & {
  indicator?: boolean;
}) {
  return (
    <CollapsiblePrimitive.Trigger
      {...props}
      data-slot="collapsible-trigger"
      className={cn(
        "group/collapsible-trigger inline-flex max-w-full cursor-pointer items-center gap-1.5 rounded-sm text-left outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 forced-colors:focus-visible:outline forced-colors:focus-visible:outline-2 disabled:cursor-not-allowed disabled:opacity-50 max-md:min-h-11",
        className,
      )}
    >
      {indicator ? (
        <ChevronRight
          aria-hidden="true"
          className="size-3.5 shrink-0 transition-transform duration-150 group-data-[state=open]/collapsible-trigger:rotate-90 motion-reduce:transition-none"
        />
      ) : null}
      {children}
    </CollapsiblePrimitive.Trigger>
  );
}

export function CollapsibleContent({
  className,
  ...props
}: Omit<ComponentProps<typeof CollapsiblePrimitive.Content>, "forceMount">) {
  return (
    <CollapsiblePrimitive.Content
      {...props}
      data-slot="collapsible-content"
      // 与 details 一样保留子树和草稿；关闭时隐藏内容，也从焦点与无障碍树中移除。
      forceMount
      className={cn("min-w-0 data-[state=closed]:hidden", className)}
    />
  );
}
