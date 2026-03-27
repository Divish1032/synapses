import { useState, useEffect, useCallback } from "preact/hooks";
import { get, api, streamSSE } from "../api";
import { useToast } from "../context/ToastContext";
import { Toggle, Select, ConfigSection } from "../components/ConfigEditor";
import { ProgressBar } from "../components/ProgressBar";
import { StatusCard } from "../components/StatusCard";

interface OllamaStatus { running: boolean; version?: string; models?: string[]; }

interface BrainConfig {
  enabled?: boolean;
  ollama_url?: string;
  model?: string;
  fast_model?: string;
  intelligence_mode?: string;
  ingest?: boolean;
  enrich?: boolean;
  context_builder?: boolean;
}

interface BrainHealthData {
  status?: string;
  summaries_count?: number;
  cache_hit_rate?: number;
  last_ingest?: string;
  queue_size?: number;
}

export function Brain() {
  const { addToast } = useToast();
  const [ollama, setOllama] = useState<OllamaStatus | null>(null);
  const [config, setConfig] = useState<BrainConfig>({});
  const [health, setHealth] = useState<BrainHealthData | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [pullModel, setPullModel] = useState("");
  const [pulling, setPulling] = useState(false);
  const [pullProgress, setPullProgress] = useState<{ pct: number; status: string } | null>(null);
  const [projects, setProjects] = useState<string[]>([]);

  const fetchAll = useCallback(async () => {
    setLoading(true);
    try {
      const [ollamaRes, configRes, projRes] = await Promise.allSettled([
        get<OllamaStatus>("/api/admin/ollama"),
        get<{ brain?: BrainConfig }>("/api/admin/config"),
        get<{ projects: Array<{ path: string }> }>("/api/admin/projects"),
      ]);
      if (ollamaRes.status === "fulfilled") setOllama(ollamaRes.value);
      if (configRes.status === "fulfilled") setConfig(configRes.value.brain ?? {});
      if (projRes.status === "fulfilled") {
        const d = projRes.value;
        const projList = Array.isArray(d) ? d : ((d as any)?.projects ?? []);
        const paths = projList.map((p: any) => p.path);
        setProjects(paths);
        // Brain health comes from pulse analytics, not a standalone tool
        if (paths.length > 0) {
          try {
            const pulse = await get<any>("/api/admin/pulse/brain");
            if (pulse) setHealth(pulse);
          } catch { /* pulse may not be available */ }
        }
      }
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetchAll(); }, [fetchAll]);

  async function saveConfig(updates: Partial<BrainConfig>) {
    const merged = { ...config, ...updates };
    setSaving(true);
    try {
      await api("/api/admin/config", {
        method: "PUT",
        body: JSON.stringify({ brain: merged }),
      });
      setConfig(merged);
      addToast("success", "Brain config saved");
    } catch (e: any) {
      addToast("error", e.message);
    } finally {
      setSaving(false);
    }
  }

  function handlePullModel() {
    if (!pullModel.trim()) return;
    setPulling(true);
    setPullProgress({ pct: 0, status: "Starting..." });
    const modelName = pullModel.trim();
    streamSSE(
      "/api/admin/ollama/pull",
      { model: modelName },
      (data) => {
        if (data.total && data.completed) {
          setPullProgress({
            pct: Math.round((data.completed / data.total) * 100),
            status: data.status ?? "Downloading...",
          });
        } else {
          setPullProgress((prev) => ({
            pct: prev?.pct ?? 0,
            status: data.status ?? "Pulling...",
          }));
        }
      },
      () => {
        setPulling(false);
        setPullProgress(null);
        setPullModel("");
        addToast("success", `Model ${modelName} pulled successfully`);
        fetchAll();
      },
      (err) => {
        addToast("error", err.message);
        setPulling(false);
        setPullProgress(null);
      },
    );
  }

  if (loading) {
    return (
      <div className="page">
        <div className="page-header">
          <h1 className="page-title">Brain</h1>
        </div>
        <div className="text-dim" style={{ padding: 32 }}>Loading...</div>
      </div>
    );
  }

  const intelligenceModes = [
    { value: "optimal", label: "Optimal (8GB RAM)" },
    { value: "standard", label: "Standard (16GB RAM)" },
    { value: "full", label: "Full (32GB+ RAM)" },
  ];

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1 className="page-title">Brain</h1>
          <span className="page-subtitle">LLM-powered code intelligence via Ollama</span>
        </div>
      </div>

      {/* Ollama Connection */}
      <section className="dash-section">
        <h2 className="section-title">Ollama Connection</h2>
        <div className="brain-connection-card">
          <div className="brain-connection-row">
            <div className="brain-connection-status">
              <div
                className={`status-dot-lg ${ollama?.running ? "pulse" : ""}`}
                style={{ background: ollama?.running ? "var(--success)" : "var(--danger)" }}
              />
              <div>
                <div className="brain-connection-label">
                  {ollama?.running ? "Connected" : "Not Connected"}
                </div>
                {ollama?.version && (
                  <div className="text-dim" style={{ fontSize: 12 }}>Version {ollama.version}</div>
                )}
              </div>
            </div>
          </div>
          {!ollama?.running && (
            <div className="info-card" style={{ marginTop: 12 }}>
              <p>Ollama is not running. Install it from <strong>ollama.com</strong> and start it.</p>
              <button className="btn-ghost btn-sm" style={{ marginTop: 8 }} onClick={fetchAll}>
                Check Again
              </button>
            </div>
          )}
        </div>
      </section>

      {/* Feature Toggles */}
      <section className="dash-section">
        <ConfigSection title="Features" description="Toggle Brain capabilities on or off">
          <Toggle
            label="Brain Enabled"
            description="Master switch for all LLM features"
            checked={config.enabled ?? false}
            onChange={(v) => saveConfig({ enabled: v })}
            disabled={saving}
          />
          <Toggle
            label="Auto-Ingest"
            description="Summarize code on file save for faster context delivery"
            checked={config.ingest ?? false}
            onChange={(v) => saveConfig({ ingest: v })}
            disabled={saving || !config.enabled}
          />
          <Toggle
            label="Context Enrichment"
            description="Add LLM-generated explanations to get_context responses"
            checked={config.enrich ?? false}
            onChange={(v) => saveConfig({ enrich: v })}
            disabled={saving || !config.enabled}
          />
          <Toggle
            label="Context Builder"
            description="LLM-assembled context packets for complex queries"
            checked={config.context_builder ?? false}
            onChange={(v) => saveConfig({ context_builder: v })}
            disabled={saving || !config.enabled}
          />
        </ConfigSection>
      </section>

      {/* Intelligence Mode */}
      <section className="dash-section">
        <ConfigSection title="Intelligence Mode" description="Choose based on your available RAM">
          <Select
            label="Mode"
            description="Higher modes use larger models for better results"
            value={config.intelligence_mode ?? "optimal"}
            options={intelligenceModes}
            onChange={(v) => saveConfig({ intelligence_mode: v })}
            disabled={saving}
          />
        </ConfigSection>
      </section>

      {/* Model Management */}
      {ollama?.running && (
        <section className="dash-section">
          <h2 className="section-title">Models</h2>
          {ollama.models && ollama.models.length > 0 ? (
            <div className="model-list">
              {ollama.models.map((m) => (
                <div key={m} className="model-card">
                  <span className="model-name">{m}</span>
                  {m === (config.model ?? "qwen3.5:2b") && (
                    <span className="agent-badge badge-success">Active</span>
                  )}
                </div>
              ))}
            </div>
          ) : (
            <div className="text-dim" style={{ padding: 12 }}>No models installed</div>
          )}

          {/* Pull model */}
          <div className="brain-pull-section">
            <div className="onboarding-input-row" style={{ marginTop: 12 }}>
              <input
                type="text"
                className="cfg-input"
                placeholder="e.g. qwen3.5:2b"
                value={pullModel}
                disabled={pulling}
                onInput={(e) => setPullModel((e.target as HTMLInputElement).value)}
                onKeyDown={(e) => e.key === "Enter" && handlePullModel()}
              />
              <button
                className="btn-primary"
                onClick={handlePullModel}
                disabled={pulling || !pullModel.trim()}
              >
                {pulling ? "Pulling..." : "Pull Model"}
              </button>
            </div>
            {pullProgress && (
              <div style={{ marginTop: 12 }}>
                <ProgressBar value={pullProgress.pct} label={pullProgress.status} />
              </div>
            )}
          </div>
        </section>
      )}

      {/* Brain Health */}
      {health && (
        <section className="dash-section">
          <h2 className="section-title">Brain Health</h2>
          <div className="health-grid">
            <StatusCard
              label="Status"
              status={health.status === "healthy" ? "healthy" : health.status === "degraded" ? "warning" : "unknown"}
              icon="\u2699"
            />
            {health.summaries_count != null && (
              <div className="adv-card">
                <div className="adv-card-value">{health.summaries_count}</div>
                <div className="adv-card-label">Summaries</div>
              </div>
            )}
            {health.cache_hit_rate != null && (
              <div className="adv-card">
                <div className="adv-card-value">{Math.round(health.cache_hit_rate * 100)}%</div>
                <div className="adv-card-label">Cache Hit Rate</div>
              </div>
            )}
            {health.queue_size != null && (
              <div className="adv-card">
                <div className="adv-card-value">{health.queue_size}</div>
                <div className="adv-card-label">Queue Size</div>
              </div>
            )}
          </div>
        </section>
      )}
    </div>
  );
}
