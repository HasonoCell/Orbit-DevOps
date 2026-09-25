import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, FolderKanban, GitBranch, LayoutDashboard, LogOut, Menu, UserRound } from "lucide-react";
import { useState } from "react";
import { Link, NavLink, Outlet, useLocation, useMatch, useNavigate, useOutletContext } from "react-router-dom";
import { logout } from "@/api/auth";
import { getProject } from "@/api/catalog";
import { errorText, type CurrentPrincipal } from "@/api/http";
import { OrbitMark } from "@/components/OrbitMark";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";

/** 导航仅读取当前项目；不遍历应用或凭 URL 推断用户权限。 */
export function ConsoleShell() {
  const principal = useOutletContext<CurrentPrincipal>();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const location = useLocation();
  const projectId = useMatch("/projects/:projectId/*")?.params.projectId;
  const project = useQuery({ queryKey: ["project", projectId], queryFn: () => getProject(projectId!), enabled: !!projectId });
  const [mobileOpen, setMobileOpen] = useState(false);
  const exit = useMutation({ mutationFn: logout, onSuccess() {
    // 身份和业务缓存一起清理，防止下一位用户看到上一个会话的数据。
    queryClient.clear();
    navigate("/login", { replace: true });
  } });
  const projectName = !project.error ? project.data?.name : undefined;
  const items = [
    { to: "/projects", label: "所有项目", icon: FolderKanban, active: location.pathname === "/projects" },
    ...(projectId && projectName ? [
      { to: `/projects/${projectId}`, label: "项目工作台", icon: LayoutDashboard, active: location.pathname.startsWith(`/projects/${projectId}`) && new URLSearchParams(location.search).get("view") !== "delivery" },
      { to: `/projects/${projectId}?view=delivery`, label: "自动交付", icon: GitBranch, active: location.pathname === `/projects/${projectId}` && new URLSearchParams(location.search).get("view") === "delivery" },
    ] : []),
  ];
  const navigation = <>
    <Link to="/projects" onClick={() => setMobileOpen(false)} className="console-brand"><OrbitMark /><span>Orbit<small>DevOps console</small></span></Link>
    {projectName && <div className="console-project"><span>{projectName.slice(0, 1)}</span><div><strong>{projectName}</strong><small>项目空间</small></div></div>}
    <nav aria-label="主导航" className="console-nav">{items.map(({ to, label, icon: Icon, active }) => <Link key={to} to={to} aria-current={active ? "page" : undefined} onClick={() => setMobileOpen(false)}><Icon aria-hidden="true" className="size-4" />{label}</Link>)}</nav>
    <div className="console-nav console-account"><NavLink to="/account" onClick={() => setMobileOpen(false)}><UserRound aria-hidden="true" className="size-4" />账号设置</NavLink><p>应用交付与运行管理</p></div>
  </>;
  return <div className="console-layout">
    <a className="console-skip" href="#console-main">跳到主要内容</a>
    <aside className="console-sidebar">{navigation}</aside>
    <div className="console-workspace">
      <header className="console-header">
        <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
          <SheetTrigger asChild><Button variant="ghost" size="icon" className="md:hidden" aria-label="打开导航"><Menu /></Button></SheetTrigger>
          <SheetContent side="left" className="console-mobile-nav w-72"><SheetHeader className="sr-only"><SheetTitle>工作区导航</SheetTitle></SheetHeader>{navigation}</SheetContent>
        </Sheet>
        <div className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground"><Link to="/projects">项目</Link>{projectName && <><ChevronRight aria-hidden="true" className="size-3 shrink-0" /><Link className="truncate text-foreground" to={`/projects/${projectId}`}>{projectName}</Link></>}</div>
        <div className="ml-auto flex min-w-0 items-center gap-3"><span className="max-w-40 truncate text-xs text-muted-foreground">{principal.user?.displayName}</span><Button variant="ghost" size="icon" aria-label="退出登录" disabled={exit.isPending} onClick={() => exit.mutate()}><LogOut className="size-4" /></Button></div>
      </header>
      {exit.error && <div role="alert" className="border-b bg-destructive/5 p-3 text-sm text-destructive">{errorText(exit.error)}</div>}
      <main id="console-main" tabIndex={-1} className="console-main"><Outlet context={principal} /></main>
    </div>
  </div>;
}
