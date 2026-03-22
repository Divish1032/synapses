import { useState, useEffect, useCallback, useMemo } from "preact/hooks";
import { get } from "../api";

interface Summary {
  total_tool_calls: number;
  tokens_saved: number;
  cost_saved_usd: number;
  avg_latency_ms: number;
  cache_hit_rate: number;
  compression_ratio: number;
  context_deliveries: number;
  sessions: number;
  tasks_completed: number;
}

interface ToolStats { name: string; calls: number; avg_ms: number; error_rate: number; }
interface AgentStats { agent_id: string; sessions: number; tool_calls: number; tokens_saved: number; tasks_completed: number; last_seen?: string; }
interface TimelinePoint { date: string; tokens_saved: number; tool_calls: number; cost_saved_usd: number; }
interface PulseDashboard { days: number; summary?: Summary; tools?: ToolStats[]; agents?: AgentStats[]; timeline?: TimelinePoint[]; }
type Days = 7 | 30 | 90;

function fmtNum(n?: number): string {
  if (n == null || n === 0) return "0";
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "K";
  return n.toLocaleString();
}

function fmtMoney(n?: number): string {
  if (n == null || n === 0) return "-";
  if (n < 0.01) return "<$0.01";
  return `$${n.toFixed(2)}`;
}

function agentLabel(id: string): string {
  const l = id.toLowerCase();
  if (l.includes("claude")) return "Claude Code";
  if (l.includes("cursor")) return "Cursor";
  if (l.includes("windsurf")) return "Windsurf";
  if (l.includes("zed")) return "Zed";
  return id.length > 28 ? id.slice(0, 26) + "..." : id;
}

