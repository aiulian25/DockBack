import React, { useEffect, useState } from "react";
import ReactDOM from "react-dom/client";
import { createBrowserRouter, RouterProvider, Navigate } from "react-router-dom";
import "./index.css";
import { api, sessionGate } from "./api";
import { applyTheme, getTheme } from "./theme";
import Layout from "./components/Layout";

// Apply the saved theme (the inline script in index.html handles pre-paint; this
// is a belt-and-suspenders re-apply at module load).
applyTheme(getTheme());
import { Spinner } from "./components/ui";
import { ToastProvider } from "./components/Toast";
import Login from "./pages/Login";

// Route-level code splitting (perf Fix 3): every page except Login (needed
// before auth) and Layout (the persistent shell) loads as its own chunk on
// first navigation, so the initial bundle carries only the shell + login.
//
// lazyPage adds the stale-chunk recovery: after an upgrade, an old tab's next
// navigation can request a hashed chunk that no longer exists — reload ONCE to
// pick up the new index.html (no-cache + ETag make that fetch fresh), and clear
// the marker on any successful load so the guard re-arms.
const lazyPage = (load: () => Promise<{ default: React.ComponentType }>) =>
  React.lazy(() =>
    load().then((m) => {
      sessionStorage.removeItem("dback.chunkReload");
      return m;
    }).catch((err) => {
      if (!sessionStorage.getItem("dback.chunkReload")) {
        sessionStorage.setItem("dback.chunkReload", "1");
        location.reload();
        return new Promise<{ default: React.ComponentType }>(() => {}); // reloading — never resolves
      }
      throw err;
    })
  );

const Dashboard = lazyPage(() => import("./pages/Dashboard"));
const Servers = lazyPage(() => import("./pages/Servers"));
const Machine = lazyPage(() => import("./pages/Machine")); // F105
const NodeDetail = lazyPage(() => import("./pages/NodeDetail"));
const ContainerDetail = lazyPage(() => import("./pages/ContainerDetail"));
// F224: the two stack flows, no longer modals.
// F225: backing up ONE container — the form that used to be a third of the
// container page, with the settings it competed with grouped beside it.
const ContainerBackup = lazyPage(() => import("./pages/ContainerBackup"));
const StackDetail = lazyPage(() => import("./pages/StackDetail"));
const StackRestore = lazyPage(() => import("./pages/StackRestore"));
const Backups = lazyPage(() => import("./pages/Backups"));
const Insights = lazyPage(() => import("./pages/Insights"));
const Recovery = lazyPage(() => import("./pages/Recovery"));
const Logs = lazyPage(() => import("./pages/Logs"));
const AuditTrail = lazyPage(() => import("./pages/AuditTrail"));
const Settings = lazyPage(() => import("./pages/Settings"));
const Docs = lazyPage(() => import("./pages/Docs"));

function Protected({ children }: { children: React.ReactNode }) {
  // Perf Fix 4: the session is validated ONCE per page load — every later
  // navigation renders immediately (no probe, no full-screen spinner). Expiry
  // safety is unchanged: any 401 from any API call hard-redirects to /login
  // (api.ts), and Layout's expiry banner still watches the session clock;
  // logout resets the gate (Layout.tsx).
  const [state, setState] = useState<"loading" | "in" | "out">(sessionGate.checked ? "in" : "loading");
  useEffect(() => {
    if (sessionGate.checked) return;
    api.me().then(() => { sessionGate.checked = true; setState("in"); }).catch(() => setState("out"));
  }, []);
  if (state === "loading")
    return <div className="grid h-full place-items-center"><Spinner /></div>;
  if (state === "out") return <Navigate to="/login" replace />;
  // Suspense INSIDE Layout so a lazily-loading page swaps within the content
  // area while the shell (sidebar/topbar) stays stable — never above Layout.
  return (
    <Layout>
      <React.Suspense fallback={<div className="grid h-full place-items-center"><Spinner /></div>}>
        {children}
      </React.Suspense>
    </Layout>
  );
}

// Data router (createBrowserRouter) so pages can use useBlocker to warn on
// navigation away with unsaved changes (Settings).
const router = createBrowserRouter([
  { path: "/login", element: <Login /> },
  { path: "/", element: <Protected><Dashboard /></Protected> },
  { path: "/servers", element: <Protected><Servers /></Protected> },
  { path: "/servers/:id", element: <Protected><NodeDetail /></Protected> },
  { path: "/servers/:id/machine", element: <Protected><Machine /></Protected> }, // F105
  { path: "/servers/:id/containers/:cid", element: <Protected><ContainerDetail /></Protected> },
  { path: "/servers/:id/containers/:cid/backup", element: <Protected><ContainerBackup /></Protected> },
  // F224: a compose project's own page, and its restore — the two stack flows
  // that used to be modals. Routed so they are linkable and survive a reload.
  { path: "/servers/:id/stacks/:project", element: <Protected><StackDetail /></Protected> },
  { path: "/servers/:id/stacks/:project/restore", element: <Protected><StackRestore /></Protected> },
  { path: "/backups", element: <Protected><Backups /></Protected> },
  { path: "/insights", element: <Protected><Insights /></Protected> },
  { path: "/recovery", element: <Protected><Recovery /></Protected> },
  { path: "/logs", element: <Protected><Logs /></Protected> },
  { path: "/audit", element: <Protected><AuditTrail /></Protected> },
  { path: "/settings", element: <Protected><Settings /></Protected> },
  { path: "/docs", element: <Protected><Docs /></Protected> },
  { path: "/docs/:cat/:art", element: <Protected><Docs /></Protected> },
  { path: "*", element: <Navigate to="/" replace /> },
]);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ToastProvider>
      <RouterProvider router={router} />
    </ToastProvider>
  </React.StrictMode>
);
