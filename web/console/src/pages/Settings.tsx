import { useState, useEffect, useCallback } from "preact/hooks";
import { get, api, callTool } from "../api";
import { useToast } from "../context/ToastContext";
import { useServices } from "../hooks/useServices";
import { StatusCard } from "../components/StatusCard";
import { Toggle } from "../components/ConfigEditor";

interface AgentInfo { Key: string; Display: string; Detected: boolean; }
interface Project { path: string; hash: string; socket: string; }
interface KBStats { plans: number; tasks: number; decisions: number; rules: number; }

const AGENTS = [
  { id: "claude", label: "Claude Code", configFile: ".mcp.json" },
  { id: "cursor", label: "Cursor", configFile: ".cursor/mcp.json" },
  { id: "windsurf", label: "Windsurf", configFile: ".windsurf/mcp_config.json" },
  { id: "zed", label: "Zed", configFile: ".zed/settings.json" },
  { id: "antigravity", label: "Antigravity", configFile: ".agent/mcp.json" },
];

const MANUAL_SNIPPET = JSON.stringify(
  { mcpServers: { synapses: { type: "http", url: "http://127.0.0.1:11435/mcp" } } },
  null, 2
);

export function Settings() {
  const { addToast } = useToast();
  const { services, restart } = useServices();
  const [section, setSection] = useState<"editors" | "engine" | "data" | "diagnostics">("editors");

  // Editors
  const [copied, setCopied] = useState<string | null>(null);
  const [projects, setProjects] = useState<Project[]>([]);
  const [detectedAgents, setDetectedAgents] = useState<AgentInfo[]>([]);
  const [connections, setConnections] = useState<Record<string, Record<string, boolean>>>({});
  const [connecting, setConnecting] = useState<string | null>(null);

  // Engine
  const [version, setVersion] = useState<{ running?: string; installed?: string } | null>(null);
  const [appSettings, setAppSettings] = useState<Record<string, any>>({});
  const [restarting, setRestarting] = useState<Record<string, boolean>>({});

  // Data
  const [dataSizes, setDataSizes] = useState<Record<string, number>>({});
  const [kbStats, setKBStats] = useState<KBStats | null>(null);
  const [clearing, setClearing] = useState<string | null>(null);

  // Diagnostics
  const [diagnostics, setDiagnostics] = useState<any>(null);
  const [diagLoading, setDiagLoading] = useState(false);
  const [logLines, setLogLines] = useState<string[]>([]);
  const [logLoading, setLogLoading] = useState(false);

  const daemonSvc = services.find((s) => s.name === "daemon");
  const daemonRunning = daemonSvc?.status === "healthy" || daemonSvc?.status === "degraded";

  // Fetch base data
  useEffect(() => {
    let cancelled = false;
    get<any>("/api/admin/projects")
      .then((r) => { if (!cancelled) setProjects(Array.isArray(r) ? r : r?.projects ?? []); })
      .catch(() => { if (!cancelled) setProjects([]); });
    get<AgentInfo[]>("/api/admin/agents/detect")
      .then((r) => { if (!cancelled) setDetectedAgents(r); })
      .catch(() => { if (!cancelled) setDetectedAgents([]); });
    get<any>("/api/admin/version")
      .then((r) => { if (!cancelled) setVersion(r); })
      .catch(() => {});
    get<any>("/api/admin/config")
      .then((r) => { if (!cancelled) setAppSettings(r?.app_settings ?? {}); })
      .catch(() => {});
    return () => { cancelled = true; };
  }, []);

  // Check connections
  useEffect(() => {
    if (projects.length === 0) return;
    let cancelled = false;
    const checks = projects.flatMap((p) =>
      AGENTS.map((a) =>
        get<{ configured: boolean }>(`/api/admin/agents/check?editor=${a.id}&project_path=${encodeURIComponent(p.path)}`)
          .then((r) => ({ path: p.path, agent: a.id, connected: r.configured }))
          .catch(() => ({ path: p.path, agent: a.id, connected: false }))
      )
    );
    Promise.all(checks).then((results) => {
      if (cancelled) return;
      const state: Record<string, Record<string, boolean>> = {};
      for (const { path, agent, connected } of results) {
        if (!state[path]) state[path] = {};
        state[path][agent] = connected;
      }
      setConnections(state);
    });
    return () => { cancelled = true; };
  }, [projects]);

  async function connectAgent(agentId: string, projectPath: string) {
    const key = `${projectPath}::${agentId}`;
    setConnecting(key);
    try {
      await api("/api/admin/agents/connect", {
        method: "POST",
        body: JSON.stringify({ agent: agentId, project_path: projectPath }),
      });
      setConnections((prev) => ({
        ...prev,
        [projectPath]: { ...prev[projectPath], [agentId]: true },
      }));
      addToast("success", `Connected - restart your editor to apply`);
    } catch (e: any) {
      addToast("error", e.message);
    } finally { setConnecting(null); }
  }

  async function copyText(text: string, key: string) {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(key);
      setTimeout(() => setCopied(null), 2000);
    } catch { /* ignore */ }
  }

  async function fetchDataInfo() {
    try {
      const sizesRes = await get<any>("/api/admin/pulse/graph").catch(() => ({}));
      if (sizesRes && typeof sizesRes === "object") setDataSizes(sizesRes);

      if (projects.length > 0) {
        try {
          const tasksRes = await callTool<any>("get_pending_tasks", projects[0].path);
          const taskList = Array.isArray(tasksRes?.tasks) ? tasksRes.tasks : (Array.isArray(tasksRes) ? tasksRes : []);
          setKBStats({
            plans: 0,
            tasks: taskList.length,
            decisions: 0,
            rules: 0,
          });
        } catch { /* tool may not be available */ }
      }
    } catch { /* ignore */ }
  }

  async function fetchDiagnostics() {
    setDiagLoading(true);
    try {
      const [healthRes, versionRes, servicesRes] = await Promise.allSettled([
        get<any>("/api/admin/health"),
        get<any>("/api/admin/version"),
        get<any>("/api/admin/services"),
      ]);
      const diag: Record<string, any> = {};
      if (healthRes.status === "fulfilled") {
        diag["daemon"] = { healthy: true, detail: "Running" };
        if (healthRes.value?.indexing_progress) {
          diag["indexing"] = { healthy: true, detail: healthRes.value.indexing_progress.state ?? "idle" };
        }
      } else {
        diag["daemon"] = { healthy: false, detail: "Unreachable" };
      }
      if (versionRes.status === "fulfilled") {
        diag["version"] = { healthy: true, detail: versionRes.value?.version ?? "unknown" };
      }
      if (servicesRes.status === "fulfilled") {
        const svcs = Array.isArray(servicesRes.value) ? servicesRes.value : [];
        const allHealthy = svcs.every((s: any) => s.status === "healthy");
        diag["services"] = { healthy: allHealthy, detail: `${svcs.length} service(s)` };
      }
      // Check Ollama
      try {
        const ollama = await get<any>("/api/admin/ollama");
        diag["ollama"] = { healthy: ollama?.running ?? false, detail: ollama?.running ? `v${ollama.version}` : "Not running" };
      } catch {
        diag["ollama"] = { healthy: false, detail: "Unreachable" };
      }
      setDiagnostics(diag);
    } catch (e: any) {
      addToast("error", e.message);
    } finally {
      setDiagLoading(false);
    }
  }

  async function fetchLogs() {
    setLogLoading(true);
    try {
      const res = await get<{ lines: string[] }>("/api/admin/logs?n=100");
      setLogLines(res.lines ?? []);
    } catch {
      setLogLines(["Failed to fetch logs"]);
    } finally { setLogLoading(false); }
  }

  async function clearData(type: string) {
    setClearing(type);
    try {
      switch (type) {
        case "activity":
          await api("/api/admin/pulse/data", { method: "DELETE" });
          addToast("success", "Activity data cleared");
          break;
        default:
          addToast("error", "Unknown clear type");
      }
    } catch (e: any) {
      addToast("error", e.message);
    } finally {
      setClearing(null);
    }
  }

  const detectedSet = new Set(detectedAgents.filter((a) => a.Detected).map((a) => a.Key));
  const sections = [
    { id: "editors" as const, label: "Editors", icon: "\u270E" },
    { id: "engine" as const, label: "Engine", icon: "\u2699" },
    { id: "data" as const, label: "Data", icon: "\u2637" },
    { id: "diagnostics" as const, label: "Diagnostics", icon: "\u2691" },
  ];

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1 className="page-title">Settings</h1>
          <span className="page-subtitle">Configure editors, engine, data, and diagnostics</span>
        </div>
      </div>

      {/* Section tabs */}
      <div className="settings-tabs">
        {sections.map((s) => (
          <button
            key={s.id}
            className={`settings-tab ${section === s.id ? "settings-tab-active" : ""}`}
            onClick={() => {
              setSection(s.id);
              if (s.id === "data") fetchDataInfo();
              if (s.id === "diagnostics") fetchLogs();
            }}
          >
            <span>{s.icon}</span>
            <span>{s.label}</span>
          </button>
        ))}
      </div>

      {/* Editors */}
      {section === "editors" && (
        <>
          <section className="dash-section">
            <h2 className="section-title">Connect Your Editor</h2>
            <div className="agent-grid">
              {AGENTS.map((a) => {
                const detected = detectedSet.has(a.id);
                const connectedToAny = projects.some((p) => connections[p.path]?.[a.id]);
                return (
                  <div key={a.id} className="agent-card">
                    <div className="agent-card-header">
                      <span className="agent-card-name">{a.label}</span>
                      <span className={`agent-badge ${connectedToAny ? "badge-success" : detected ? "badge-info" : "badge-dim"}`}>
                        {connectedToAny ? "Connected" : detected ? "Installed" : "Not found"}
                      </span>
                    </div>
                    {detected && projects.length > 0 && !connectedToAny && (
                      <button
                        className="btn-primary btn-sm"
                        disabled={connecting !== null}
                        onClick={() => connectAgent(a.id, projects[0].path)}
                      >
                        {connecting === `${projects[0].path}::${a.id}` ? "Connecting..." : "Connect"}
                      </button>
                    )}
                  </div>
                );
              })}
            </div>
            <div style={{ marginTop: 16 }}>
              <div className="text-dim" style={{ fontSize: 12, marginBottom: 4 }}>
                Manual: add to your MCP config file
              </div>
              <div className="code-block" style={{ position: "relative" }}>
                <pre style={{ fontSize: 11, margin: 0, overflow: "auto" }}>{MANUAL_SNIPPET}</pre>
                <button
                  className="btn-ghost btn-sm"
                  style={{ position: "absolute", top: 4, right: 4 }}
                  onClick={() => copyText(MANUAL_SNIPPET, "snippet")}
                >
                  {copied === "snippet" ? "\u2713" : "Copy"}
                </button>
              </div>
            </div>
          </section>

          <section className="dash-section">
            <h2 className="section-title">Privacy</h2>
            <div className="info-card">
              <p>All data stays on your machine. Synapses never phones home.</p>
              <p style={{ fontSize: 12, color: "var(--text-dim)" }}>
                Data directory: <code>~/.synapses/</code>
              </p>
            </div>
          </section>
        </>
      )}

      {/* Engine */}
      {section === "engine" && (
        <>
          <section className="dash-section">
            <h2 className="section-title">Daemon</h2>
            <div className="adv-grid">
              <div className="adv-card">
                <div className="adv-card-value" style={{ color: daemonRunning ? "var(--success)" : "var(--danger)" }}>
                  {daemonRunning ? "Running" : "Offline"}
                </div>
                <div className="adv-card-label">Status</div>
              </div>
              <div className="adv-card">
                <div className="adv-card-value">{version?.running ?? "-"}</div>
                <div className="adv-card-label">Version</div>
              </div>
              <div className="adv-card">
                <div className="adv-card-value">{projects.length}</div>
                <div className="adv-card-label">Projects</div>
              </div>
            </div>
          </section>

          <section className="dash-section">
            <h2 className="section-title">Services</h2>
            <div className="services-panel-list">
              {services.map((s) => {
                const color =
                  s.status === "healthy" ? "var(--success)" :
                  s.status === "degraded" ? "var(--warning)" :
                  s.status === "offline" ? "var(--danger)" : "var(--text-dim)";
                return (
                  <div key={s.name} className="service-row">
                    <div className="service-row-left">
                      <div className="status-dot" style={{ background: color }} />
                      <div>
                        <div className="service-name">{s.name}</div>
                        <div className="service-status-label" style={{ color }}>
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
          </section>
        </>
      )}

      {/* Data */}
      {section === "data" && (
        <>
          <section className="dash-section">
            <h2 className="section-title">Storage</h2>
            <div className="adv-grid">
              {Object.entries(dataSizes).map(([key, bytes]) => (
                <div key={key} className="adv-card">
                  <div className="adv-card-value">{formatBytes(bytes)}</div>
                  <div className="adv-card-label">{key}</div>
                </div>
              ))}
            </div>
          </section>

          {kbStats && (
            <section className="dash-section">
              <h2 className="section-title">Knowledge Base</h2>
              <div className="adv-grid">
                <div className="adv-card">
                  <div className="adv-card-value">{kbStats.plans}</div>
                  <div className="adv-card-label">Plans</div>
                </div>
                <div className="adv-card">
                  <div className="adv-card-value">{kbStats.tasks}</div>
                  <div className="adv-card-label">Tasks</div>
                </div>
                <div className="adv-card">
                  <div className="adv-card-value">{kbStats.decisions}</div>
                  <div className="adv-card-label">Decisions</div>
                </div>
                <div className="adv-card">
                  <div className="adv-card-value">{kbStats.rules}</div>
                  <div className="adv-card-label">Rules</div>
                </div>
              </div>
            </section>
          )}

          <section className="dash-section">
            <h2 className="section-title">Clear Data</h2>
            <div className="clear-actions">
              <button
                className="btn-ghost"
                disabled={clearing !== null}
                onClick={() => clearData("activity")}
              >
                {clearing === "activity" ? "Clearing..." : "Clear Activity Logs"}
              </button>
            </div>
            <p className="text-dim" style={{ fontSize: 11, marginTop: 8 }}>
              To wipe all data, run <code>synapses reset -all</code> from the terminal.
            </p>
          </section>
        </>
      )}

      {/* Diagnostics */}
      {section === "diagnostics" && (
        <>
          <section className="dash-section">
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
              <h2 className="section-title">Health Checks</h2>
              <button className="btn-ghost btn-sm" onClick={fetchDiagnostics} disabled={diagLoading}>
                {diagLoading ? "Running..." : "Run Diagnostics"}
              </button>
            </div>
            {diagnostics ? (
              <div className="health-grid">
                {Object.entries(diagnostics).map(([key, val]: [string, any]) => {
                  const status = typeof val === "object" && val !== null
                    ? (val.healthy === true || val.status === "healthy" ? "healthy" : val.healthy === false ? "error" : "unknown")
                    : "unknown";
                  const detail = typeof val === "string" ? val : typeof val === "object" && val !== null ? (val.detail ?? val.message ?? "") : "";
                  return (
                    <StatusCard
                      key={key}
                      label={key.replace(/_/g, " ")}
                      status={status as any}
                      detail={detail}
                    />
                  );
                })}
              </div>
            ) : (
              <div className="text-dim" style={{ padding: 12 }}>
                Click "Run Diagnostics" to check system health
              </div>
            )}
          </section>

          <section className="dash-section">
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 8 }}>
              <h2 className="section-title">Engine Logs</h2>
              <button className="btn-ghost btn-sm" onClick={fetchLogs}>
                {logLoading ? "..." : "\u21BB Refresh"}
              </button>
            </div>
            <div className="log-viewer">
              {logLines.length === 0 ? (
                <div className="text-dim">No logs available</div>
              ) : (
                logLines.map((line, i) => <div key={i} className="log-line">{line}</div>)
              )}
            </div>
          </section>
        </>
      )}
    </div>
  );
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + " " + sizes[i];
}
