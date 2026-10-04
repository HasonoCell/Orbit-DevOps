import {
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
  type ComponentProps,
  type ReactNode,
} from "react";
import { Select as SelectPrimitive } from "radix-ui";
import { Check, ChevronDown, ChevronUp } from "lucide-react";
import { cn } from "cn";

// Radix 禁止空字符串 Item；保留业务表单可选的空值，不让内部占位值进入提交参数。
const emptyOption = "__orbit_empty_option__";
const encode = (value: string) => (value === "" ? emptyOption : value);

/** 统一组合 Radix Select：定位、碰撞处理、焦点与键盘由现成原语负责。
 * 与原有受控表单一样回传业务 value；空选项、禁用选项和外部 label 关联均保留。 */
export function Select({
  value,
  onValueChange,
  children,
  disabled,
  className,
  placeholder = "请选择",
  ref: forwardedRef,
  ...triggerProps
}: Omit<
  ComponentProps<typeof SelectPrimitive.Trigger>,
  "children" | "onChange"
> & {
  value: string;
  onValueChange: (value: string) => void;
  children: ReactNode;
  placeholder?: string;
}) {
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  useImperativeHandle(forwardedRef, () => trigger.current!);
  useEffect(() => {
    if (!open || !trigger.current) return;
    // 上方异步内容可能把锚点推到视口外；碰撞处理仍需贴着锚点，无法单独修复这种位移。
    // 只在菜单打开时按可见性变化保留当前操作位置，不使用持续逐帧的位置检查。
    let observing = true;
    const observer = new IntersectionObserver(
      (entries) => {
        if (!observing) return;
        for (const entry of entries)
          if (entry.target.isConnected && entry.intersectionRatio < 1)
            entry.target.scrollIntoView({
              block: "nearest",
              inline: "nearest",
            });
      },
      { threshold: 1 },
    );
    observer.observe(trigger.current);
    return () => {
      observing = false;
      observer.disconnect();
    };
  }, [open]);
  return (
    <SelectPrimitive.Root
      open={open}
      onOpenChange={setOpen}
      value={encode(value)}
      disabled={disabled}
      onValueChange={(next) => onValueChange(next === emptyOption ? "" : next)}
    >
      <SelectPrimitive.Trigger
        {...triggerProps}
        ref={trigger}
        data-slot="select-trigger"
        data-value={value}
        className={cn(
          "inline-flex h-9 max-w-full items-center justify-between gap-3 rounded-md border border-input bg-background px-2.5 py-0 text-[13px] outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 forced-colors:focus-visible:outline forced-colors:focus-visible:outline-2 forced-colors:focus-visible:outline-offset-2 disabled:cursor-not-allowed disabled:opacity-50 max-md:min-h-11 max-md:text-base",
          className,
        )}
      >
        <span className="min-w-0 truncate">
          <SelectPrimitive.Value placeholder={placeholder} />
        </span>
        <SelectPrimitive.Icon asChild>
          <ChevronDown
            aria-hidden="true"
            className="size-4 shrink-0 text-muted-foreground"
          />
        </SelectPrimitive.Icon>
      </SelectPrimitive.Trigger>
      <SelectPrimitive.Portal>
        <SelectPrimitive.Content
          data-slot="select-content"
          position="popper"
          side="bottom"
          align="start"
          sideOffset={4}
          collisionPadding={8}
          className="z-[60] max-h-[var(--radix-select-content-available-height)] min-w-[var(--radix-select-trigger-width)] max-w-[calc(100vw-1rem)] overflow-hidden rounded-md border bg-popover text-popover-foreground shadow-md"
        >
          <SelectPrimitive.ScrollUpButton className="flex items-center justify-center py-1">
            <ChevronUp aria-hidden="true" className="size-4" />
          </SelectPrimitive.ScrollUpButton>
          <SelectPrimitive.Viewport className="p-1">
            {children}
          </SelectPrimitive.Viewport>
          <SelectPrimitive.ScrollDownButton className="flex items-center justify-center py-1">
            <ChevronDown aria-hidden="true" className="size-4" />
          </SelectPrimitive.ScrollDownButton>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  );
}

export function SelectItem({
  value,
  children,
  className,
  ...props
}: ComponentProps<typeof SelectPrimitive.Item>) {
  return (
    <SelectPrimitive.Item
      {...props}
      value={encode(value)}
      data-slot="select-item"
      data-value={value}
      className={cn(
        "relative flex min-h-9 cursor-default items-center rounded-sm py-2 pr-8 pl-3 text-sm outline-none select-none max-md:min-h-11 data-[highlighted]:bg-accent data-[highlighted]:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50",
        className,
      )}
    >
      <SelectPrimitive.ItemText className="break-words">
        {children}
      </SelectPrimitive.ItemText>
      <SelectPrimitive.ItemIndicator className="absolute right-2 flex items-center justify-center">
        <Check aria-hidden="true" className="size-4" />
      </SelectPrimitive.ItemIndicator>
    </SelectPrimitive.Item>
  );
}
