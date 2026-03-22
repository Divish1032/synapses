import { useState, useEffect } from "preact/hooks";
import { Sidebar } from "./components/Sidebar";
import { Toasts } from "./components/Toasts";
import { ToastProvider } from "./context/ToastContext";
import { Dashboard } from "./pages/Dashboard";
import { Projects } from "./pages/Projects";
import { Settings } from "./pages/Settings";
import { Activity } from "./pages/Activity";
import "./app.css";

function AppShell() {
  const [route, setRoute] = useState(window.location.hash.slice(1) || "/");

  const onNav = (r: string) => {
    window.location.hash = r;
    setRoute(r);
  };

  // Listen for hash changes (back/forward)
  useEffect(() => {
    const handler = () => setRoute(window.location.hash.slice(1) || "/");
    window.addEventListener("hashchange", handler);
    return () => window.removeEventListener("hashchange", handler);
  }, []);

  let page;
  switch (route) {
    case "/projects":  page = <Projects />; break;
    case "/activity":  page = <Activity />; break;
    case "/settings":  page = <Settings />; break;
    default:           page = <Dashboard onNav={onNav} />; break;
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
