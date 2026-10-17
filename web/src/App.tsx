import { type ReactNode, useEffect, useState } from "react";
import { NavLink, Navigate, Outlet, createBrowserRouter, useLocation, useNavigate } from "react-router-dom";

import { signInPath } from "./api/client";
import { can, clearSession, getSession } from "./auth/session";
import Placeholder from "./routes/placeholder";
import SignIn from "./routes/signin";

function RequireAuth({ children }: { children: ReactNode }) {
  const location = useLocation();
  if (!getSession()) return <Navigate to={signInPath(location)} replace />;
  return <>{children}</>;
}

type Theme = "dark" | "light";

function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(() => {
    try {
      return (localStorage.getItem("fraud.theme") as Theme) || "dark";
    } catch {
      return "dark";
    }
  });
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    try {
      localStorage.setItem("fraud.theme", theme);
    } catch {
      // per-viewer convenience only
    }
  }, [theme]);
  return [theme, () => setTheme((t) => (t === "dark" ? "light" : "dark"))];
}

const navClass = ({ isActive }: { isActive: boolean }) =>
  `rounded px-2 py-1 text-sm ${isActive ? "bg-panel-raised text-text" : "text-muted hover:text-text"}`;

export function Layout() {
  const session = getSession();
  const navigate = useNavigate();
  const [theme, toggleTheme] = useTheme();
  return (
    <div className="min-h-screen">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:p-2">
        Skip to content
      </a>
      <header className="flex flex-wrap items-center gap-2 border-b border-border bg-panel px-4 py-2">
        <span className="mr-4 font-semibold">Fraud console</span>
        <nav aria-label="Main" className="flex flex-wrap gap-1">
          <NavLink to="/queue" className={navClass}>
            Queue
          </NavLink>
          <NavLink to="/live" className={navClass}>
            Live
          </NavLink>
          {can("shadow", session) && (
            <NavLink to="/shadow" className={navClass}>
              Model &amp; shadow
            </NavLink>
          )}
          <NavLink to="/rules" className={navClass}>
            Rules
          </NavLink>
        </nav>
        <div className="ml-auto flex items-center gap-3 text-sm">
          <button type="button" onClick={toggleTheme} className="text-muted hover:text-text">
            {theme === "dark" ? "Light theme" : "Dark theme"}
          </button>
          <span>
            {session?.persona.name} <span className="num text-xs text-muted">({session?.persona.role})</span>
          </span>
          <button
            type="button"
            className="text-muted hover:text-text"
            onClick={() => {
              clearSession();
              navigate("/sign-in", { replace: true });
            }}
          >
            Sign out
          </button>
        </div>
      </header>
      <main id="main">
        <Outlet />
      </main>
    </div>
  );
}

export const routes = [
  { path: "/sign-in", element: <SignIn /> },
  {
    element: (
      <RequireAuth>
        <Layout />
      </RequireAuth>
    ),
    children: [
      { index: true, element: <Navigate to="/queue" replace /> },
      { path: "/queue", element: <Placeholder title="Review queue" /> },
      { path: "/cases/:caseId", element: <Placeholder title="Case" /> },
      { path: "/accounts/:accountId", element: <Placeholder title="Account timeline" /> },
      { path: "/shadow", element: <Placeholder title="Model and shadow" /> },
      { path: "/live", element: <Placeholder title="Live metrics" /> },
      { path: "/rules", element: <Placeholder title="Rules" /> },
      { path: "*", element: <Placeholder title="Not found" /> },
    ],
  },
];

export const createRouter = () => createBrowserRouter(routes);
