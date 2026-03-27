import { useState, useEffect, useCallback } from "preact/hooks";
import { get, api, callTool } from "../api";
import { useToast } from "../context/ToastContext";
import { TabBar } from "../components/TabBar";
import { StatusCard } from "../components/StatusCard";
import { Toggle, Slider, Select, NumberInput, ConfigSection } from "../components/ConfigEditor";

interface ProjectIdentity {
  name?: string;
  scale?: string;
  languages?: string[];
  node_count?: number;
  edge_count?: number;
  function_count?: number;
  package_count?: number;
  file_count?: number;
  entry_points?: string[];
  highest_connectivity?: Array<{ name: string; callers: number }>;
  active_rules?: number;
  mode?: string;
}

interface Rule {
  id: string;
  description: string;
  severity?: string;
  rule_type?: string;
  forbidden_edge?: { from?: string; to?: string; edge_type?: string };
}

interface Violation {
  rule_id: string;
  description: string;
  entity?: string;
  file?: string;
  severity?: string;
}

interface Task {
  id: string;
  title?: string;
  description?: string;
  status: string;
  created_at?: string;
  agent_id?: string;
}

interface Episode {
  id: string;
  episode_type: string;
  summary?: string;
  outcome?: string;
  created_at?: string;
  agent_id?: string;
}

interface FederationStatus {
  enabled?: boolean;
  linked_projects?: Array<{ alias: string; path: string; healthy: boolean }>;
  acl?: { allow_read_from?: string[] };
}

interface ProjectConfig {
  version?: string;
  mode?: string;
  context_carve?: {
    default_depth?: number;
    token_budget?: number;
    exclude_test_files?: boolean;
    decay_factor?: number;
    min_relevance?: number;
  };
  use_go_types?: boolean;
  use_ts_types?: boolean;
  metrics_days?: number;
  embeddings?: string;
  embedding_endpoint?: string;
  recall?: {
    fusion_mode?: string;
    convex_alpha?: number;
  };
  rate_limits?: {
    write_ops_per_minute?: number;
    expensive_reads_per_minute?: number;
    cross_project_per_minute?: number;
  };
  content_safety?: {
    enabled?: boolean;
    mode?: string;
  };
  brain?: { enabled?: boolean };
  [key: string]: any;
}

const TABS = [
  { id: "overview", label: "Overview", icon: "\u2302" },
  { id: "config", label: "Config", icon: "\u2699" },
  { id: "rules", label: "Rules", icon: "\u26A0" },
  { id: "memory", label: "Memory", icon: "\u2691" },
  { id: "federation", label: "Federation", icon: "\u2194" },
];

