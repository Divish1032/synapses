import {
  SidebarState, SidebarTab, ServiceId, ServiceHealth,
  OllamaModel, PatternHint, ADR, EntityInfo, Violation,
  SuggestedRule, GraphSummary, BrainHealthExtended,
  PulseAgentStats, BrainCostTier,
} from '../types';

// ── Helpers ──────────────────────────────────────────────────────────────

function esc(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function fmtNum(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return String(n);
}

function fmtBytes(bytes: number): string {
  if (bytes >= 1_073_741_824) return `${(bytes / 1_073_741_824).toFixed(1)} GB`;
  if (bytes >= 1_048_576) return `${(bytes / 1_048_576).toFixed(0)} MB`;
  return `${(bytes / 1024).toFixed(0)} KB`;
}

function statusDot(status: ServiceHealth['status']): string {
  const cls: Record<string, string> = {
    online: 'dot-online', degraded: 'dot-degraded', offline: 'dot-offline', disabled: 'dot-disabled',
  };
  return `<span class="dot ${cls[status] ?? 'dot-disabled'}"></span>`;
}

function serviceIcon(id: ServiceId): string {
  return ({ core: '\u25C9', intelligence: '\u25C8', scout: '\u25CE', pulse: '\u25C7' })[id];
}

function serviceLabel(id: ServiceId): string {
  return ({ core: 'Core', intelligence: 'Brain', scout: 'Scout', pulse: 'Pulse' })[id];
}

function toggleSwitch(id: ServiceId, enabled: boolean): string {
  const checked = enabled ? 'checked' : '';
  return `<label class="toggle" title="${enabled ? 'Disable' : 'Enable'} ${serviceLabel(id)}">
    <input type="checkbox" ${checked} onchange="toggleService('${id}', this.checked)">
    <span class="slider"></span>
  </label>`;
}

function collapsible(sectionId: string, title: string, body: string, collapsed: boolean): string {
  const cls = collapsed ? '' : 'open';
  const display = collapsed ? 'none' : 'block';
  return `
  <div class="collapsible">
    <button class="section-toggle" onclick="toggleSection('${sectionId}')">
      <span class="chevron ${cls}">\u203A</span>
      <span>${title}</span>
    </button>
    <div class="section-body" style="display:${display}">
      ${body}
    </div>
  </div>`;
}

function sparklineSvg(points: number[], width = 140, height = 28): string {
  if (!points || points.length < 2) return '';
  const max = Math.max(...points, 1);
  const step = width / (points.length - 1);
  const coords = points
    .map((v, i) => `${(i * step).toFixed(1)},${(height - (v / max) * (height - 2)).toFixed(1)}`)
    .join(' ');
  return `<svg width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" xmlns="http://www.w3.org/2000/svg">
    <polyline fill="none" class="chart-line" stroke-width="1.5"
      stroke-linecap="round" stroke-linejoin="round" points="${coords}"/>
  </svg>`;
}

function scaleBadge(scale: string): string {
  const cls: Record<string, string> = {
    micro: 'badge-micro', small: 'badge-small', medium: 'badge-medium', large: 'badge-large',
  };
  return `<span class="scale-badge ${cls[scale] ?? ''}">${scale}</span>`;
}

function severityBadge(severity: string): string {
  const cls: Record<string, string> = {
    error: 'sev-error', warning: 'sev-warning', info: 'sev-info',
  };
  return `<span class="severity-badge ${cls[severity] ?? 'sev-info'}">${severity}</span>`;
}

function entityIcon(type: string): string {
  return ({
    'function': 'fn\u25B8', struct: '\u25C6', 'interface': '\u25C7', method: '\u25B9',
    variable: '\u25CF', 'package': '\u25A1', file: '\u25AB',
  })[type] ?? '\u00B7';
}

// ── Tab bar ──────────────────────────────────────────────────────────────

function tabBar(active: SidebarTab): string {
  const tabs: { id: SidebarTab; icon: string; label: string }[] = [
    { id: 'home', icon: '\u25C9', label: 'Home' },
    { id: 'intelligence', icon: '\u25C8', label: 'Intelligence' },
    { id: 'analytics', icon: '\u25C7', label: 'Analytics' },
    { id: 'explorer', icon: '\u2B21', label: 'Explorer' },
  ];
  return `<div class="tab-bar">${tabs.map((t) =>
    `<button class="tab ${t.id === active ? 'active' : ''}" onclick="switchTab('${t.id}')" title="${t.label}">
      <span class="tab-icon">${t.icon}</span>
      <span class="tab-label">${t.label}</span>
    </button>`
  ).join('')}</div>`;
}

// ── Home tab ─────────────────────────────────────────────────────────────

function homeTabHtml(state: SidebarState): string {
  const ids: ServiceId[] = ['core', 'intelligence', 'scout', 'pulse'];

  const tiles = ids.map((id) => {
    const h = state.health[id];
    const enabled = h.status !== 'disabled';
    const meta = h.version
      ? `<span class="tile-meta">${esc(h.version)}${h.latencyMs !== undefined ? ` \u00B7 ${h.latencyMs}ms` : ''}</span>`
      : `<span class="tile-meta status-${h.status}">${h.status}</span>`;
    return `<div class="tile ${h.status}">
      <div class="tile-header">
        <span class="tile-icon">${serviceIcon(id)}</span>
        <span class="tile-name">${serviceLabel(id)}</span>
        ${statusDot(h.status)}
      </div>
      <div class="tile-footer">${meta}${toggleSwitch(id, enabled)}</div>
    </div>`;
  }).join('');

  let identityCard = '';
  const pi = state.projectIdentity;
  if (pi) {
    const langs = pi.languages?.length ? pi.languages.slice(0, 4).join(', ') : '\u2014';
    const gs = pi.summary;
    const totalNodes = gs.files + gs.functions + gs.methods + gs.structs + gs.interfaces;
    identityCard = `
    <div class="card">
      <div class="card-header">
        <span class="card-title">Project</span>
        ${scaleBadge(pi.scale)}
      </div>
      <div class="stats-grid">
        <div class="stat-mini"><span class="stat-val">${fmtNum(totalNodes)}</span><span class="stat-lbl">nodes</span></div>
        <div class="stat-mini"><span class="stat-val">${fmtNum(gs.edges)}</span><span class="stat-lbl">edges</span></div>
        <div class="stat-mini"><span class="stat-val">${fmtNum(gs.files)}</span><span class="stat-lbl">files</span></div>
        <div class="stat-mini"><span class="stat-val">${langs}</span><span class="stat-lbl">languages</span></div>
      </div>
    </div>`;
  }

  let roiCard = '';
  if (state.pulse) {
    const p = state.pulse;
    const trend = state.pulseTrend ?? [];
    const sparkPoints = trend.map((t) => t.tokens_saved);
    roiCard = `
    <div class="card roi-card">
      <div class="roi-row">
        <div>
          <span class="roi-num">${fmtNum(p.tokens_saved)}</span>
          <span class="roi-label">tokens saved</span>
        </div>
        <div>
          <span class="roi-num">${p.cost_saved_usd > 0 ? '$' + p.cost_saved_usd.toFixed(2) : '\u2014'}</span>
          <span class="roi-label">cost saved</span>
        </div>
      </div>
      ${sparklineSvg(sparkPoints) ? `<div class="sparkline">${sparklineSvg(sparkPoints)}</div>` : ''}
    </div>`;
  }

  return `
  <div class="section">
    <div class="section-header">
      <span class="section-title">Services</span>
      <button class="link-btn" onclick="refreshHealth()" title="Refresh">\u21BB</button>
    </div>
    <div class="tiles">${tiles}</div>
  </div>
  ${identityCard}
  ${roiCard}`;
}

// ── Intelligence tab ─────────────────────────────────────────────────────

function intelligenceTabHtml(state: SidebarState): string {
  const { ollamaStatus, ollamaModels, defaultModel, modelPullProgress, brainHealth, patterns, adrs, sdlc } = state;

  let ollamaBody = '';
  if (ollamaStatus === 'not-installed') {
    ollamaBody = `<div class="info-box warn">
      <strong>Ollama not found</strong>
      <p>Install Ollama to enable local AI enrichment.</p>
      <button class="btn secondary" onclick="openOllamaInstall()">Open ollama.com \u2192</button>
    </div>`;
  } else if (ollamaStatus === 'stopped') {
    ollamaBody = `<div class="info-box warn">
      <strong>Ollama not running</strong>
      <p>Start Ollama to use local models.</p>
      <button class="btn secondary" onclick="startOllama()">Start Ollama</button>
    </div>`;
  } else {
    const tiers = [
      { label: 'T0 \u00B7 Tiny (0.8b)', value: 'qwen2.5-coder:0.5b' },
      { label: 'T1 \u00B7 Small (1.5b)', value: 'qwen2.5-coder:1.5b' },
      { label: 'T2 \u00B7 Medium (3b)', value: 'qwen2.5-coder:3b' },
      { label: 'T3 \u00B7 Large (7b)', value: 'qwen2.5-coder:7b' },
    ];
    const modelList = ollamaModels.length
      ? `<div class="model-list">${ollamaModels.map((m) => `
          <div class="model-row ${m.name === defaultModel ? 'active' : ''}">
            <span class="model-name">${esc(m.name)}</span>
            <span class="model-size">${fmtBytes(m.size)}</span>
            <button class="icon-btn" title="Delete model" onclick="deleteModel('${esc(m.name)}')">\u2715</button>
          </div>`).join('')}</div>`
      : '<p class="muted">No models installed</p>';

    ollamaBody = `
      <div class="row">
        <span class="label">Active model</span>
        <select onchange="setDefaultModel(this.value)">
          ${tiers.map((t) => `<option value="${t.value}"${t.value === defaultModel ? ' selected' : ''}>${t.label}</option>`).join('')}
        </select>
      </div>
      ${modelPullProgress
        ? `<div class="progress-wrap">
            <div class="progress-label">${esc(modelPullProgress.status)}</div>
            <div class="progress-bar"><div class="progress-fill" style="width:${modelPullProgress.pct}%"></div></div>
          </div>`
        : `<button class="btn secondary small" onclick="pullDefaultModel()">Pull / Update Model</button>`}
      ${modelList}`;
  }

  const phase = sdlc?.phase ?? '\u2014';
  const sdlcHtml = `
  <div class="row" style="margin-top:8px">
    <span class="label">SDLC Phase</span>
    <select onchange="setPhase(this.value)">
      ${['planning', 'development', 'testing', 'review', 'deployment'].map(
        (p) => `<option value="${p}"${p === phase ? ' selected' : ''}>${p}</option>`
      ).join('')}
    </select>
  </div>`;

  let brainStatsHtml = '';
  if (brainHealth) {
    const bh = brainHealth;
    brainStatsHtml = `
    <div class="stats-grid three-col">
      <div class="stat-mini">
        <span class="stat-val">${bh.enrichment_rate !== undefined ? Math.round(bh.enrichment_rate * 100) + '%' : '\u2014'}</span>
        <span class="stat-lbl">enrichment</span>
      </div>
      <div class="stat-mini">
        <span class="stat-val">${bh.patterns_learned !== undefined ? fmtNum(bh.patterns_learned) : '\u2014'}</span>
        <span class="stat-lbl">patterns</span>
      </div>
      <div class="stat-mini">
        <span class="stat-val">${bh.summaries_count !== undefined ? fmtNum(bh.summaries_count) : '\u2014'}</span>
        <span class="stat-lbl">summaries</span>
      </div>
    </div>`;
  }

  let patternsHtml = '';
  if (patterns && patterns.length > 0) {
    const items = patterns.slice(0, 10).map((p) =>
      `<div class="pattern-row">
        <span class="pattern-trigger">${esc(p.trigger)}</span>
        <span class="pattern-arrow">\u2192</span>
        <span class="pattern-target">${esc(p.co_change)}</span>
        <span class="pattern-conf">${Math.round(p.confidence * 100)}%</span>
      </div>`
    ).join('');
    patternsHtml = collapsible('patterns', `Patterns (${patterns.length})`, items, state.collapsedSections['patterns'] ?? false);
  }

  let adrsHtml = '';
  if (adrs && adrs.length > 0) {
    const items = adrs.map((a) => {
      const statusCls = ({ proposed: 'adr-proposed', accepted: 'adr-accepted', deprecated: 'adr-deprecated' })[a.status] ?? '';
      return `<div class="adr-item">
        <div class="adr-header">
          <span class="adr-title">${esc(a.title)}</span>
          <span class="adr-status ${statusCls}">${a.status}</span>
        </div>
        <div class="adr-body">${esc(a.decision)}</div>
      </div>`;
    }).join('');
    adrsHtml = collapsible('adrs', `ADRs (${adrs.length})`, items, state.collapsedSections['adrs'] ?? false);
  }

  return `
  <div class="section">
    <div class="section-header"><span class="section-title">\u25C8 Ollama & Model</span></div>
    ${ollamaBody}
  </div>
  <div class="section">
    ${sdlcHtml}
    ${brainStatsHtml}
  </div>
  ${patternsHtml}
  ${adrsHtml}`;
}

// ── Analytics tab ────────────────────────────────────────────────────────

function analyticsTabHtml(state: SidebarState): string {
  const days = state.analyticsDateRange;

  const dateSelector = `
  <div class="date-range">
    ${[7, 30, 90].map((d) =>
      `<button class="range-btn ${d === days ? 'active' : ''}" onclick="setDateRange(${d})">${d}d</button>`
    ).join('')}
  </div>`;

  if (!state.pulse) {
    return `${dateSelector}
    <div class="empty-section">
      <p class="muted">Pulse offline \u2014 enable the Pulse service to see analytics.</p>
    </div>`;
  }

  const p = state.pulse;
  const trend = state.pulseTrend ?? [];
  const sparkPoints = trend.map((t) => t.tokens_saved);

  const heroHtml = `
  <div class="card hero-card">
    <div class="hero-num">${fmtNum(p.tokens_saved)}</div>
    <div class="hero-label">tokens saved (${days}d)</div>
    ${sparklineSvg(sparkPoints, 200, 32) ? `<div class="sparkline">${sparklineSvg(sparkPoints, 200, 32)}</div>` : ''}
  </div>`;

  const statsHtml = `
  <div class="stats-grid four-col">
    <div class="stat-mini"><span class="stat-val">${p.compression_ratio.toFixed(1)}:1</span><span class="stat-lbl">compression</span></div>
    <div class="stat-mini"><span class="stat-val">${p.cost_saved_usd > 0 ? '$' + p.cost_saved_usd.toFixed(2) : '\u2014'}</span><span class="stat-lbl">cost saved</span></div>
    <div class="stat-mini"><span class="stat-val">${Math.round(p.cache_hit_rate * 100)}%</span><span class="stat-lbl">cache hit</span></div>
    <div class="stat-mini"><span class="stat-val">${p.savings_pct.toFixed(0)}%</span><span class="stat-lbl">reduction</span></div>
  </div>`;

  const tools = p.top_tools ?? [];
  const maxCalls = tools.reduce((m, t) => Math.max(m, t.calls), 0);
  let toolsHtml = '';
  if (tools.length > 0) {
    const bars = tools.slice(0, 8).map((t) => {
      const pct = maxCalls > 0 ? Math.round((t.calls / maxCalls) * 100) : 0;
      return `<div class="bar-row">
        <span class="bar-label">${esc(t.name.replace('synapses.', ''))}</span>
        <div class="bar-track"><div class="bar-fill" style="width:${pct}%"></div></div>
        <span class="bar-val">${fmtNum(t.calls)}</span>
      </div>`;
    }).join('');
    toolsHtml = collapsible('tools', 'Top Tools', bars, state.collapsedSections['tools'] ?? false);
  }

  let agentsHtml = '';
  if (state.pulseAgents && state.pulseAgents.length > 0) {
    const rows = state.pulseAgents.map((a) =>
      `<div class="agent-row">
        <span class="agent-id">${esc(a.agent_id)}</span>
        <span class="agent-stat">${a.sessions}s</span>
        <span class="agent-stat">${fmtNum(a.tool_calls)} calls</span>
        <span class="agent-stat">${fmtNum(a.tokens_saved)} saved</span>
      </div>`
    ).join('');
    agentsHtml = collapsible('agents', `Agents (${state.pulseAgents.length})`, rows, state.collapsedSections['agents'] ?? false);
  }

  let costsHtml = '';
  if (state.brainCosts && state.brainCosts.length > 0) {
    const rows = state.brainCosts.map((c) =>
      `<div class="cost-row">
        <span class="cost-tier">${esc(c.tier)}</span>
        <span class="cost-model">${esc(c.model)}</span>
        <span class="cost-val">${fmtNum(c.tokens)} tok</span>
        <span class="cost-val">${fmtNum(c.calls)} calls</span>
      </div>`
    ).join('');
    costsHtml = collapsible('brainCosts', 'Brain Costs', rows, state.collapsedSections['brainCosts'] ?? false);
  }

  return `
  ${dateSelector}
  ${heroHtml}
  ${statsHtml}
  ${toolsHtml}
  ${agentsHtml}
  ${costsHtml}
  <div class="section" style="text-align:center;padding:8px">
    <button class="link-btn" onclick="openPulseDashboard()">Open Full Dashboard \u2192</button>
  </div>`;
}

// ── Explorer tab ─────────────────────────────────────────────────────────

function explorerTabHtml(state: SidebarState): string {
  let entitiesHtml = '';
  const entities = state.keyEntities ?? [];
  if (entities.length > 0) {
    const items = entities.slice(0, 15).map((e) =>
      `<div class="entity-row" onclick="navigateToEntity('${esc(e.file)}', ${e.line})">
        <span class="entity-icon">${entityIcon(e.type)}</span>
        <span class="entity-name">${esc(e.name)}</span>
        <span class="entity-loc">${esc(e.file.split('/').pop() ?? '')}:${e.line}</span>
        <span class="entity-fan">\u2193${e.fanin} \u2191${e.fanout}</span>
      </div>`
    ).join('');
    entitiesHtml = collapsible('entities', `Key Entities (${entities.length})`, items, state.collapsedSections['entities'] ?? false);
  } else {
    entitiesHtml = '<p class="muted" style="padding:8px 12px">No entity data \u2014 run re-index first.</p>';
  }

  let violationsHtml = '';
  const violations = state.violations ?? [];
  if (violations.length > 0) {
    const items = violations.map((v) =>
      `<div class="violation-row">
        ${severityBadge(v.severity)}
        <span class="violation-rule">${esc(v.rule_name)}</span>
        <span class="violation-detail">${esc(v.from)} \u2192 ${esc(v.to)}</span>
      </div>`
    ).join('');
    violationsHtml = collapsible('violations', `Violations (${violations.length})`, items, state.collapsedSections['violations'] ?? false);
  }

  let rulesHtml = '';
  const rules = state.suggestedRules ?? [];
  if (rules.length > 0) {
    const items = rules.map((r) =>
      `<div class="rule-row">
        <span class="rule-desc">${esc(r.description)}</span>
        <div class="conf-bar"><div class="conf-fill" style="width:${Math.round(r.confidence * 100)}%"></div></div>
        <span class="conf-val">${Math.round(r.confidence * 100)}%</span>
      </div>`
    ).join('');
    rulesHtml = collapsible('rules', `Suggested Rules (${rules.length})`, items, state.collapsedSections['rules'] ?? true);
  }

  let graphHtml = '';
  const gs = state.graphSummary;
  if (gs) {
    graphHtml = `
    <div class="card" style="margin-top:8px">
      <div class="card-header"><span class="card-title">Graph Stats</span></div>
      <div class="stats-grid three-col">
        <div class="stat-mini"><span class="stat-val">${fmtNum(gs.files)}</span><span class="stat-lbl">files</span></div>
        <div class="stat-mini"><span class="stat-val">${fmtNum(gs.functions)}</span><span class="stat-lbl">functions</span></div>
        <div class="stat-mini"><span class="stat-val">${fmtNum(gs.methods)}</span><span class="stat-lbl">methods</span></div>
        <div class="stat-mini"><span class="stat-val">${fmtNum(gs.structs)}</span><span class="stat-lbl">structs</span></div>
        <div class="stat-mini"><span class="stat-val">${fmtNum(gs.interfaces)}</span><span class="stat-lbl">interfaces</span></div>
        <div class="stat-mini"><span class="stat-val">${fmtNum(gs.edges)}</span><span class="stat-lbl">edges</span></div>
      </div>
    </div>`;
  }

  return `${entitiesHtml}${violationsHtml}${rulesHtml}${graphHtml}`;
}

// ── Main export ──────────────────────────────────────────────────────────

export function buildSidebarHtml(state: SidebarState): string {
  const tab = state.activeTab;

  let content = '';
  switch (tab) {
    case 'home': content = homeTabHtml(state); break;
    case 'intelligence': content = intelligenceTabHtml(state); break;
    case 'analytics': content = analyticsTabHtml(state); break;
    case 'explorer': content = explorerTabHtml(state); break;
  }

  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
${cssDesignSystem()}
</style>
</head>
<body>
${tabBar(tab)}
<div class="tab-content">
${content}
</div>
<script>
${clientScript()}
</script>
</body>
</html>`;
}

// ── CSS Design System ────────────────────────────────────────────────────

function cssDesignSystem(): string {
  return `
  *, *::before, *::after { box-sizing: border-box; }
  :focus-visible { outline: 2px solid var(--vscode-focusBorder); outline-offset: 2px; }

  body {
    font-family: var(--vscode-font-family);
    font-size: var(--vscode-font-size);
    color: var(--vscode-foreground);
    background: var(--vscode-sideBar-background, var(--vscode-editor-background));
    padding: 0; margin: 0;
  }

  .tab-bar {
    display: flex; position: sticky; top: 0; z-index: 10;
    background: var(--vscode-sideBar-background, var(--vscode-editor-background));
    border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
  }
  .tab {
    flex: 1; display: flex; flex-direction: column; align-items: center; gap: 2px;
    padding: 6px 4px; border: none; cursor: pointer;
    background: transparent; color: var(--vscode-descriptionForeground);
    font-size: 0.72em; transition: color 0.2s, border-bottom 0.2s;
    border-bottom: 2px solid transparent;
  }
  .tab:hover { color: var(--vscode-foreground); }
  .tab.active {
    color: var(--vscode-foreground);
    border-bottom-color: var(--vscode-activityBarBadge-background, #007acc);
  }
  .tab-icon { font-size: 1.3em; }
  .tab-label { font-size: 0.85em; }
  .tab-content { padding: 0; }

  .section {
    padding: 10px 12px;
    border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.15));
  }
  .section:last-child { border-bottom: none; }
  .section-header {
    display: flex; align-items: center; justify-content: space-between; margin-bottom: 8px;
  }
  .section-title {
    font-size: 0.78em; font-weight: 700; letter-spacing: 0.08em;
    text-transform: uppercase; color: var(--vscode-descriptionForeground);
  }

  .collapsible { border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.12)); }
  .collapsible:last-child { border-bottom: none; }
  .section-toggle {
    display: flex; align-items: center; gap: 5px; width: 100%;
    padding: 8px 12px; border: none; cursor: pointer;
    background: transparent; color: var(--vscode-foreground);
    font-size: 0.82em; font-weight: 600; text-align: left;
  }
  .section-toggle:hover { background: var(--vscode-list-hoverBackground, rgba(128,128,128,0.1)); }
  .chevron { display: inline-block; transition: transform 0.2s; font-size: 1.1em; }
  .chevron.open { transform: rotate(90deg); }
  .section-body { padding: 0 12px 8px; }

  .card {
    background: var(--vscode-editor-background);
    border: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 6px; padding: 12px; margin: 8px 12px;
  }
  .card-header {
    display: flex; align-items: center; justify-content: space-between; margin-bottom: 8px;
  }
  .card-title {
    font-size: 0.78em; font-weight: 700; text-transform: uppercase;
    color: var(--vscode-descriptionForeground);
  }

  .tiles { display: grid; grid-template-columns: 1fr 1fr; gap: 6px; }
  .tile {
    background: var(--vscode-editor-background);
    border: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 6px; padding: 8px 10px;
    display: flex; flex-direction: column; gap: 6px; transition: opacity 0.2s;
  }
  .tile.offline { opacity: 0.6; }
  .tile.disabled { opacity: 0.35; }
  .tile-header { display: flex; align-items: center; gap: 5px; }
  .tile-icon { font-size: 1.1em; }
  .tile-name { font-weight: 600; font-size: 0.85em; flex: 1; }
  .tile-footer { display: flex; align-items: center; justify-content: space-between; }
  .tile-meta { font-size: 0.75em; color: var(--vscode-descriptionForeground); flex: 1; }
  .status-offline { color: var(--vscode-errorForeground, #f44336); }
  .status-online { color: var(--vscode-testing-iconPassed, #4caf50); }
  .status-degraded { color: var(--vscode-editorWarning-foreground, #ff9800); }

  .dot { width: 7px; height: 7px; border-radius: 50%; display: inline-block; flex-shrink: 0; }
  .dot-online { background: var(--vscode-testing-iconPassed, #4caf50); }
  .dot-degraded { background: var(--vscode-editorWarning-foreground, #ff9800); }
  .dot-offline { background: var(--vscode-errorForeground, #f44336); }
  .dot-disabled { background: var(--vscode-descriptionForeground, #666); }

  .toggle { position: relative; width: 32px; height: 18px; flex-shrink: 0; cursor: pointer; }
  .toggle input { opacity: 0; width: 0; height: 0; position: absolute; }
  .slider {
    position: absolute; inset: 0; background: var(--vscode-widget-border, #555);
    border-radius: 18px; transition: background 0.2s;
  }
  .slider::before {
    content: ''; position: absolute; width: 12px; height: 12px;
    background: #fff; border-radius: 50%; top: 3px; left: 3px; transition: left 0.2s;
  }
  .toggle input:checked + .slider { background: var(--vscode-activityBarBadge-background, #007acc); }
  .toggle input:checked + .slider::before { left: 17px; }

  .stats-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 6px; }
  .stats-grid.three-col { grid-template-columns: 1fr 1fr 1fr; }
  .stats-grid.four-col { grid-template-columns: 1fr 1fr 1fr 1fr; }
  .stat-mini { text-align: center; padding: 4px 0; }
  .stat-val { display: block; font-size: 1.1em; font-weight: 700; }
  .stat-lbl { display: block; font-size: 0.7em; color: var(--vscode-descriptionForeground); text-transform: uppercase; letter-spacing: 0.05em; }

  .scale-badge {
    display: inline-block; padding: 1px 7px; border-radius: 10px;
    font-size: 0.72em; font-weight: 600; text-transform: uppercase;
    background: var(--vscode-badge-background); color: var(--vscode-badge-foreground);
  }
  .badge-micro { background: rgba(76,175,80,0.2); color: var(--vscode-testing-iconPassed, #4caf50); }
  .badge-small { background: rgba(33,150,243,0.2); color: #2196f3; }
  .badge-medium { background: rgba(255,152,0,0.2); color: var(--vscode-editorWarning-foreground, #ff9800); }
  .badge-large { background: rgba(244,67,54,0.2); color: var(--vscode-errorForeground, #f44336); }

  .severity-badge {
    display: inline-block; padding: 1px 6px; border-radius: 3px;
    font-size: 0.7em; font-weight: 600; text-transform: uppercase;
  }
  .sev-error { background: rgba(244,67,54,0.2); color: var(--vscode-errorForeground, #f44336); }
  .sev-warning { background: rgba(255,152,0,0.2); color: var(--vscode-editorWarning-foreground, #ff9800); }
  .sev-info { background: rgba(33,150,243,0.2); color: #2196f3; }

  .roi-row { display: flex; gap: 16px; margin-bottom: 4px; }
  .roi-num { font-size: 1.3em; font-weight: 700; }
  .roi-label { display: block; font-size: 0.72em; color: var(--vscode-descriptionForeground); }
  .sparkline { margin-top: 4px; }

  .chart-line { stroke: var(--vscode-activityBarBadge-background, #007acc); }
  .chart-fill { fill: var(--vscode-activityBarBadge-background, #007acc); opacity: 0.12; }

  .date-range {
    display: flex; gap: 4px; padding: 8px 12px;
    border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.15));
  }
  .range-btn {
    flex: 1; padding: 4px 8px; border: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.3));
    border-radius: 4px; background: transparent; color: var(--vscode-foreground);
    cursor: pointer; font-size: 0.82em; transition: all 0.2s;
  }
  .range-btn:hover { background: var(--vscode-list-hoverBackground, rgba(128,128,128,0.1)); }
  .range-btn.active {
    background: var(--vscode-activityBarBadge-background, #007acc);
    color: var(--vscode-activityBarBadge-foreground, #fff);
    border-color: transparent;
  }

  .hero-card { text-align: center; }
  .hero-num { font-size: 2em; font-weight: 700; line-height: 1; }
  .hero-label { font-size: 0.78em; color: var(--vscode-descriptionForeground); margin: 4px 0; }

  .bar-row { display: flex; align-items: center; gap: 6px; margin: 4px 0; font-size: 0.82em; }
  .bar-label { width: 90px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .bar-track {
    flex: 1; height: 6px; background: var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 3px; overflow: hidden;
  }
  .bar-fill {
    height: 100%; background: var(--vscode-activityBarBadge-background, #007acc);
    border-radius: 3px; transition: width 0.3s;
  }
  .bar-val { width: 32px; text-align: right; font-weight: 600; font-size: 0.9em; }

  .agent-row { display: flex; gap: 8px; padding: 3px 0; font-size: 0.82em; align-items: center; }
  .agent-id { flex: 1; font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .agent-stat { color: var(--vscode-descriptionForeground); font-size: 0.9em; white-space: nowrap; }

  .cost-row { display: flex; gap: 8px; padding: 3px 0; font-size: 0.82em; align-items: center; }
  .cost-tier { font-weight: 600; width: 32px; }
  .cost-model { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .cost-val { color: var(--vscode-descriptionForeground); white-space: nowrap; }

  .entity-row {
    display: flex; align-items: center; gap: 5px; padding: 4px 0;
    font-size: 0.82em; cursor: pointer; transition: background 0.15s;
    border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.08));
  }
  .entity-row:hover { background: var(--vscode-list-hoverBackground, rgba(128,128,128,0.1)); }
  .entity-row:last-child { border-bottom: none; }
  .entity-icon { width: 22px; text-align: center; font-size: 0.9em; flex-shrink: 0; }
  .entity-name { font-weight: 500; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .entity-loc { color: var(--vscode-descriptionForeground); font-size: 0.88em; white-space: nowrap; }
  .entity-fan { color: var(--vscode-descriptionForeground); font-size: 0.82em; white-space: nowrap; }

  .violation-row { display: flex; align-items: center; gap: 6px; padding: 4px 0; font-size: 0.82em; }
  .violation-rule { font-weight: 500; }
  .violation-detail { color: var(--vscode-descriptionForeground); flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  .rule-row { display: flex; align-items: center; gap: 6px; padding: 4px 0; font-size: 0.82em; }
  .rule-desc { flex: 1; }
  .conf-bar { width: 40px; height: 4px; background: var(--vscode-widget-border); border-radius: 2px; overflow: hidden; }
  .conf-fill { height: 100%; background: var(--vscode-activityBarBadge-background, #007acc); border-radius: 2px; }
  .conf-val { width: 28px; text-align: right; font-size: 0.9em; color: var(--vscode-descriptionForeground); }

  .pattern-row { display: flex; align-items: center; gap: 4px; padding: 3px 0; font-size: 0.82em; }
  .pattern-trigger { font-weight: 500; }
  .pattern-arrow { color: var(--vscode-descriptionForeground); }
  .pattern-target { color: var(--vscode-descriptionForeground); flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .pattern-conf { font-size: 0.85em; color: var(--vscode-descriptionForeground); }

  .adr-item { padding: 4px 0; border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.08)); }
  .adr-item:last-child { border-bottom: none; }
  .adr-header { display: flex; align-items: center; gap: 6px; }
  .adr-title { font-weight: 500; font-size: 0.85em; flex: 1; }
  .adr-status { font-size: 0.7em; padding: 1px 5px; border-radius: 3px; font-weight: 600; }
  .adr-proposed { background: rgba(33,150,243,0.2); color: #2196f3; }
  .adr-accepted { background: rgba(76,175,80,0.2); color: var(--vscode-testing-iconPassed, #4caf50); }
  .adr-deprecated { background: rgba(128,128,128,0.2); color: var(--vscode-descriptionForeground); }
  .adr-body { font-size: 0.8em; color: var(--vscode-descriptionForeground); margin-top: 3px; line-height: 1.4; }

  .info-box { border-radius: 5px; padding: 8px 10px; margin: 4px 0; font-size: 0.85em; }
  .info-box.warn {
    background: rgba(255,152,0,0.1); border: 1px solid rgba(255,152,0,0.3);
  }
  .info-box strong { display: block; margin-bottom: 3px; }
  .info-box p { margin: 3px 0 6px; color: var(--vscode-descriptionForeground); }

  .model-list { margin-top: 6px; }
  .model-row {
    display: flex; align-items: center; gap: 6px; padding: 3px 0;
    border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.12)); font-size: 0.83em;
  }
  .model-row:last-child { border-bottom: none; }
  .model-row.active .model-name { color: var(--vscode-activityBarBadge-background); font-weight: 600; }
  .model-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .model-size { color: var(--vscode-descriptionForeground); white-space: nowrap; }
  .icon-btn {
    background: none; border: none; color: var(--vscode-descriptionForeground);
    cursor: pointer; padding: 0 3px; font-size: 0.9em;
  }
  .icon-btn:hover { color: var(--vscode-errorForeground, #f44336); }

  .progress-wrap { margin: 6px 0; }
  .progress-label { font-size: 0.78em; color: var(--vscode-descriptionForeground); margin-bottom: 3px; }
  .progress-bar {
    height: 5px; background: var(--vscode-widget-border, rgba(128,128,128,0.3));
    border-radius: 3px; overflow: hidden;
  }
  .progress-fill {
    height: 100%; background: var(--vscode-activityBarBadge-background);
    border-radius: 3px; transition: width 0.3s;
  }

  .row { display: flex; justify-content: space-between; align-items: center; padding: 2px 0; }
  .label { color: var(--vscode-descriptionForeground); font-size: 0.85em; }
  .muted { color: var(--vscode-descriptionForeground); font-size: 0.82em; margin: 4px 0; }
  .empty-section { text-align: center; padding: 20px 12px; }

  .btn, button.btn {
    padding: 5px 8px;
    background: var(--vscode-button-background); color: var(--vscode-button-foreground);
    border: none; border-radius: 3px; cursor: pointer; font-size: 0.84em;
  }
  .btn:hover { background: var(--vscode-button-hoverBackground); }
  .btn.secondary {
    background: var(--vscode-button-secondaryBackground);
    color: var(--vscode-button-secondaryForeground);
  }
  .btn.secondary:hover { background: var(--vscode-button-secondaryHoverBackground); }
  .btn.small { padding: 3px 7px; font-size: 0.78em; margin-top: 4px; }
  .link-btn {
    background: none; border: none; color: var(--vscode-activityBarBadge-background);
    cursor: pointer; font-size: 0.78em; padding: 0;
  }
  .link-btn:hover { text-decoration: underline; }
  select {
    background: var(--vscode-dropdown-background); color: var(--vscode-dropdown-foreground);
    border: 1px solid var(--vscode-dropdown-border); border-radius: 3px;
    font-size: 0.83em; padding: 2px 4px;
  }
  `;
}

// ── Client-side script ───────────────────────────────────────────────────

function clientScript(): string {
  return `
const vscode = acquireVsCodeApi();

function switchTab(tab) { vscode.postMessage({ command: 'switchTab', tab }); }
function toggleSection(section) { vscode.postMessage({ command: 'toggleSection', section }); }
function setDateRange(days) { vscode.postMessage({ command: 'setDateRange', days }); }
function toggleService(id, enabled) { vscode.postMessage({ command: 'toggleService', service: id, enabled }); }
function refreshHealth() { vscode.postMessage({ command: 'refreshHealth' }); }
function openPulseDashboard() { vscode.postMessage({ command: 'openPulseDashboard' }); }
function setDefaultModel(model) { vscode.postMessage({ command: 'setDefaultModel', model }); }
function pullDefaultModel() { vscode.postMessage({ command: 'pullModel' }); }
function deleteModel(model) { vscode.postMessage({ command: 'deleteModel', model }); }
function openOllamaInstall() { vscode.postMessage({ command: 'openExternal', url: 'https://ollama.com' }); }
function startOllama() { vscode.postMessage({ command: 'startOllama' }); }
function refreshContext() { vscode.postMessage({ command: 'refreshContext' }); }
function injectContext() { vscode.postMessage({ command: 'injectContext' }); }
function setPhase(phase) { vscode.postMessage({ command: 'setPhase', phase }); }
function navigateToEntity(file, line) { vscode.postMessage({ command: 'navigateToEntity', file, line }); }
function showGraphExplorer() { vscode.postMessage({ command: 'showGraphExplorer' }); }
  `;
}
