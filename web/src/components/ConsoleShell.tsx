import { useMutation, useQueryClient } from "@tanstack/react-query";
import { FolderKanban, LogOut, Menu, UserRound } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Link, NavLink, Outlet, useNavigate, useOutletContext } from "react-router-dom";
import { logout, principalQueryKey } from "@/api/auth";
import { errorText, type CurrentPrincipal } from "@/api/http";
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
      // 退出后移除身份与资源缓存，避免下一个登录用户看到上个会话的页面数据。
      queryClient.clear();
      navigate("/login", { replace: true });
    },
  });

  return <div className="min-h-dvh bg-background lg:grid lg:grid-cols-[232px_minmax(0,1fr)]">
    <aside className="hidden min-h-dvh flex-col bg-sidebar px-3 py-5 text-sidebar-foreground lg:flex" aria-label="全局导航">
      <Navigation onNavigate={() => {}} />
      <div className="mt-auto border-t border-sidebar-border px-3 pt-5"><p className="truncate text-sm font-medium">{principal.user?.displayName ?? "当前用户"}</p><p className="mt-1 text-xs text-slate-400">{principal.user?.platformRole === "platform_admin" ? "平台管理员" : "项目成员"}</p></div>
    </aside>
    <div className="min-w-0">
      <header className="flex h-16 items-center justify-between gap-3 border-b bg-card px-4 sm:px-7">
        <div className="flex items-center gap-3"><Sheet open={mobileOpen} onOpenChange={setMobileOpen}><SheetTrigger asChild><Button variant="ghost" size="icon" className="lg:hidden" aria-label="打开导航"><Menu /></Button></SheetTrigger><SheetContent side="left" className="w-72 bg-sidebar p-4 text-sidebar-foreground"><SheetHeader className="sr-only"><SheetTitle>全局导航</SheetTitle></SheetHeader><Navigation onNavigate={() => setMobileOpen(false)} /></SheetContent></Sheet><span className="text-sm font-medium text-muted-foreground">Orbit DevOps / 工作区</span></div>
        <div className="flex items-center gap-2"><span className="hidden max-w-40 truncate text-sm text-muted-foreground sm:block">{principal.user?.displayName}</span><Button variant="ghost" size="icon" aria-label="退出登录" disabled={exit.isPending} onClick={() => exit.mutate()}><LogOut /></Button></div>
      </header>
      {exit.error && <div role="alert" className="border-b border-destructive/30 bg-destructive/5 px-6 py-2 text-sm text-destructive">{errorText(exit.error)}</div>}
      <main className="mx-auto w-full max-w-7xl px-4 py-7 sm:px-7 sm:py-9"><Outlet context={principal} /></main>
    </div>
  </div>;
}

function Navigation({ onNavigate }: { onNavigate: () => void }) {
  return <nav className="flex flex-col gap-5" aria-label="主导航">
    <Link onClick={onNavigate} to="/projects" className="flex items-center gap-3 px-3 pb-5 text-base font-semibold"><span className="grid size-8 place-items-center rounded-lg bg-primary text-primary-foreground">O</span> Orbit DevOps</Link>
    <div className="space-y-1"><p className="px-3 pb-2 text-xs font-medium tracking-wider text-slate-400 uppercase">工作区</p><NavItem to="/projects" label="项目" icon={<FolderKanban className="size-4" />} onNavigate={onNavigate} /><NavItem to="/account" label="账号" icon={<UserRound className="size-4" />} onNavigate={onNavigate} /></div>
  </nav>;
}

function NavItem({ to, label, icon, onNavigate }: { to: string; label: string; icon: ReactNode; onNavigate: () => void }) {
  return <NavLink to={to} onClick={onNavigate} className={({ isActive }) => `flex h-10 items-center gap-3 rounded-lg px-3 text-sm transition-colors ${isActive ? "bg-sidebar-accent text-sidebar-accent-foreground" : "text-slate-300 hover:bg-sidebar-accent/70 hover:text-white"}`}>
    {icon}<span>{label}</span>
  </NavLink>;
}
