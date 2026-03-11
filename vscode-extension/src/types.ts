// Shared types for the Synapses VS Code extension.

// ---------------------------------------------------------------------------
// Service health
// ---------------------------------------------------------------------------

export type ServiceId = 'core' | 'intelligence' | 'scout' | 'pulse';

export interface ServiceHealth {
  id: ServiceId;
  status: 'online' | 'offline' | 'degraded' | 'disabled';
  version?: string;
  latencyMs?: number;
}

export type HealthState = Record<ServiceId, ServiceHealth>;

// ---------------------------------------------------------------------------
// Pulse analytics
// ---------------------------------------------------------------------------

export interface PulseTimelinePoint {
  date: string;
  tokens_saved: number;
  tool_calls: number;
  cost_saved_usd: number;
}

export interface PulseTool {
  name: string;
  calls: number;
  avg_ms: number;
  error_rate: number;
  avg_tokens_delivered: number;
  avg_compression: number;
}

export interface PulseSummary {
  tokens_delivered: number;
  baseline_tokens: number;
  tokens_saved: number;
  savings_pct: number;
  compression_ratio: number;
  cost_saved_usd: number;
  tool_calls: number;
  avg_latency_ms: number;
  cache_hit_rate: number;
  sessions: number;
  tasks_completed: number;
  top_tools: PulseTool[];
  top_entities: string[];
}

export interface PulseDashboard {
  summary: PulseSummary;
  timeline: PulseTimelinePoint[];
  tools: PulseTool[];
}

// ---------------------------------------------------------------------------
// Ollama
// ---------------------------------------------------------------------------

export type OllamaStatus = 'running' | 'stopped' | 'not-installed';

export interface OllamaModel {
  name: string;
  size: number;
  modified: string;
}

// ---------------------------------------------------------------------------
// Brain / intelligence (existing types, moved here)
// ---------------------------------------------------------------------------

export interface BrainHealth { status: string; version?: string }

export interface IngestRequest {
  node_id: string;
  node_name: string;
  node_type: string;
  file: string;
  package?: string;
  signature?: string;
  doc?: string;
  callee_names?: string[];
  caller_names?: string[];
}

export interface IngestResponse {
  node_id: string;
  summary: string;
  tags: string[];
}

export interface ContextPacketRequest {
  snapshot: {
    root_node_id: string;
    root_name: string;
    root_type: string;
    root_file: string;
    callee_names?: string[];
    caller_names?: string[];
    applicable_rules?: string[];
    active_claims?: string[];
  };
  phase?: string;
  enable_llm?: boolean;
}

export interface ContextPacket {
  root_summary: string;
  insight: string;
  concerns: string[];
  packet_quality: number;
  llm_used: boolean;
  dependency_summaries: Record<string, string>;
}

export interface SDLCConfig {
  phase: string;
  quality_mode: string;
}

// ---------------------------------------------------------------------------
// Sidebar state (passed to HTML builder)
// ---------------------------------------------------------------------------

export interface SidebarState {
  health: HealthState;
  pulse?: PulseSummary;
  pulseTrend?: PulseTimelinePoint[];  // last 7 days for sparkline
  ollamaStatus: OllamaStatus;
  ollamaModels: OllamaModel[];
  defaultModel: string;
  contextPacket?: ContextPacket;
  sdlc?: SDLCConfig;
  graphStats?: string;
  modelPullProgress?: { model: string; pct: number; status: string };
}
