import { useMutation, useQueryClient } from "@tanstack/react-query";
import { FolderKanban, LogOut, Menu, UserRound } from "lucide-react";
import { useState } from "react";
import { Link, NavLink, Outlet, useNavigate, useOutletContext } from "react-router-dom";
import { logout } from "@/api/auth";
import { errorText, type CurrentPrincipal } from "@/api/http";
import { OrbitMark } from "@/components/OrbitMark";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";

export function ConsoleShell() {
  const principal = useOutletContext<CurrentPrincipal>();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [mobileOpen, setMobileOpen] = useState(false);
  const exit = useMutation({
    mutationFn: logout,
    onSuccess() {
      // 身份和业务缓存一起清理，防止下一位用户看到上一个会话的数据。
      queryClient.clear();
      navigate("/login", { replace: true });
    },
  });

  return <div className="min-h-dvh bg-background">
    <header className="border-b">
      <div className="mx-auto flex h-16 max-w-[1400px] items-center gap-6 px-4 sm:px-8">
        <Link to="/projects" className="flex shrink-0 items-center gap-2.5 whitespace-nowrap text-base font-semibold tracking-tight"><OrbitMark />Orbit DevOps</Link>
        <div className="hidden h-full md:block"><Navigation onNavigate={() => {}} /></div>
        <div className="ml-auto flex items-center gap-2">
          <span className="hidden max-w-40 truncate text-sm text-muted-foreground sm:block">{principal.user?.displayName}</span>
          <Button variant="ghost" size="icon" aria-label="退出登录" disabled={exit.isPending} onClick={() => exit.mutate()}><LogOut className="size-4" /></Button>
          <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
            <SheetTrigger asChild><Button variant="ghost" size="icon" className="md:hidden" aria-label="打开导航"><Menu /></Button></SheetTrigger>
            <SheetContent side="left" className="w-72 p-5"><SheetHeader className="px-0"><SheetTitle>工作区导航</SheetTitle></SheetHeader><Navigation mobile onNavigate={() => setMobileOpen(false)} /></SheetContent>
          </Sheet>
        </div>
      </div>
    </header>
    {exit.error && <div role="alert" className="border-b border-destructive/30 bg-destructive/5 px-6 py-2 text-sm text-destructive">{errorText(exit.error)}</div>}
    <main className="mx-auto w-full max-w-[1364px] px-4 py-6 sm:px-8 sm:py-8"><Outlet context={principal} /></main>
  </div>;
}

function Navigation({ mobile = false, onNavigate }: { mobile?: boolean; onNavigate: () => void }) {
  return <nav className={mobile ? "flex flex-col gap-2" : "flex h-full items-stretch gap-5"} aria-label="主导航">
    {[{ to: "/projects", label: "项目", icon: FolderKanban }, { to: "/account", label: "账号", icon: UserRound }].map(({ to, label, icon: Icon }) =>
      <NavLink key={to} to={to} onClick={onNavigate} className={({ isActive }) => `flex items-center gap-2 text-sm font-medium transition-colors ${mobile ? "rounded-md px-3 py-3" : "border-b-2 px-2"} ${isActive ? (mobile ? "bg-accent text-accent-foreground" : "border-primary text-foreground") : "border-transparent text-muted-foreground hover:text-foreground"}`}><Icon className="size-4" />{label}</NavLink>,
    )}
  </nav>;
}