function relTime(isoStr?: string): string {
  if (!isoStr) return "";
  try {
    const ms = Date.now() - new Date(isoStr).getTime();
    const m = Math.floor(ms / 60_000);
    if (m < 1) return "just now";
    if (m < 60) return `${m}m ago`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h ago`;
    return `${Math.floor(h / 24)}d ago`;
  } catch { return ""; }
}

function buildDateRange(days: number): string[] {
  const out: string[] = [];
  const today = new Date();
  today.setUTCHours(0, 0, 0, 0);
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(today);
    d.setUTCDate(d.getUTCDate() - i);
    out.push(d.toISOString().slice(0, 10));
  }
  return out;
}

function fillTimeline(points: TimelinePoint[], days: number): TimelinePoint[] {
  const byDate = new Map(points.map((p) => [p.date, p]));
  return buildDateRange(days).map((date) =>
    byDate.get(date) ?? { date, tokens_saved: 0, tool_calls: 0, cost_saved_usd: 0 }
  );
}

function Sparkline({ points, color }: { points: number[]; color: string }) {
  if (points.length < 2) return null;
  const w = 400, h = 56;
  const max = Math.max(...points, 1);
  const step = w / (points.length - 1);
  const pathD = points
    .map((v, i) => `${i === 0 ? "M" : "L"} ${(i * step).toFixed(1)} ${(h - (v / max) * (h - 6)).toFixed(1)}`)
    .join(" ");
  const areaD = `${pathD} L ${((points.length - 1) * step).toFixed(1)} ${h} L 0 ${h} Z`;

  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" style={{ width: "100%", height: "100%", display: "block" }}>
      <defs>
        <linearGradient id="spark-fill" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color={color} stop-opacity="0.2" />
          <stop offset="100%" stop-color={color} stop-opacity="0" />
        </linearGradient>
      </defs>
      <path d={areaD} fill="url(#spark-fill)" />
      <path d={pathD} fill="none" stroke={color} stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round" />
    </svg>
  );
}

export function Activity() {
  const [data, setData] = useState<PulseDashboard | null>(null);
  const [offline, setOffline] = useState(false);
  const [loading, setLoading] = useState(true);
  const [days, setDays] = useState<Days>(7);
  const [advanced, setAdvanced] = useState(false);

  const fetchAll = useCallback(async (d: Days) => {
    setLoading(true);
    try {
      const pulse = await get<PulseDashboard>(`/api/admin/pulse/summary?days=${d}`);
      setData(pulse);
      setOffline(false);
    } catch {
      setData(null);
      setOffline(true);
    } finally { setLoading(false); }
  }, []);

  useEffect(() => { fetchAll(days); }, [fetchAll, days]);

  const s = data?.summary;
  const agents = data?.agents ?? [];

  const filledTimeline = useMemo(
    () => data?.timeline
      ? fillTimeline(data.timeline, days)
      : buildDateRange(days).map((date) => ({ date, tokens_saved: 0, tool_calls: 0, cost_saved_usd: 0 })),
    [data?.timeline, days]
  );

  const sparkValues = filledTimeline.map((p) => p.tool_calls);
  const now = Date.now();
  const todayAgents = agents.filter((a) => a.last_seen && now - new Date(a.last_seen).getTime() < 24 * 3600_000);
  const weekAgents = agents.filter((a) => a.last_seen && now - new Date(a.last_seen).getTime() >= 24 * 3600_000 && now - new Date(a.last_seen).getTime() < 7 * 24 * 3600_000);
  const olderAgents = agents.filter((a) => !a.last_seen || now - new Date(a.last_seen).getTime() >= 7 * 24 * 3600_000);

  return (
    <div className="page">
      <div className="page-header">
        <div>
          <h1 className="page-title">Activity</h1>
          <span className="page-subtitle">What your agents have been doing</span>
        </div>
        <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
          <div className="day-selector">
            {([7, 30, 90] as Days[]).map((d) => (
              <button key={d} className={`day-btn ${days === d ? "day-btn-active" : ""}`} onClick={() => setDays(d)}>
                {d}d
              </button>
            ))}
          </div>
          <button className="btn-ghost" onClick={() => fetchAll(days)} title="Refresh">
            {loading ? "..." : "\u21BB"}
          </button>
        </div>
      </div>

      {offline && (
        <div className="offline-banner">
          <span>Engine is offline - connect Synapses to see activity</span>
        </div>
      )}

      {/* Sparkline */}
      <div className="sparkline-card">
        <div className="sparkline-wrap">
          <Sparkline points={sparkValues} color="var(--accent)" />
        </div>
        <div className="sparkline-labels">
          <span>{filledTimeline[0]?.date?.slice(5) ?? ""}</span>
          <span style={{ color: "var(--text-muted)" }}>Activity - last {days} days</span>
          <span>today</span>
        </div>
      </div>

      {/* Summary row */}
      <div className="activity-summary-row">
        <div className="activity-summary-item">
          <span className="activity-summary-value" style={{ color: "var(--success)" }}>{fmtMoney(s?.cost_saved_usd)}</span>
          <span className="activity-summary-label">saved</span>
        </div>
        <div className="activity-summary-sep" />
        <div className="activity-summary-item">
          <span className="activity-summary-value">{fmtNum(s?.tasks_completed)}</span>
          <span className="activity-summary-label">tasks</span>
        </div>
        <div className="activity-summary-sep" />
        <div className="activity-summary-item">
          <span className="activity-summary-value">{fmtNum(s?.total_tool_calls)}</span>
          <span className="activity-summary-label">tool calls</span>
        </div>
        <div className="activity-summary-sep" />
        <div className="activity-summary-item">
          <span className="activity-summary-value">{agents.length}</span>
          <span className="activity-summary-label">agents</span>
        </div>
      </div>

      {/* Agent activity feed */}
      {agents.length === 0 && !offline && !loading && (
        <div className="empty-state-large">
          <p>No activity yet</p>
          <p style={{ fontSize: 12, color: "var(--text-dim)" }}>
            Connect an AI assistant in Settings to start seeing activity here
          </p>
        </div>
      )}

      {todayAgents.length > 0 && <ActivitySection label="Today" agents={todayAgents} />}
      {weekAgents.length > 0 && <ActivitySection label="Earlier this week" agents={weekAgents} />}
      {olderAgents.length > 0 && olderAgents.some((a) => a.sessions > 0) && (
        <ActivitySection label="Older" agents={olderAgents.filter((a) => a.sessions > 0)} dimmed />
      )}

      {/* Advanced stats */}
      <div className="advanced-section">
        <button className="advanced-toggle" onClick={() => setAdvanced((v) => !v)}>
          {advanced ? "\u25BC" : "\u25B6"} Advanced Stats
        </button>
        {advanced && (
          <div className="advanced-content">
            <div className="adv-grid">
              <AdvCard label="Sessions" value={fmtNum(s?.sessions)} />
              <AdvCard label="Deliveries" value={fmtNum(s?.context_deliveries)} />
              <AdvCard label="Cache hit rate" value={s?.cache_hit_rate != null ? `${(s.cache_hit_rate * 100).toFixed(1)}%` : "-"} />
              <AdvCard label="Avg latency" value={s?.avg_latency_ms != null ? `${s.avg_latency_ms.toFixed(0)}ms` : "-"} />
              <AdvCard label="Tokens saved" value={fmtNum(s?.tokens_saved)} />
              <AdvCard label="Compression" value={s?.compression_ratio != null ? `${s.compression_ratio.toFixed(1)}x` : "-"} />
            </div>
            {(data?.tools ?? []).length > 0 && (
              <div style={{ marginTop: 20 }}>
                <div className="section-title" style={{ marginBottom: 8 }}>Tool breakdown</div>
                <div className="an-table">
                  <div className="an-table-head">
                    <div className="an-table-row">
                      <div className="an-table-cell" style={{ flex: "0 0 40%" }}>Tool</div>
                      <div className="an-table-cell" style={{ flex: "0 0 20%" }}>Calls</div>
                      <div className="an-table-cell" style={{ flex: "0 0 20%" }}>Avg ms</div>
                      <div className="an-table-cell" style={{ flex: "0 0 20%" }}>Error rate</div>
                    </div>
                  </div>
                  <div className="an-table-body">
                    {(data?.tools ?? []).slice(0, 15).map((t) => (
                      <div key={t.name} className="an-table-row">
                        <div className="an-table-cell" style={{ flex: "0 0 40%" }}>{t.name}</div>
                        <div className="an-table-cell" style={{ flex: "0 0 20%" }}>{t.calls.toLocaleString()}</div>
                        <div className="an-table-cell" style={{ flex: "0 0 20%" }}>{t.avg_ms > 0 ? t.avg_ms.toFixed(0) : "-"}</div>
                        <div className="an-table-cell" style={{ flex: "0 0 20%", color: t.error_rate > 0.05 ? "var(--danger)" : "var(--text-dim)" }}>
                          {t.error_rate > 0 ? `${(t.error_rate * 100).toFixed(1)}%` : "-"}
                        </div>
                      </div>
                    ))}
                  </div>
                </div>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

function ActivitySection({ label, agents, dimmed }: { label: string; agents: AgentStats[]; dimmed?: boolean }) {
  return (
    <div className="activity-section">
      <div className="activity-section-label">{label}</div>
      {agents.map((a) => (
        <div key={a.agent_id} className={`activity-card ${dimmed ? "activity-card-dimmed" : ""}`}>
          <div className="activity-card-header">
            <div className="activity-card-meta">
              <span className="activity-card-name">{agentLabel(a.agent_id)}</span>
              {a.last_seen && <span className="activity-card-time">{relTime(a.last_seen)}</span>}
            </div>
          </div>
          <div className="activity-card-stats">
            {a.sessions > 0 && <span className="activity-stat"><strong>{a.sessions}</strong> session{a.sessions !== 1 ? "s" : ""}</span>}
            {a.tool_calls > 0 && <span className="activity-stat"><strong>{fmtNum(a.tool_calls)}</strong> tool calls</span>}
            {a.tasks_completed > 0 && <span className="activity-stat activity-stat-success"><strong>{a.tasks_completed}</strong> tasks done</span>}
            {a.tokens_saved > 0 && <span className="activity-stat activity-stat-accent"><strong>{fmtNum(a.tokens_saved)}</strong> context saved</span>}
          </div>
        </div>
      ))}
    </div>
  );
}

function AdvCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="adv-card">
      <div className="adv-card-value">{value}</div>
      <div className="adv-card-label">{label}</div>
    </div>
  );
}