export function ProjectDetail({ projectPath, onBack }: { projectPath: string; onBack: () => void }) {
  const { addToast } = useToast();
  const [tab, setTab] = useState("overview");
  const [loading, setLoading] = useState(true);

  // Overview
  const [identity, setIdentity] = useState<ProjectIdentity | null>(null);

  // Config
  const [config, setConfig] = useState<ProjectConfig>({});
  const [configDirty, setConfigDirty] = useState(false);
  const [configSaving, setConfigSaving] = useState(false);

  // Rules
  const [rules, setRules] = useState<Rule[]>([]);
  const [violations, setViolations] = useState<Violation[]>([]);

  // Memory
  const [tasks, setTasks] = useState<Task[]>([]);
  const [episodes, setEpisodes] = useState<Episode[]>([]);

  // Federation
  const [federation, setFederation] = useState<FederationStatus | null>(null);

  const projectName = projectPath.split("/").pop() ?? projectPath;

  const fetchOverview = useCallback(async () => {
    setLoading(true);
    try {
      const res = await callTool<any>("get_project_identity", projectPath);
      // Response is { identity: {...}, federation: {...}, ... }
      const id = res?.identity ?? res;
      setIdentity(id);
    } catch { /* may fail if project not ready */ }
    setLoading(false);
  }, [projectPath]);

  const fetchConfig = useCallback(async () => {
    try {
      const res = await get<{ config: ProjectConfig }>(`/api/admin/projects/config?path=${encodeURIComponent(projectPath)}`);
      setConfig(res.config ?? {});
      setConfigDirty(false);
    } catch {
      setConfig({});
    }
  }, [projectPath]);

  const fetchRules = useCallback(async () => {
    try {
      const [r, v] = await Promise.allSettled([
        callTool<{ rules: Rule[] }>("get_rules", projectPath),
        callTool<{ violations: Violation[] }>("get_violations", projectPath),
      ]);
      if (r.status === "fulfilled") setRules(r.value.rules ?? []);
      if (v.status === "fulfilled") setViolations(v.value.violations ?? []);
    } catch { /* ignore */ }
  }, [projectPath]);

  const fetchMemory = useCallback(async () => {
    try {
      const [t, e] = await Promise.allSettled([
        callTool<any>("get_pending_tasks", projectPath),
        callTool<any>("recall", projectPath),
      ]);
      if (t.status === "fulfilled") {
        const taskList = t.value?.tasks ?? [];
        setTasks(Array.isArray(taskList) ? taskList : []);
      }
      if (e.status === "fulfilled") {
        const epList = e.value?.episodes ?? e.value?.results ?? [];
        setEpisodes(Array.isArray(epList) ? epList : []);
      }
    } catch { /* ignore */ }
  }, [projectPath]);

  const fetchFederation = useCallback(async () => {
    try {
      // Federation is part of get_project_identity response
      const res = await callTool<any>("get_project_identity", projectPath);
      const fed = res?.federation;
      if (fed) {
        setFederation({
          enabled: fed.is_federated ?? false,
          linked_projects: (fed.linked_repos ?? []).map((r: any) => ({
            alias: typeof r === "string" ? r : r.alias ?? r.repo_id ?? "unknown",
            path: typeof r === "string" ? r : r.path ?? "",
            healthy: true,
          })),
        });
      } else {
        setFederation(null);
      }
    } catch {
      setFederation(null);
    }
  }, [projectPath]);

  useEffect(() => { fetchOverview(); }, [fetchOverview]);

  useEffect(() => {
    if (tab === "config") fetchConfig();
    else if (tab === "rules") fetchRules();
    else if (tab === "memory") fetchMemory();
    else if (tab === "federation") fetchFederation();
  }, [tab, fetchConfig, fetchRules, fetchMemory, fetchFederation]);

  function updateConfig(path: string, value: any) {
    setConfig((prev) => {
      const next = { ...prev };
      const parts = path.split(".");
      let obj: any = next;
      for (let i = 0; i < parts.length - 1; i++) {
        if (!obj[parts[i]]) obj[parts[i]] = {};
        else obj[parts[i]] = { ...obj[parts[i]] };
        obj = obj[parts[i]];
      }
      obj[parts[parts.length - 1]] = value;
      return next;
    });
    setConfigDirty(true);
  }

  async function saveConfig() {
    setConfigSaving(true);
    try {
      await api(`/api/admin/projects/config`, {
        method: "PUT",
        body: JSON.stringify({ path: projectPath, config }),
      });
      setConfigDirty(false);
      addToast("success", "Config saved");
    } catch (e: any) {
      addToast("error", e.message);
    } finally {
      setConfigSaving(false);
    }
  }

  async function handleReindex() {
    try {
      await api("/api/admin/projects/reindex", {
        method: "POST",
        body: JSON.stringify({ path: projectPath }),
      });
      addToast("success", "Reindex started");
    } catch (e: any) {
      addToast("error", e.message);
    }
  }

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <button className="btn-ghost" onClick={onBack} style={{ marginBottom: 4 }}>
            &larr; Back to Projects
          </button>
          <h1 className="page-title">{projectName}</h1>
          <span className="page-subtitle text-dim" style={{ fontSize: 12 }}>{projectPath}</span>
        </div>
      </div>

      <TabBar tabs={TABS} active={tab} onChange={setTab} />

      {/* Overview Tab */}
      {tab === "overview" && (
        <div className="project-detail-content">
          {loading ? (
            <div className="text-dim" style={{ padding: 24 }}>Loading project identity...</div>
          ) : identity ? (
            <>
              <div className="project-stats-grid">
                <div className="adv-card">
                  <div className="adv-card-value">{identity.scale ?? "unknown"}</div>
                  <div className="adv-card-label">Scale</div>
                </div>
                <div className="adv-card">
                  <div className="adv-card-value">{identity.file_count ?? "-"}</div>
                  <div className="adv-card-label">Files</div>
                </div>
                <div className="adv-card">
                  <div className="adv-card-value">{identity.function_count ?? "-"}</div>
                  <div className="adv-card-label">Functions</div>
                </div>
                <div className="adv-card">
                  <div className="adv-card-value">{identity.package_count ?? "-"}</div>
                  <div className="adv-card-label">Packages</div>
                </div>
                <div className="adv-card">
                  <div className="adv-card-value">{identity.node_count ?? "-"}</div>
                  <div className="adv-card-label">Nodes</div>
                </div>
                <div className="adv-card">
                  <div className="adv-card-value">{identity.edge_count ?? "-"}</div>
                  <div className="adv-card-label">Edges</div>
                </div>
              </div>

              {identity.languages && identity.languages.length > 0 && (
                <div className="project-detail-section">
                  <h3 className="section-title" style={{ fontSize: 13 }}>Languages</h3>
                  <div className="tag-list">
                    {identity.languages.map((l) => (
                      <span key={l} className="tag">{l}</span>
                    ))}
                  </div>
                </div>
              )}

              {identity.entry_points && identity.entry_points.length > 0 && (
                <div className="project-detail-section">
                  <h3 className="section-title" style={{ fontSize: 13 }}>Entry Points</h3>
                  <div className="entity-list">
                    {identity.entry_points.slice(0, 10).map((ep) => (
                      <div key={ep} className="entity-item">{ep}</div>
                    ))}
                  </div>
                </div>
              )}

              {identity.highest_connectivity && identity.highest_connectivity.length > 0 && (
                <div className="project-detail-section">
                  <h3 className="section-title" style={{ fontSize: 13 }}>Highest Connectivity</h3>
                  <div className="entity-list">
                    {identity.highest_connectivity.slice(0, 8).map((e) => (
                      <div key={e.name} className="entity-item">
                        <span>{e.name}</span>
                        <span className="text-dim">{e.callers} callers</span>
                      </div>
                    ))}
                  </div>
                </div>
              )}

              <div className="project-detail-actions">
                <button className="btn-primary btn-sm" onClick={handleReindex}>Reindex</button>
              </div>
            </>
          ) : (
            <div className="text-dim" style={{ padding: 24 }}>
              Could not load project identity. The project may still be indexing.
            </div>
          )}
        </div>
      )}

      {/* Config Tab */}
      {tab === "config" && (
        <div className="project-detail-content">
          <ConfigSection title="Context Delivery" description="How context is carved and served to agents">
            <Slider
              label="Default Depth"
              description="BFS traversal depth for ego-graph slices"
              value={config.context_carve?.default_depth ?? 2}
              min={1} max={5}
              onChange={(v) => updateConfig("context_carve.default_depth", v)}
            />
            <Slider
              label="Token Budget"
              description="Maximum tokens per context response"
              value={config.context_carve?.token_budget ?? 4000}
              min={1000} max={16000} step={500}
              onChange={(v) => updateConfig("context_carve.token_budget", v)}
              unit=" tokens"
            />
            <Toggle
              label="Exclude Test Files"
              description="Skip test files when carving context"
              checked={config.context_carve?.exclude_test_files ?? false}
              onChange={(v) => updateConfig("context_carve.exclude_test_files", v)}
            />
          </ConfigSection>

          <ConfigSection title="Code Intelligence" description="Type-checked resolution and metrics">
            <Toggle
              label="Go Type Resolution"
              description="Use go/types for accurate CALLS edges (requires go.mod)"
              checked={config.use_go_types ?? false}
              onChange={(v) => updateConfig("use_go_types", v)}
            />
            <Toggle
              label="TypeScript Type Resolution"
              description="Use TypeScript compiler for type-aware resolution"
              checked={config.use_ts_types ?? false}
              onChange={(v) => updateConfig("use_ts_types", v)}
            />
            <NumberInput
              label="Metrics Window"
              description="Git churn analysis window in days"
              value={config.metrics_days ?? 90}
              min={7} max={365}
              unit="days"
              onChange={(v) => updateConfig("metrics_days", v)}
            />
          </ConfigSection>

          <ConfigSection title="Embeddings" description="Semantic search vector configuration">
            <Select
              label="Mode"
              description="How embeddings are generated for semantic search"
              value={config.embeddings ?? "builtin"}
              options={[
                { value: "builtin", label: "Built-in (ONNX, ~137MB, no deps)" },
                { value: "ollama", label: "Ollama (local LLM)" },
                { value: "off", label: "Off (disable semantic search)" },
              ]}
              onChange={(v) => updateConfig("embeddings", v)}
            />
            {config.embeddings === "ollama" && (
              <div className="cfg-field">
                <div className="cfg-field-text">
                  <span className="cfg-field-label">Endpoint</span>
                  <span className="cfg-field-desc">OpenAI-compatible embedding endpoint</span>
                </div>
                <input
                  type="text"
                  className="cfg-input"
                  value={config.embedding_endpoint ?? ""}
                  placeholder="http://localhost:11434/api/embed"
                  onInput={(e) => updateConfig("embedding_endpoint", (e.target as HTMLInputElement).value)}
                />
              </div>
            )}
          </ConfigSection>

          <ConfigSection title="Recall Pipeline" description="How search results are ranked and fused">
            <Select
              label="Fusion Mode"
              description="How BM25 and semantic results are combined"
              value={config.recall?.fusion_mode ?? "rrf"}
              options={[
                { value: "rrf", label: "RRF (Reciprocal Rank Fusion)" },
                { value: "convex", label: "Convex (score-aware blending)" },
              ]}
              onChange={(v) => updateConfig("recall.fusion_mode", v)}
            />
            {(config.recall?.fusion_mode ?? "rrf") === "convex" && (
              <Slider
                label="Alpha"
                description="BM25 vs semantic balance (0 = all BM25, 1 = all semantic)"
                value={config.recall?.convex_alpha ?? 0.5}
                min={0} max={1} step={0.05}
                onChange={(v) => updateConfig("recall.convex_alpha", v)}
              />
            )}
          </ConfigSection>

          <ConfigSection title="Rate Limits" description="Per-session rate limits for agent operations">
            <NumberInput
              label="Write Operations"
              description="Max write ops per minute per session"
              value={config.rate_limits?.write_ops_per_minute ?? 30}
              min={5} max={200} unit="/min"
              onChange={(v) => updateConfig("rate_limits.write_ops_per_minute", v)}
            />
            <NumberInput
              label="Expensive Reads"
              description="Max recall/search ops per minute per session"
              value={config.rate_limits?.expensive_reads_per_minute ?? 20}
              min={5} max={100} unit="/min"
              onChange={(v) => updateConfig("rate_limits.expensive_reads_per_minute", v)}
            />
            <NumberInput
              label="Cross-Project"
              description="Max cross-project queries per minute"
              value={config.rate_limits?.cross_project_per_minute ?? 60}
              min={5} max={200} unit="/min"
              onChange={(v) => updateConfig("rate_limits.cross_project_per_minute", v)}
            />
          </ConfigSection>

          <ConfigSection title="Content Safety" description="Prompt injection scanning for agent inputs">
            <Toggle
              label="Enabled"
              description="Scan agent inputs for prompt injection attempts"
              checked={config.content_safety?.enabled ?? true}
              onChange={(v) => updateConfig("content_safety.enabled", v)}
            />
            <Select
              label="Mode"
              description="What to do when injection is detected"
              value={config.content_safety?.mode ?? "reject"}
              options={[
                { value: "reject", label: "Reject (block the request)" },
                { value: "truncate", label: "Truncate (strip suspicious content)" },
                { value: "warn", label: "Warn (flag but allow)" },
              ]}
              onChange={(v) => updateConfig("content_safety.mode", v)}
              disabled={!(config.content_safety?.enabled ?? true)}
            />
          </ConfigSection>

          {configDirty && (
            <div className="config-save-bar">
              <span className="text-dim">Unsaved changes</span>
              <button className="btn-primary" onClick={saveConfig} disabled={configSaving}>
                {configSaving ? "Saving..." : "Save Config"}
              </button>
            </div>
          )}
        </div>
      )}

      {/* Rules Tab */}
      {tab === "rules" && (
        <div className="project-detail-content">
          {violations.length > 0 && (
            <div className="rules-violations-section">
              <h3 className="section-title" style={{ color: "var(--warning)" }}>
                {violations.length} Violation{violations.length !== 1 ? "s" : ""}
              </h3>
              <div className="violations-list">
                {violations.map((v, i) => (
                  <div key={i} className="violation-card">
                    <div className="violation-header">
                      <span className="violation-rule">{v.rule_id}</span>
                      <span className={`agent-badge ${v.severity === "error" ? "badge-danger" : "badge-warning"}`}>
                        {v.severity ?? "warning"}
                      </span>
                    </div>
                    <div className="violation-desc">{v.description}</div>
                    {v.entity && <div className="text-dim" style={{ fontSize: 11 }}>{v.entity}</div>}
                    {v.file && <div className="text-dim" style={{ fontSize: 11 }}>{v.file}</div>}
                  </div>
                ))}
              </div>
            </div>
          )}

          <h3 className="section-title">Active Rules ({rules.length})</h3>
          {rules.length === 0 ? (
            <div className="info-card">
              <p>No architectural rules defined yet.</p>
              <p className="text-dim" style={{ fontSize: 12 }}>
                Add rules in <code>synapses.json</code> under the "rules" key to enforce code boundaries.
              </p>
            </div>
          ) : (
            <div className="rules-list">
              {rules.map((r) => (
                <div key={r.id} className="rule-card">
                  <div className="rule-header">
                    <span className="rule-id">{r.id}</span>
                    <span className={`agent-badge ${
                      r.severity === "error" ? "badge-danger" : r.severity === "warn" ? "badge-warning" : "badge-info"
                    }`}>
                      {r.severity ?? "info"}
                    </span>
                  </div>
                  <div className="rule-desc">{r.description}</div>
                  {r.forbidden_edge && (
                    <div className="rule-edge text-dim" style={{ fontSize: 11 }}>
                      {r.forbidden_edge.from ?? "*"} &rarr; {r.forbidden_edge.to ?? "*"} ({r.forbidden_edge.edge_type ?? "any"})
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Memory Tab */}
      {tab === "memory" && (
        <div className="project-detail-content">
          <h3 className="section-title">Pending Tasks ({tasks.length})</h3>
          {tasks.length === 0 ? (
            <div className="text-dim" style={{ padding: 12 }}>No pending tasks from agents</div>
          ) : (
            <div className="memory-list">
              {tasks.map((t) => (
                <div key={t.id} className="memory-card">
                  <div className="memory-card-header">
                    <span className="memory-card-title">{t.title ?? t.id}</span>
                    <span className={`agent-badge ${t.status === "done" ? "badge-success" : "badge-info"}`}>
                      {t.status}
                    </span>
                  </div>
                  {t.description && <div className="memory-card-body">{t.description}</div>}
                  <div className="memory-card-meta text-dim">
                    {t.agent_id && <span>{t.agent_id}</span>}
                    {t.created_at && <span>{new Date(t.created_at).toLocaleDateString()}</span>}
                  </div>
                </div>
              ))}
            </div>
          )}

          <h3 className="section-title" style={{ marginTop: 24 }}>Episodes ({episodes.length})</h3>
          {episodes.length === 0 ? (
            <div className="text-dim" style={{ padding: 12 }}>No episodes recorded yet</div>
          ) : (
            <div className="memory-list">
              {episodes.map((ep) => (
                <div key={ep.id} className="memory-card">
                  <div className="memory-card-header">
                    <span className="tag">{ep.episode_type}</span>
                    {ep.outcome && (
                      <span className={`agent-badge ${ep.outcome === "success" ? "badge-success" : "badge-warning"}`}>
                        {ep.outcome}
                      </span>
                    )}
                  </div>
                  {ep.summary && <div className="memory-card-body">{ep.summary}</div>}
                  <div className="memory-card-meta text-dim">
                    {ep.agent_id && <span>{ep.agent_id}</span>}
                    {ep.created_at && <span>{new Date(ep.created_at).toLocaleDateString()}</span>}
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Federation Tab */}
      {tab === "federation" && (
        <div className="project-detail-content">
          {federation === null ? (
            <div className="info-card">
              <p>Federation is not configured for this project.</p>
              <p className="text-dim" style={{ fontSize: 12 }}>
                Add a "federation" section in <code>synapses.json</code> to link cross-project graphs.
              </p>
            </div>
          ) : (
            <>
              <div className="federation-status">
                <StatusCard
                  label="Federation"
                  status={federation.enabled ? "healthy" : "unknown"}
                  detail={federation.enabled ? "Active" : "Disabled"}
                  icon="\u2194"
                />
              </div>
              {federation.linked_projects && federation.linked_projects.length > 0 && (
                <div className="project-detail-section">
                  <h3 className="section-title" style={{ fontSize: 13 }}>Linked Projects</h3>
                  <div className="federation-list">
                    {federation.linked_projects.map((lp) => (
                      <div key={lp.alias} className="federation-card">
                        <div className="federation-card-header">
                          <span className="federation-alias">{lp.alias}</span>
                          <div
                            className="status-dot-inline"
                            style={{ background: lp.healthy ? "var(--success)" : "var(--danger)" }}
                          />
                        </div>
                        <div className="text-dim" style={{ fontSize: 11 }}>{lp.path}</div>
                      </div>
                    ))}
                  </div>
                </div>
              )}
              {federation.acl?.allow_read_from && federation.acl.allow_read_from.length > 0 && (
                <div className="project-detail-section">
                  <h3 className="section-title" style={{ fontSize: 13 }}>Read ACL</h3>
                  <div className="tag-list">
                    {federation.acl.allow_read_from.map((a) => (
                      <span key={a} className="tag">{a}</span>
                    ))}
                  </div>
                </div>
              )}
            </>
          )}
        </div>
      )}
    </div>
  );
}
