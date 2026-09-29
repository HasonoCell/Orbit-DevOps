import { useQuery, useQueryClient } from "@tanstack/react-query";
import { lazy, Suspense, useEffect } from "react";
import { Navigate, Outlet, Route, Routes, useLocation } from "react-router-dom";
import { getCurrentPrincipal, principalQueryKey } from "@/features/auth/api";
import { ApiError, sessionInvalidEvent } from "@/api/http";
import { ErrorPanel, LoadingPage } from "@/shared/PageState";

// 路由按需加载：登录页不下载工作台和 Kubernetes 运行观测代码。
const ConsoleShell = lazy(() =>
  import("@/app/ConsoleShell").then(({ ConsoleShell }) => ({
    default: ConsoleShell,
  })),
);
const AccountPage = lazy(() =>
  import("@/features/auth/AccountPage").then(({ AccountPage }) => ({
    default: AccountPage,
  })),
);
const AdmissionPage = lazy(() =>
  import("@/features/auth/AdmissionPage").then(({ AdmissionPage }) => ({
    default: AdmissionPage,
  })),
);
const ApplicationPage = lazy(() =>
  import("@/features/applications/ApplicationPage").then(
    ({ ApplicationPage }) => ({
      default: ApplicationPage,
    }),
  ),
);
const TargetPage = lazy(() =>
  import("@/features/applications/targets/TargetPage").then(
    ({ TargetPage }) => ({
      default: TargetPage,
    }),
  ),
);
const AuthCallbackPage = lazy(() =>
  import("@/features/auth/AuthCallbackPage").then(({ AuthCallbackPage }) => ({
    default: AuthCallbackPage,
  })),
);
const LoginPage = lazy(() =>
  import("@/features/auth/LoginPage").then(({ LoginPage }) => ({
    default: LoginPage,
  })),
);
const PasswordPage = lazy(() =>
  import("@/features/auth/PasswordPage").then(({ PasswordPage }) => ({
    default: PasswordPage,
  })),
);
const ProjectPage = lazy(() =>
  import("@/features/projects/ProjectPage").then(({ ProjectPage }) => ({
    default: ProjectPage,
  })),
);
const ProjectsPage = lazy(() =>
  import("@/features/projects/ProjectsPage").then(({ ProjectsPage }) => ({
    default: ProjectsPage,
  })),
);

function SessionBoundary() {
  const location = useLocation();
  const queryClient = useQueryClient();
  useEffect(() => {
    const recheck = () => {
      void queryClient.invalidateQueries({ queryKey: principalQueryKey });
    };
    window.addEventListener(sessionInvalidEvent, recheck);
    return () => window.removeEventListener(sessionInvalidEvent, recheck);
  }, [queryClient]);
  const principal = useQuery({
    queryKey: principalQueryKey,
    queryFn: getCurrentPrincipal,
    retry: false,
  });
  if (principal.isPending) return <LoadingPage label="正在确认身份" />;
  if (principal.error) {
    if (principal.error instanceof ApiError && principal.error.status === 401) {
      const next = encodeURIComponent(location.pathname + location.search);
      return <Navigate to={`/login?next=${next}`} replace />;
    }
    return (
      <ErrorPanel
        title="无法确认身份"
        error={principal.error}
        onRetry={() => void principal.refetch()}
      />
    );
  }
  if (principal.data.kind === "pending") {
    return location.pathname === "/admission" ? (
      <Outlet context={principal.data} />
    ) : (
      <Navigate to="/admission" replace />
    );
  }
  if (
    principal.data.mustChangePassword &&
    location.pathname !== "/account/password"
  ) {
    return <Navigate to="/account/password" replace />;
  }
  if (location.pathname === "/admission")
    return <Navigate to="/projects" replace />;
  return <Outlet context={principal.data} />;
}

export function App() {
  return (
    <Suspense fallback={<LoadingPage label="正在加载页面" />}>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/auth/callback" element={<AuthCallbackPage />} />
        <Route element={<SessionBoundary />}>
          <Route path="/admission" element={<AdmissionPage />} />
          <Route path="/account/password" element={<PasswordPage />} />
          <Route element={<ConsoleShell />}>
            <Route path="/projects" element={<ProjectsPage />} />
            <Route path="/projects/:projectId" element={<ProjectPage />} />
            <Route
              path="/projects/:projectId/applications/:applicationId"
              element={<ApplicationPage />}
            />
            <Route
              path="/projects/:projectId/applications/:applicationId/targets/:targetId"
              element={<TargetPage />}
            />
            <Route path="/account" element={<AccountPage />} />
          </Route>
        </Route>
        <Route path="/" element={<Navigate to="/projects" replace />} />
        <Route path="*" element={<Navigate to="/projects" replace />} />
      </Routes>
    </Suspense>
  );
}
