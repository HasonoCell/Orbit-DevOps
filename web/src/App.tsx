import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { Navigate, Outlet, Route, Routes, useLocation } from "react-router-dom";
import { getCurrentPrincipal, principalQueryKey } from "@/api/auth";
import { ApiError, sessionInvalidEvent } from "@/api/http";
import { ConsoleShell } from "@/components/ConsoleShell";
import { ErrorPanel, LoadingPage } from "@/components/PageState";
import { AccountPage } from "@/pages/AccountPage";
import { AdmissionPage } from "@/pages/AdmissionPage";
import { ApplicationPage } from "@/pages/ApplicationPage";
import { AuthCallbackPage } from "@/pages/AuthCallbackPage";
import { LoginPage } from "@/pages/LoginPage";
import { PasswordPage } from "@/pages/PasswordPage";
import { ProjectPage } from "@/pages/ProjectPage";
import { ProjectsPage } from "@/pages/ProjectsPage";

function SessionBoundary() {
  const location = useLocation();
  const queryClient = useQueryClient();
  useEffect(() => {
    const recheck = () => { void queryClient.invalidateQueries({ queryKey: principalQueryKey }); };
    window.addEventListener(sessionInvalidEvent, recheck);
    return () => window.removeEventListener(sessionInvalidEvent, recheck);
  }, [queryClient]);
  const principal = useQuery({ queryKey: principalQueryKey, queryFn: getCurrentPrincipal, retry: false });
  if (principal.isPending) return <LoadingPage label="正在确认身份" />;
  if (principal.error) {
    if (principal.error instanceof ApiError && principal.error.status === 401) {
      const next = encodeURIComponent(location.pathname + location.search);
      return <Navigate to={`/login?next=${next}`} replace />;
    }
    return <ErrorPanel title="无法确认身份" error={principal.error} onRetry={() => void principal.refetch()} />;
  }
  if (principal.data.kind === "pending") {
    return location.pathname === "/admission"
      ? <Outlet context={principal.data} />
      : <Navigate to="/admission" replace />;
  }
  if (principal.data.mustChangePassword && location.pathname !== "/account/password") {
    return <Navigate to="/account/password" replace />;
  }
  if (location.pathname === "/admission") return <Navigate to="/projects" replace />;
  return <Outlet context={principal.data} />;
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/auth/callback" element={<AuthCallbackPage />} />
      <Route element={<SessionBoundary />}>
        <Route path="/admission" element={<AdmissionPage />} />
        <Route path="/account/password" element={<PasswordPage />} />
        <Route element={<ConsoleShell />}>
          <Route path="/projects" element={<ProjectsPage />} />
          <Route path="/projects/:projectId" element={<ProjectPage />} />
          <Route path="/projects/:projectId/applications/:applicationId" element={<ApplicationPage />} />
          <Route path="/account" element={<AccountPage />} />
        </Route>
      </Route>
      <Route path="/" element={<Navigate to="/projects" replace />} />
      <Route path="*" element={<Navigate to="/projects" replace />} />
    </Routes>
  );
}
