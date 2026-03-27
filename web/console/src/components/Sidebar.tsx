import { useEffect, useState } from "preact/hooks";
import { useServices } from "../hooks/useServices";

const NAV_ITEMS = [
  { to: "/",          label: "Home",     icon: "\u2302" },
  { to: "/projects",  label: "Projects", icon: "\u2750" },
  { to: "/brain",     label: "Brain",    icon: "\u2606" },
  { to: "/activity",  label: "Activity", icon: "\u2261" },
  { to: "/settings",  label: "Settings", icon: "\u2699" },
];

export function Sidebar({ route, onNav }: { route: string; onNav: (r: string) => void }) {
  const [dark, setDark] = useState<boolean>(() => {
    const saved = localStorage.getItem("synapses-theme");
    if (saved) return saved === "dark";
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  });
  const { services } = useServices();

  useEffect(() => {
    document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
    localStorage.setItem("synapses-theme", dark ? "dark" : "light");
  }, [dark]);

  const total = services.length;
  const healthy = services.filter((s) => s.status === "healthy").length;
  const anyOffline = services.some((s) => s.status === "offline");
  const anyDegraded = services.some((s) => s.status === "degraded");

  const statusColor = anyOffline
    ? "var(--danger)"
    : anyDegraded
    ? "var(--warning)"
    : total > 0 && healthy === total
    ? "var(--success)"
    : "var(--text-dim)";

  const statusLabel =
    total === 0
      ? "Starting..."
      : anyOffline
      ? `${total - healthy} offline`
      : anyDegraded
      ? "Degraded"
      : "Running";

  // Match active nav: /projects/... matches /projects
  const activeNav = (to: string) => {
    if (to === "/") return route === "/" || route === "";
    return route === to || route.startsWith(to + "/");
  };

  return (
    <nav className="sidebar">
      <div className="sidebar-logo">
        <span className="logo-icon">{"\u26A1"}</span>
        <span className="logo-text">Synapses</span>
      </div>

      <ul className="nav-list">
        {NAV_ITEMS.map(({ to, icon, label }) => (
          <li key={to}>
            <a
              href={`#${to}`}
              className={`nav-item ${activeNav(to) ? "active" : ""}`}
              onClick={(e) => { e.preventDefault(); onNav(to); }}
            >
              <span style={{ fontSize: 15 }}>{icon}</span>
              <span>{label}</span>
            </a>
          </li>
        ))}
      </ul>

      <div className="sidebar-footer">
        <div className="sidebar-status">
          <span
            className={`sidebar-status-dot ${!anyOffline && !anyDegraded && total > 0 ? "pulse" : ""}`}
            style={{ background: statusColor }}
          />
          <span className="sidebar-status-label" style={{ color: statusColor }}>
            {statusLabel}
          </span>
        </div>
        <div className="sidebar-footer-row">
          <span className="sidebar-version">v0.8.0</span>
          <button
            className="theme-toggle"
            onClick={() => setDark((d) => !d)}
            title={dark ? "Switch to light mode" : "Switch to dark mode"}
          >
            {dark ? "\u2600" : "\u263E"}
          </button>
        </div>
      </div>
    </nav>
  );
}
