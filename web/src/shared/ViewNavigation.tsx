import { cn } from "cn";
import { Button } from "@/components/ui/button";

/** URL 视图导航共用按钮与样式；切换仍由页面更新路由，不模拟自动激活的 ARIA Tab。 */
export function ViewNavigation({
  label,
  value,
  items,
  onValueChange,
  className,
}: {
  label: string;
  value: string;
  items: readonly (readonly [string, string])[];
  onValueChange: (value: string) => void;
  className?: string;
}) {
  return (
    <nav
      aria-label={label}
      className={cn(
        "flex gap-[26px] overflow-x-auto border-b max-md:gap-[22px]",
        className,
      )}
    >
      {items.map(([key, text]) => (
        <Button
          key={key}
          type="button"
          variant="ghost"
          aria-pressed={value === key}
          className="h-auto cursor-pointer rounded-none border-0 border-b-2 border-transparent px-px pt-0 pb-[13px] text-[13px] font-normal text-muted-foreground hover:bg-transparent hover:text-foreground active:not-aria-[haspopup]:translate-y-0 aria-pressed:border-primary aria-pressed:font-semibold aria-pressed:text-foreground max-md:min-h-11 max-md:pt-2"
          onClick={() => onValueChange(key)}
        >
          {text}
        </Button>
      ))}
    </nav>
  );
}
