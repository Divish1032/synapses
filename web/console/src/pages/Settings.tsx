import { useState, useEffect } from "preact/hooks";
import { get, api } from "../api";
import { useToast } from "../context/ToastContext";
import { useServices } from "../hooks/useServices";

interface AgentInfo {
  Key: string;
  Display: string;
  Detected: boolean;
}

const AGENTS = [
  { id: "claude", label: "Claude Code", configFile: ".mcp.json" },
  { id: "cursor", label: "Cursor", configFile: ".cursor/mcp.json" },
  { id: "windsurf", label: "Windsurf", configFile: ".windsurf/mcp_config.json" },
  { id: "zed", label: "Zed", configFile: ".zed/settings.json" },
  { id: "vscode", label: "VS Code", configFile: ".vscode/mcp.json" },
  { id: "antigravity", label: "Antigravity", configFile: ".agent/mcp.json" },
];

const MANUAL_SNIPPET = JSON.stringify(
  { mcpServers: { synapses: { type: "http", url: "http://127.0.0.1:11435/mcp" } } },
  null, 2
);

interface Project {
  path: string;
  hash: string;
  socket: string;
}

export function Settings() {
  const { addToast } = useToast();
  const { services } = useServices();
  const [copied, setCopied] = useState<string | null>(null);
  const [logLines, setLogLines] = useState<string[]>([]);
  const [logLoading, setLogLoading] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  const [projects, setProjects] = useState<Project[]>([]);
  const [detectedAgents, setDetectedAgents] = useState<AgentInfo[]>([]);
  const [connections, setConnections] = useState<Record<string, Record<string, boolean>>>({});
  const [connecting, setConnecting] = useState<string | null>(null);

  const daemonSvc = services.find((s) => s.name === "daemon");
  const daemonRunning = daemonSvc?.status === "healthy" || daemonSvc?.status === "degraded";

  useEffect(() => {
    get<Project[]>("/api/admin/projects").then(setProjects).catch(() => setProjects([]));
    get<AgentInfo[]>("/api/admin/agents/detect").then(setDetectedAgents).catch(() => []);
  }, []);

  // Check connections after projects load
  useEffect(() => {
    if (projects.length === 0) return;
    const checks = projects.flatMap((p) =>
      AGENTS.map((a) =>
        get<{ configured: boolean }>(`/api/admin/agents/check?editor=${a.id}&project_path=${encodeURIComponent(p.path)}`)
          .then((r) => ({ path: p.path, agent: a.id, connected: r.configured }))
          .catch(() => ({ path: p.path, agent: a.id, connected: false }))
      )
    );
    Promise.all(checks).then((results) => {
      const state: Record<string, Record<string, boolean>> = {};
      for (const { path, agent, connected } of results) {
        if (!state[path]) state[path] = {};
        state[path][agent] = connected;
      }
      setConnections(state);
    });
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
      const label = AGENTS.find((a) => a.id === agentId)?.label ?? agentId;
      addToast("success", `${label} connected - restart your editor to apply`);
    } catch (e: any) {
      addToast("error", `Failed: ${e.message}`);
    } finally { setConnecting(null); }
  }

  async function copyText(text: string, key: string) {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(key);
      setTimeout(() => setCopied(null), 2000);
    } catch { /* clipboard access denied in some contexts */ }
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

  // Determine agent state for each
  const detectedSet = new Set(detectedAgents.filter((a) => a.Detected).map((a) => a.Key));

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1 className="page-title">Settings</h1>
          <span className="page-subtitle">Configure editors, view logs, manage data</span>
        </div>
      </div>

      {/* Connect Your Editor */}
      <section className="dash-section">
        <h2 className="section-title">Connect Your Editor</h2>
        <div className="agent-grid">
          {AGENTS.map((a) => {
            const detected = detectedSet.has(a.id);
            // Check if connected to any project
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

        {/* Manual config snippet */}
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

      {/* Privacy & Data */}
      <section className="dash-section">
        <h2 className="section-title">Privacy & Data</h2>
        <div className="info-card">
          <p>All data stays on your machine. Synapses never phones home.</p>
          <p style={{ fontSize: 12, color: "var(--text-dim)" }}>
            Data directory: <code>~/.synapses/</code>
          </p>
        </div>
      </section>

      {/* Advanced */}
      <div className="advanced-section">
        <button className="advanced-toggle" onClick={() => { setAdvanced((v) => !v); if (!advanced) fetchLogs(); }}>
          {advanced ? "\u25BC" : "\u25B6"} Advanced
        </button>
        {advanced && (
          <div className="advanced-content">
            <div className="adv-grid">
              <div className="adv-card">
                <div className="adv-card-value">{daemonRunning ? "Running" : "Offline"}</div>
                <div className="adv-card-label">Engine</div>
              </div>
              <div className="adv-card">
                <div className="adv-card-value">{projects.length}</div>
                <div className="adv-card-label">Projects</div>
              </div>
            </div>

            <div style={{ marginTop: 16 }}>
              <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 8 }}>
                <span className="section-title">Engine Logs</span>
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
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
