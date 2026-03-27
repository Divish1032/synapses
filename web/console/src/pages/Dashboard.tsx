import { useState, useEffect, useCallback } from "preact/hooks";
import { get } from "../api";
import { useServices } from "../hooks/useServices";
import type { ComponentChildren } from "preact";

interface Project {
  path: string;
  name: string;
  hash: string;
  socket: string;
}

interface PulseSummary {
  tokens_saved?: number;
  cost_saved_usd?: number;
  total_tool_calls?: number;
  sessions?: number;
  tasks_completed?: number;
  context_deliveries?: number;
}

interface AgentStat {
  agent_id: string;
  sessions: number;
  tool_calls: number;
  tokens_saved: number;
  tasks_completed?: number;
  last_seen?: string;
}

function relTime(iso?: string): string {
  if (!iso) return "";
  try {
    const diff = Date.now() - new Date(iso).getTime();
    const m = Math.floor(diff / 60_000);
    if (m < 1) return "just now";
    if (m < 60) return `${m}m ago`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h ago`;
    return `${Math.floor(h / 24)}d ago`;
  } catch { return ""; }
}

function agentShortName(id: string): string {
  const l = id.toLowerCase();
  if (l.includes("claude")) return "Claude Code";
  if (l.includes("cursor")) return "Cursor";
  if (l.includes("windsurf")) return "Windsurf";
  if (l.includes("zed")) return "Zed";
  return id.length > 24 ? id.slice(0, 22) + "..." : id;
}

const STATUS_COLOR: Record<string, string> = {
  healthy: "var(--success)", degraded: "var(--warning)",
  offline: "var(--danger)", disabled: "var(--text-dim)", starting: "var(--warning)",
};

export function Dashboard({ onNav }: { onNav: (r: string) => void }) {
  const { services, restart, stop, startupError } = useServices();
  const [restarting, setRestarting] = useState<Record<string, boolean>>({});
  const [showServices, setShowServices] = useState(false);
  const [projects, setProjects] = useState<Project[]>([]);
  const [summary, setSummary] = useState<PulseSummary | null>(null);
  const [agents, setAgents] = useState<AgentStat[]>([]);
  const [offline, setOffline] = useState(false);
  const [loading, setLoading] = useState(true);
  const [updateInfo, setUpdateInfo] = useState<{ latest_version: string; changelog_url: string } | null>(null);
  const [updateDismissed, setUpdateDismissed] = useState(() => {
    try { return localStorage.getItem("synapses_update_dismissed") ?? ""; } catch { return ""; }
  });

  const healthy = services.filter((s) => s.status === "healthy").length;
  const total = services.length;
  const anyOffline = services.some((s) => s.status === "offline");
  const anyDegraded = services.some((s) => s.status === "degraded");

  const fetchData = useCallback(async () => {
    setLoading(true);
    try {
      const [projRes, pulseRes, updateRes] = await Promise.allSettled([
        get<Project[]>("/api/admin/projects"),
        get<any>("/api/admin/pulse/summary?days=7"),
        get<{ update_available: boolean; latest_version: string; changelog_url: string }>("/api/admin/update-check"),
      ]);
      if (projRes.status === "fulfilled") {
        const d = projRes.value;
        setProjects(Array.isArray(d) ? d : (d as any)?.projects ?? []);
      }
      if (pulseRes.status === "fulfilled") {
        setSummary(pulseRes.value.summary ?? null);
        setAgents((pulseRes.value.agents ?? []).slice(0, 6));
        setOffline(false);
      } else {
        setOffline(true);
      }
      if (updateRes.status === "fulfilled" && updateRes.value.update_available) {
        setUpdateInfo({ latest_version: updateRes.value.latest_version, changelog_url: updateRes.value.changelog_url });
      }
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetchData(); }, [fetchData]);

  const statusOk = total > 0 && healthy === total && !offline;
  const statusHeadline = offline
    ? "Engine is offline"
    : anyOffline
    ? `${total - healthy} service${total - healthy > 1 ? "s" : ""} offline`
    : anyDegraded
    ? "Running with issues"
    : total === 0
    ? "Starting up..."
    : "Synapses is running";

  const costStr = summary?.cost_saved_usd != null && summary.cost_saved_usd > 0
    ? `$${summary.cost_saved_usd.toFixed(2)}`
    : "-";

  return (
    <div className="page dash-page">
      {/* Hero status */}
      <div className="hero-status">
        <div className="hero-status-left">
          <button
            className={`hero-orb ${anyOffline ? "orb-danger" : anyDegraded || offline ? "orb-warning" : statusOk ? "orb-success" : "orb-dim"}`}
            title="Click to see services"
            onClick={() => setShowServices((v) => !v)}
            style={{ cursor: "pointer", border: "none", background: "none", padding: 0 }}
          />
          <div>
            <div className="hero-headline">{statusHeadline}</div>
            <div className="hero-subline">
              {projects.length > 0
                ? `${projects.length} project${projects.length !== 1 ? "s" : ""} indexed`
                : "No projects yet"}
            </div>
          </div>
        </div>
        <button className="btn-ghost" onClick={fetchData} title="Refresh">
          {loading ? "..." : "\u21BB"}
        </button>
      </div>

      {startupError && (
        <div className="offline-banner">
          <span>{startupError}</span>
        </div>
      )}

      {updateInfo && updateDismissed !== updateInfo.latest_version && (
        <div className="update-banner" style={{
          background: "var(--warning-bg, #3d3000)", border: "1px solid var(--warning, #f0a030)",
          borderRadius: 8, padding: "10px 16px", marginBottom: 16, display: "flex",
          alignItems: "center", justifyContent: "space-between", gap: 12
        }}>
          <span style={{ color: "var(--warning, #f0a030)" }}>
            Update available: <strong>{updateInfo.latest_version}</strong>
            {" \u2014 "}
            <code style={{ fontSize: 12 }}>synapses update</code>
            {updateInfo.changelog_url && (
              <>{" \u2014 "}<a href={updateInfo.changelog_url} target="_blank" rel="noopener"
                style={{ color: "var(--warning, #f0a030)", textDecoration: "underline" }}>changelog</a></>
            )}
          </span>
          <button
            className="btn-ghost"
            style={{ padding: "2px 8px", fontSize: 12 }}
            onClick={() => {
              setUpdateDismissed(updateInfo.latest_version);
              try { localStorage.setItem("synapses_update_dismissed", updateInfo.latest_version); } catch {}
            }}
            title="Dismiss"
          >{"\u2715"}</button>
        </div>
      )}

      {/* Services panel */}
      {showServices && services.length > 0 && (
        <div className="services-panel">
          <div className="services-panel-header">
            <span className="services-panel-title">Services</span>
          </div>
          <div className="services-panel-list">
            {services.map((s) => {
              const dot = STATUS_COLOR[s.status] ?? "var(--text-dim)";
              return (
                <div key={s.name} className="service-row">
                  <div className="service-row-left">
                    <div className="status-dot" style={{ background: dot }} />
                    <div>
                      <div className="service-name">{s.name}</div>
                      <div className="service-status-label" style={{ color: dot }}>
                        {s.status.charAt(0).toUpperCase() + s.status.slice(1)}
                      </div>
                    </div>
                  </div>
                  <div className="card-actions">
                    {s.status !== "disabled" && (
                      <button
                        className="icon-btn"
                        title="Restart"
                        disabled={restarting[s.name]}
                        onClick={async () => {
                          setRestarting((p) => ({ ...p, [s.name]: true }));
                          try { await restart(s.name); } finally {
                            setRestarting((p) => ({ ...p, [s.name]: false }));
                          }
                        }}
                      >
                        {restarting[s.name] ? "..." : "\u21BB"}
                      </button>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      )}

      {/* Value strip */}
      <div className="value-strip">
        <ValueCard color="var(--success)" value={costStr} label="Saved this week" sub="vs sending everything" />
        <ValueCard color="var(--accent)" value={summary?.context_deliveries != null ? String(summary.context_deliveries) : "-"} label="Context deliveries" sub="last 7 days" />
        <ValueCard color="var(--warning)" value={summary?.tasks_completed != null ? String(summary.tasks_completed) : summary?.sessions != null ? String(summary.sessions) : "-"} label={summary?.tasks_completed != null ? "Tasks done" : "Agent sessions"} sub="last 7 days" />
      </div>

      {/* Projects */}
      <section className="dash-section">
        <div className="dash-section-header">
          <h2 className="section-title" style={{ margin: 0 }}>Your Projects</h2>
          <a className="section-link" href="#" onClick={(e) => { e.preventDefault(); onNav("/projects"); }}>
            Manage &rsaquo;
          </a>
        </div>
        {projects.length === 0 ? (
          <a className="quick-action" href="#" onClick={(e) => { e.preventDefault(); onNav("/projects"); }} style={{ display: "flex", alignItems: "center", gap: 14 }}>
            <div className="quick-action-icon">+</div>
            <div>
              <div className="quick-action-label">Index your first project</div>
              <div className="quick-action-desc">Point Synapses at a codebase to start</div>
            </div>
          </a>
        ) : (
          <div className="dash-project-grid">
            {projects.slice(0, 4).map((p) => (
              <div
                key={p.path}
                className="dash-project-card dash-project-card-clickable"
                onClick={() => onNav(`/projects/${encodeURIComponent(p.path)}`)}
                role="button"
                tabIndex={0}
              >
                <div className="dash-project-card-header">
                  <span className="dash-project-name">{p.path.split("/").pop()}</span>
                </div>
                <div className="dash-project-meta">{p.path}</div>
              </div>
            ))}
            {projects.length > 4 && (
              <div className="dash-project-card dash-project-more">
                <span>+{projects.length - 4} more</span>
              </div>
            )}
          </div>
        )}
      </section>

      {/* Recent agent activity */}
      {agents.length > 0 && (
        <section className="dash-section">
          <div className="dash-section-header">
            <h2 className="section-title" style={{ margin: 0 }}>Recent Activity</h2>
            <a className="section-link" href="#" onClick={(e) => { e.preventDefault(); onNav("/activity"); }}>
              View all &rsaquo;
            </a>
          </div>
          <div className="agent-feed">
            {agents.map((a) => (
              <div key={a.agent_id} className="agent-feed-row">
                <div className="agent-feed-info">
                  <div className="agent-feed-id">{agentShortName(a.agent_id)}</div>
                  <div className="agent-feed-meta">
                    {a.sessions} session{a.sessions !== 1 ? "s" : ""}
                    {" \u00B7 "}
                    {a.tool_calls} calls
                    {a.last_seen && ` \u00B7 ${relTime(a.last_seen)}`}
                  </div>
                </div>
                {a.tasks_completed != null && a.tasks_completed > 0 && (
                  <div className="agent-feed-badge">{a.tasks_completed} done</div>
                )}
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  );
}

function ValueCard({ color, value, label, sub }: { color: string; value: string; label: string; sub?: string }) {
  return (
    <div className="value-card">
      <div className="value-number" style={{ color }}>{value}</div>
      <div className="value-label">{label}</div>
      {sub && <div className="value-sub">{sub}</div>}
    </div>
  );
}
