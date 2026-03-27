import { useState, useEffect } from "preact/hooks";
import { Sidebar } from "./components/Sidebar";
import { Toasts } from "./components/Toasts";
import { ToastProvider } from "./context/ToastContext";
import { Dashboard } from "./pages/Dashboard";
import { Projects } from "./pages/Projects";
import { Settings } from "./pages/Settings";
import { Activity } from "./pages/Activity";
import { Brain } from "./pages/Brain";
import { Onboarding } from "./pages/Onboarding";
import { ProjectDetail } from "./pages/ProjectDetail";
import { get } from "./api";
import "./app.css";

function AppShell() {
  const [route, setRoute] = useState(window.location.hash.slice(1) || "/");
  const [checkingOnboard, setCheckingOnboard] = useState(true);
  const [showOnboarding, setShowOnboarding] = useState(false);

  const onNav = (r: string) => {
    window.location.hash = r;
    setRoute(r);
  };

  useEffect(() => {
    const handler = () => setRoute(window.location.hash.slice(1) || "/");
    window.addEventListener("hashchange", handler);
    return () => window.removeEventListener("hashchange", handler);
  }, []);

  // Check if onboarding needed
  useEffect(() => {
    (async () => {
      try {
        const [configRes, projRes] = await Promise.allSettled([
          get<{ app_settings?: { onboarding_done?: boolean } }>("/api/admin/config"),
          get<{ projects: any[] }>("/api/admin/projects"),
        ]);
        const done = configRes.status === "fulfilled" && configRes.value?.app_settings?.onboarding_done;
        const projData = projRes.status === "fulfilled" ? projRes.value : null;
        const projList = Array.isArray(projData) ? projData : (projData?.projects ?? []);
        const hasProjects = projList.length > 0;
        if (!done && !hasProjects) {
          setShowOnboarding(true);
        }
      } catch { /* ignore, show dashboard */ }
      setCheckingOnboard(false);
    })();
  }, []);

  if (checkingOnboard) {
    return (
      <div className="app-layout">
        <div className="app-body" style={{ display: "flex", alignItems: "center", justifyContent: "center" }}>
          <span className="text-dim">Loading...</span>
        </div>
      </div>
    );
  }

  if (showOnboarding) {
    return (
      <Onboarding onComplete={() => {
        setShowOnboarding(false);
        onNav("/");
      }} />
    );
  }

  // Route matching
  let page;
  if (route.startsWith("/projects/")) {
    const encodedPath = route.slice("/projects/".length);
    const projectPath = decodeURIComponent(encodedPath);
    page = <ProjectDetail projectPath={projectPath} onBack={() => onNav("/projects")} />;
  } else {
    switch (route) {
      case "/projects":  page = <Projects onNav={onNav} />; break;
      case "/activity":  page = <Activity />; break;
      case "/settings":  page = <Settings />; break;
      case "/brain":     page = <Brain />; break;
      default:           page = <Dashboard onNav={onNav} />; break;
    }
  }

  return (
    <div className="app-layout">
      <Sidebar route={route} onNav={onNav} />
      <div className="app-body">
        <main className="main-content">
          {page}
        </main>
      </div>
      <Toasts />
    </div>
  );
}

export default function App() {
  return (
    <ToastProvider>
      <AppShell />
    </ToastProvider>
  );
}
