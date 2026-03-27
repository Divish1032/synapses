import { useState, useEffect, useCallback } from "preact/hooks";
import { get, api } from "../api";
import { ProgressBar } from "../components/ProgressBar";
import { useToast } from "../context/ToastContext";

const isTauri = !!(window as any).__TAURI__;

async function openFolderPicker(): Promise<string | null> {
  if (!isTauri) return null;
  try {
    // Use Tauri's invoke API directly to avoid build-time import resolution.
    // The dialog plugin exposes `plugin:dialog|open` as an IPC command.
    const invoke = (window as any).__TAURI__.core?.invoke ?? (window as any).__TAURI__?.invoke;
    if (!invoke) return null;
    const result = await invoke("plugin:dialog|open", {
      options: { directory: true, multiple: false, title: "Select your project folder" },
    });
    return typeof result === "string" ? result : null;
  } catch {
    return null;
  }
}

interface AgentInfo { Key: string; Display: string; Detected: boolean; }
interface OllamaStatus { running: boolean; version?: string; models?: string[]; }

export function Onboarding({ onComplete }: { onComplete: () => void }) {
  const { addToast } = useToast();
  const [step, setStep] = useState(0);

  // Step 0: Add project
  const [projectPath, setProjectPath] = useState("");
  const [indexing, setIndexing] = useState(false);
  const [indexProgress, setIndexProgress] = useState<{ state: string; pct?: number } | null>(null);
  const [projectAdded, setProjectAdded] = useState(false);

  // Step 1: Connect editor
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const [connecting, setConnecting] = useState<string | null>(null);
  const [connected, setConnected] = useState<Set<string>>(new Set());

  // Step 2: Brain setup
  const [ollama, setOllama] = useState<OllamaStatus | null>(null);
  const [brainEnabled, setBrainEnabled] = useState(false);

  // Load agents for step 1
  useEffect(() => {
    if (step === 1) {
      get<AgentInfo[]>("/api/admin/agents/detect").then(setAgents).catch(() => []);
    }
  }, [step]);

  // Load ollama for step 2
  useEffect(() => {
    if (step === 2) {
      get<OllamaStatus>("/api/admin/ollama").then(setOllama).catch(() => setOllama({ running: false }));
    }
  }, [step]);

  // Poll indexing progress — timeout after 120s to avoid infinite polling
  useEffect(() => {
    if (!indexing) return;
    let elapsed = 0;
    const id = setInterval(async () => {
      elapsed += 1;
      if (elapsed > 150) { // 150 * 800ms = 2 min
        setIndexing(false);
        setProjectAdded(true); // Assume done if no progress reported
        return;
      }
      try {
        const health = await get<any>("/api/admin/health");
        const p = health?.indexing_progress;
        if (p) {
          setIndexProgress(p);
          if (p.state === "idle" || p.state === "done") {
            setIndexing(false);
            setProjectAdded(true);
          }
        } else if (elapsed > 5) {
          // No indexing_progress field after a few polls = indexing likely done
          setIndexing(false);
          setProjectAdded(true);
        }
      } catch { /* ignore */ }
    }, 800);
    return () => clearInterval(id);
  }, [indexing]);

  async function addProject() {
    if (!projectPath.trim()) return;
    setIndexing(true);
    setIndexProgress({ state: "starting" });
    try {
      await api("/api/admin/projects", {
        method: "POST",
        body: JSON.stringify({ path: projectPath.trim() }),
      });
    } catch (e: any) {
      addToast("error", e.message);
      setIndexing(false);
    }
  }

  async function connectAgent(agentId: string) {
    setConnecting(agentId);
    try {
      await api("/api/admin/agents/connect", {
        method: "POST",
        body: JSON.stringify({ agent: agentId, project_path: projectPath.trim() }),
      });
      setConnected((prev) => new Set([...prev, agentId]));
      addToast("success", `Connected! Restart your editor to apply.`);
    } catch (e: any) {
      addToast("error", e.message);
    } finally {
      setConnecting(null);
    }
  }

  async function finishOnboarding() {
    try {
      await api("/api/admin/config", {
        method: "PUT",
        body: JSON.stringify({ app_settings: { onboarding_done: true } }),
      });
    } catch { /* non-fatal */ }
    onComplete();
  }

  async function enableBrain() {
    setBrainEnabled(true);
    try {
      await api("/api/admin/config", {
        method: "PUT",
        body: JSON.stringify({
          brain: { enabled: true, ollama_url: "http://localhost:11434", model: "qwen3.5:2b" },
        }),
      });
      addToast("success", "Brain enabled with default model");
    } catch (e: any) {
      addToast("error", e.message);
      setBrainEnabled(false);
    }
  }

  const steps = ["Add Project", "Connect Editor", "Brain Setup"];

  return (
    <div className="page onboarding-page">
      <div className="onboarding-container">
        <div className="onboarding-header">
          <h1 className="onboarding-title">Welcome to Synapses</h1>
          <p className="onboarding-subtitle">Let's get your codebase connected in a few steps.</p>
        </div>

        {/* Step indicator */}
        <div className="onboarding-steps">
          {steps.map((s, i) => (
            <div key={i} className={`onboarding-step ${i === step ? "step-active" : i < step ? "step-done" : ""}`}>
              <div className="step-number">{i < step ? "\u2713" : i + 1}</div>
              <span className="step-label">{s}</span>
            </div>
          ))}
        </div>

        {/* Step 0: Add project */}
        {step === 0 && (
          <div className="onboarding-card">
            <h2 className="onboarding-card-title">Point Synapses at your codebase</h2>
            <p className="onboarding-card-desc">
              Enter the absolute path to your project. Synapses will parse the code,
              build a relational graph, and start serving context to your AI agents.
            </p>
            <div className="onboarding-input-row">
              <input
                type="text"
                className="cfg-input"
                placeholder="/Users/you/projects/my-app"
                value={projectPath}
                disabled={indexing || projectAdded}
                onInput={(e) => setProjectPath((e.target as HTMLInputElement).value)}
                onKeyDown={(e) => e.key === "Enter" && addProject()}
              />
              {!projectAdded && (
                <button
                  className="btn-ghost"
                  onClick={async () => {
                    const path = await openFolderPicker();
                    if (path) setProjectPath(path);
                  }}
                  disabled={indexing}
                  style={{ whiteSpace: "nowrap" }}
                >
                  Browse
                </button>
              )}
              {!projectAdded && (
                <button
                  className="btn-primary"
                  onClick={addProject}
                  disabled={indexing || !projectPath.trim()}
                >
                  {indexing ? "Indexing..." : "Add Project"}
                </button>
              )}
            </div>
            {indexing && indexProgress && (
              <div style={{ marginTop: 16 }}>
                <ProgressBar
                  value={indexProgress.pct ?? 30}
                  label={`Indexing: ${indexProgress.state}`}
                  color="var(--accent)"
                />
              </div>
            )}
            {projectAdded && (
              <div className="onboarding-success">
                Project indexed successfully!
              </div>
            )}
            <div className="onboarding-actions">
              <button className="btn-ghost" onClick={() => setStep(1)}>
                Skip
              </button>
              <button
                className="btn-primary"
                disabled={!projectAdded}
                onClick={() => setStep(1)}
              >
                Continue
              </button>
            </div>
          </div>
        )}

        {/* Step 1: Connect editor */}
        {step === 1 && (
          <div className="onboarding-card">
            <h2 className="onboarding-card-title">Connect your AI editor</h2>
            <p className="onboarding-card-desc">
              Synapses auto-detected these editors on your machine.
              Click "Connect" to write the MCP configuration, then restart the editor.
            </p>
            <div className="agent-grid">
              {agents.filter((a) => a.Detected).map((a) => (
                <div key={a.Key} className="agent-card">
                  <div className="agent-card-header">
                    <span className="agent-card-name">{a.Display}</span>
                    <span className={`agent-badge ${connected.has(a.Key) ? "badge-success" : "badge-info"}`}>
                      {connected.has(a.Key) ? "Connected" : "Installed"}
                    </span>
                  </div>
                  {!connected.has(a.Key) && (
                    <button
                      className="btn-primary btn-sm"
                      disabled={connecting !== null}
                      onClick={() => connectAgent(a.Key)}
                    >
                      {connecting === a.Key ? "Connecting..." : "Connect"}
                    </button>
                  )}
                </div>
              ))}
              {agents.filter((a) => a.Detected).length === 0 && (
                <div className="text-dim" style={{ padding: 16 }}>
                  No supported editors detected. You can connect manually in Settings later.
                </div>
              )}
            </div>
            <div className="onboarding-actions">
              <button className="btn-ghost" onClick={() => setStep(0)}>Back</button>
              <button className="btn-primary" onClick={() => setStep(2)}>
                {connected.size > 0 ? "Continue" : "Skip"}
              </button>
            </div>
          </div>
        )}

        {/* Step 2: Brain setup */}
        {step === 2 && (
          <div className="onboarding-card">
            <h2 className="onboarding-card-title">Enable Brain (optional)</h2>
            <p className="onboarding-card-desc">
              Brain uses a local LLM via Ollama to add code summaries and intelligent enrichment.
              This is optional — Synapses works great without it.
            </p>
            {ollama && (
              <div className="onboarding-brain-status">
                <div className="status-dot-inline" style={{ background: ollama.running ? "var(--success)" : "var(--text-dim)" }} />
                <span>
                  Ollama: {ollama.running ? `Running (v${ollama.version})` : "Not detected"}
                </span>
              </div>
            )}
            {ollama?.running && !brainEnabled && (
              <button className="btn-primary" onClick={enableBrain} style={{ marginTop: 12 }}>
                Enable Brain
              </button>
            )}
            {brainEnabled && (
              <div className="onboarding-success">
                Brain enabled! You can fine-tune settings on the Brain page later.
              </div>
            )}
            {!ollama?.running && (
              <div className="info-card" style={{ marginTop: 12 }}>
                <p>Install Ollama from <strong>ollama.com</strong> to enable Brain features.</p>
                <p className="text-dim" style={{ fontSize: 12 }}>You can set this up anytime from the Brain page.</p>
              </div>
            )}
            <div className="onboarding-actions">
              <button className="btn-ghost" onClick={() => setStep(1)}>Back</button>
              <button className="btn-primary" onClick={finishOnboarding}>
                {brainEnabled ? "Finish Setup" : "Skip & Finish"}
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
