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

export interface PulseAgentStats {
  agent_id: string;
  sessions: number;
  tool_calls: number;
  tokens_saved: number;
}

export interface BrainCostTier {
  tier: string;
  model: string;
  tokens: number;
  calls: number;
}

export interface PulseDashboard {
  summary: PulseSummary;
  timeline: PulseTimelinePoint[];
  tools: PulseTool[];
  agents?: PulseAgentStats[];
  brain_costs?: BrainCostTier[];
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

export interface BrainHealthExtended {
  status: string;
  model: string;
  available: boolean;
  version?: string;
  enrichment_rate?: number;
  patterns_learned?: number;
  summaries_count?: number;
}

export interface PatternHint {
  trigger: string;
  co_change: string;
  reason?: string;
  confidence: number;
}

export interface ADR {
  id: string;
  title: string;
  status: 'proposed' | 'accepted' | 'deprecated';
  decision: string;
  context?: string;
  linked_files?: string[];
  created_at: string;
}

// ---------------------------------------------------------------------------
// Graph / Project
// ---------------------------------------------------------------------------

export interface GraphSummary {
  files: number;
  packages: number;
  functions: number;
  methods: number;
  structs: number;
  interfaces: number;
  edges: number;
}

export interface EntityInfo {
  id: string;
  name: string;
  type: string;
  file: string;
  line: number;
  fanin: number;
  fanout: number;
}

export interface Violation {
  rule_id: string;
  rule_name: string;
  severity: 'error' | 'warning' | 'info';
  from: string;
  to: string;
  message: string;
}

export interface SuggestedRule {
  id: string;
  description: string;
  confidence: number;
}

export interface ProjectIdentity {
  repo_id: string;
  summary: GraphSummary;
  key_entities: EntityInfo[];
  scale: 'micro' | 'small' | 'medium' | 'large';
  suggested_rules: SuggestedRule[];
  languages?: string[];
}

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
// Sidebar tab
// ---------------------------------------------------------------------------

export type SidebarTab = 'home' | 'intelligence' | 'analytics' | 'explorer';

// ---------------------------------------------------------------------------
// Sidebar state (passed to HTML builder)
// ---------------------------------------------------------------------------

export interface SidebarState {
  activeTab: SidebarTab;
  health: HealthState;

  // Home tab
  projectIdentity?: ProjectIdentity;

  // Intelligence tab
  ollamaStatus: OllamaStatus;
  ollamaModels: OllamaModel[];
  defaultModel: string;
  modelPullProgress?: { model: string; pct: number; status: string };
  brainHealth?: BrainHealthExtended;
  patterns?: PatternHint[];
  adrs?: ADR[];
  sdlc?: SDLCConfig;

  // Analytics tab
  pulse?: PulseSummary;
  pulseTrend?: PulseTimelinePoint[];
  pulseAgents?: PulseAgentStats[];
  brainCosts?: BrainCostTier[];
  analyticsDateRange: number;

  // Explorer tab
  keyEntities?: EntityInfo[];
  violations?: Violation[];
  suggestedRules?: SuggestedRule[];
  graphSummary?: GraphSummary;

  // Context (legacy)
  contextPacket?: ContextPacket;
  graphStats?: string;

  // UI state
  collapsedSections: Record<string, boolean>;
}
