import { SidebarState, ServiceId, ServiceHealth, OllamaModel } from '../types';

function esc(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
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
  const colors: Record<string, string> = {
    online: '#4caf50',
    degraded: '#ff9800',
    offline: '#f44336',
    disabled: '#666',
  };
  return `<span class="dot" style="background:${colors[status] ?? '#666'}"></span>`;
}

function serviceIcon(id: ServiceId): string {
  const icons: Record<ServiceId, string> = {
    core: '◉',
    intelligence: '◈',
    scout: '◎',
    pulse: '◇',
  };
  return icons[id];
}

function serviceLabel(id: ServiceId): string {
  const labels: Record<ServiceId, string> = {
    core: 'Core',
    intelligence: 'Brain',
    scout: 'Scout',
    pulse: 'Pulse',
  };
  return labels[id];
}

function toggleSwitch(id: ServiceId, enabled: boolean): string {
  const checked = enabled ? 'checked' : '';
  return `
    <label class="toggle" title="${enabled ? 'Disable' : 'Enable'} ${serviceLabel(id)}">
      <input type="checkbox" ${checked} onchange="toggleService('${id}', this.checked)">
      <span class="slider"></span>
    </label>`;
}

function serviceTile(health: ServiceHealth): string {
  const { id, status, version, latencyMs } = health;
  const enabled = status !== 'disabled';
  const onlineInfo = version
    ? `<span class="tile-meta">${esc(version)}${latencyMs !== undefined ? ` · ${latencyMs}ms` : ''}</span>`
    : `<span class="tile-meta ${status}">${status}</span>`;

  return `
  <div class="tile ${status}">
    <div class="tile-header">
      <span class="tile-icon">${serviceIcon(id)}</span>
      <span class="tile-name">${serviceLabel(id)}</span>
      ${statusDot(status)}
    </div>
    <div class="tile-footer">
      ${onlineInfo}
      ${toggleSwitch(id, enabled)}
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
    <polyline fill="none" stroke="var(--vscode-activityBarBadge-background)" stroke-width="1.5"
      stroke-linecap="round" stroke-linejoin="round" points="${coords}"/>
  </svg>`;
}

function pulseSectionHtml(state: SidebarState): string {
  if (!state.pulse) return '';

  const { tokens_saved, savings_pct, compression_ratio, cost_saved_usd } = state.pulse;
  const trend = state.pulseTrend ?? [];
  const sparkPoints = trend.map((p) => p.tokens_saved);
  const sparkline = sparklineSvg(sparkPoints);

  return `
  <div class="section">
    <div class="section-header">
      <span class="section-title">◇ Analytics</span>
      <button class="link-btn" onclick="openPulseDashboard()">Full Dashboard →</button>
    </div>
    <div class="pulse-hero">
      <div class="pulse-stat">
        <span class="pulse-num">${fmtNum(tokens_saved)}</span>
        <span class="pulse-label">tokens saved</span>
      </div>
      <div class="pulse-badges">
        <span class="badge">${savings_pct.toFixed(0)}% reduction</span>
        <span class="badge">${compression_ratio.toFixed(1)}:1</span>
        ${cost_saved_usd > 0 ? `<span class="badge green">$${cost_saved_usd.toFixed(2)} saved</span>` : ''}
      </div>
      ${sparkline ? `<div class="sparkline">${sparkline}</div>` : ''}
    </div>
  </div>`;
}

function ollamaModelList(models: OllamaModel[], defaultModel: string): string {
  if (!models.length) return '<p class="muted">No models installed</p>';
  return `<div class="model-list">${models.map((m) => `
    <div class="model-row ${m.name === defaultModel ? 'active' : ''}">
      <span class="model-name">${esc(m.name)}</span>
      <span class="model-size">${fmtBytes(m.size)}</span>
      <button class="icon-btn" title="Delete model" onclick="deleteModel('${esc(m.name)}')">✕</button>
    </div>`).join('')}</div>`;
}

function intelligenceSectionHtml(state: SidebarState): string {
  const { ollamaStatus, ollamaModels, defaultModel, modelPullProgress } = state;

  let body = '';

  if (ollamaStatus === 'not-installed') {
    body = `
      <div class="info-box warn">
        <strong>Ollama not found</strong>
        <p>Install Ollama to enable local AI enrichment.</p>
        <button class="btn secondary" onclick="openOllamaInstall()">Open ollama.com →</button>
      </div>`;
  } else if (ollamaStatus === 'stopped') {
    body = `
      <div class="info-box warn">
        <strong>Ollama not running</strong>
        <p>Start Ollama to use local models.</p>
        <button class="btn secondary" onclick="startOllama()">Start Ollama</button>
      </div>`;
  } else {
    const tiers = [
      { label: 'T0 · Tiny (0.8b)', value: 'qwen2.5-coder:0.5b' },
      { label: 'T1 · Small (1.5b)', value: 'qwen2.5-coder:1.5b' },
      { label: 'T2 · Medium (3b)', value: 'qwen2.5-coder:3b' },
      { label: 'T3 · Large (7b)', value: 'qwen2.5-coder:7b' },
    ];
    body = `
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
      ${ollamaModelList(ollamaModels, defaultModel)}`;
  }

  return `
  <div class="section">
    <div class="section-header">
      <span class="section-title">◈ Intelligence</span>
    </div>
    ${body}
  </div>`;
}

function contextSectionHtml(state: SidebarState): string {
  const phase = state.sdlc?.phase ?? '—';
  const quality = state.contextPacket?.packet_quality ?? null;
  const qualityPct = quality !== null ? Math.round(quality * 100) : null;
  const qualityColor =
    quality === null ? '#666' :
    quality >= 0.9 ? '#4caf50' :
    quality >= 0.5 ? '#ff9800' : '#f44336';

  const summaryHtml = state.contextPacket?.root_summary
    ? `<p class="summary">${esc(state.contextPacket.root_summary)}</p>`
    : `<p class="muted">Open a file and hover over a symbol</p>`;

  const insightHtml = state.contextPacket?.insight
    ? `<p class="insight">${esc(state.contextPacket.insight)}</p>`
    : '';

  const concernsHtml = state.contextPacket?.concerns?.length
    ? `<ul class="concerns">${state.contextPacket.concerns.map((c) => `<li>${esc(c)}</li>`).join('')}</ul>`
    : '';

  const statsLines = (state.graphStats ?? '')
    .split('\n')
    .filter((l) => /Files|Functions|Methods|CALLS|IMPLEMENTS/.test(l))
    .map((l) => l.trim())
    .slice(0, 6);
  const statsHtml = statsLines.length
    ? `<pre class="stats">${statsLines.map(esc).join('\n')}</pre>`
    : '';

  return `
  <div class="section">
    <div class="section-header">
      <span class="section-title">◉ Context</span>
      <div class="phase-row">
        <select id="phaseSelect" onchange="setPhase(this.value)">
          ${['planning', 'development', 'testing', 'review', 'deployment'].map(
            (p) => `<option value="${p}"${p === phase ? ' selected' : ''}>${p}</option>`
          ).join('')}
        </select>
      </div>
    </div>

    ${qualityPct !== null ? `
    <div class="row">
      <span class="label">Quality</span>
      <span class="value">
        <span class="quality-bar"><span class="quality-fill" style="width:${qualityPct}%;background:${qualityColor}"></span></span>
        ${qualityPct}%
      </span>
    </div>` : ''}

    ${summaryHtml}
    ${insightHtml ? `<div class="insight">${insightHtml}</div>` : ''}
    ${concernsHtml ? `<ul class="concerns">${concernsHtml}</ul>` : ''}

    ${statsHtml || `<p class="muted small">No graph — run Synapses: Re-index</p>`}

    <div class="btn-row">
      <button class="btn" onclick="refreshContext()">↻ Refresh</button>
      <button class="btn secondary" onclick="injectContext()">⇥ Copy</button>
    </div>
  </div>`;
}

export function buildSidebarHtml(state: SidebarState): string {
  const ids: ServiceId[] = ['core', 'intelligence', 'scout', 'pulse'];

  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  *, *::before, *::after { box-sizing: border-box; }
  body {
    font-family: var(--vscode-font-family);
    font-size: var(--vscode-font-size);
    color: var(--vscode-foreground);
    background: var(--vscode-sideBar-background, var(--vscode-editor-background));
    padding: 0;
    margin: 0;
  }

  /* ---- Sections ---- */
  .section {
    padding: 10px 12px;
    border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
  }
  .section:last-child { border-bottom: none; }
  .section-header {
    display: flex; align-items: center; justify-content: space-between;
    margin-bottom: 8px;
  }
  .section-title {
    font-size: 0.78em; font-weight: 700; letter-spacing: 0.1em;
    text-transform: uppercase; color: var(--vscode-descriptionForeground);
  }

  /* ---- Service tiles ---- */
  .tiles {
    display: grid; grid-template-columns: 1fr 1fr; gap: 6px;
    margin-bottom: 2px;
  }
  .tile {
    background: var(--vscode-editor-background);
    border: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 6px; padding: 8px 10px;
    display: flex; flex-direction: column; gap: 6px;
  }
  .tile.offline { opacity: 0.6; }
  .tile.disabled { opacity: 0.35; }
  .tile-header {
    display: flex; align-items: center; gap: 5px;
  }
  .tile-icon { font-size: 1.1em; }
  .tile-name { font-weight: 600; font-size: 0.85em; flex: 1; }
  .tile-footer {
    display: flex; align-items: center; justify-content: space-between;
  }
  .tile-meta {
    font-size: 0.75em; color: var(--vscode-descriptionForeground); flex: 1;
  }
  .tile-meta.offline { color: #f44336; }
  .tile-meta.online  { color: #4caf50; }

  /* ---- Status dot ---- */
  .dot {
    width: 7px; height: 7px; border-radius: 50%; display: inline-block;
    flex-shrink: 0;
  }

  /* ---- CSS toggle switch ---- */
  .toggle {
    position: relative; width: 32px; height: 18px; flex-shrink: 0; cursor: pointer;
  }
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

  /* ---- Pulse section ---- */
  .pulse-hero { display: flex; flex-direction: column; gap: 4px; }
  .pulse-stat { display: flex; align-items: baseline; gap: 5px; }
  .pulse-num { font-size: 1.5em; font-weight: 700; }
  .pulse-label { font-size: 0.8em; color: var(--vscode-descriptionForeground); }
  .pulse-badges { display: flex; flex-wrap: wrap; gap: 4px; }
  .badge {
    display: inline-block; background: var(--vscode-badge-background);
    color: var(--vscode-badge-foreground); padding: 1px 7px; border-radius: 10px;
    font-size: 0.75em; font-weight: 600;
  }
  .badge.green { background: rgba(76, 175, 80, 0.2); color: #4caf50; }
  .sparkline { margin-top: 4px; }
  .link-btn {
    background: none; border: none; color: var(--vscode-activityBarBadge-background);
    cursor: pointer; font-size: 0.75em; padding: 0; text-decoration: none;
  }
  .link-btn:hover { text-decoration: underline; }

  /* ---- Info boxes ---- */
  .info-box {
    border-radius: 5px; padding: 8px 10px; margin: 4px 0; font-size: 0.85em;
  }
  .info-box.warn {
    background: rgba(255, 152, 0, 0.1);
    border: 1px solid rgba(255, 152, 0, 0.3);
  }
  .info-box strong { display: block; margin-bottom: 3px; }
  .info-box p { margin: 3px 0 6px; color: var(--vscode-descriptionForeground); }

  /* ---- Model list ---- */
  .model-list { margin-top: 6px; }
  .model-row {
    display: flex; align-items: center; gap: 6px;
    padding: 3px 0; border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.15));
    font-size: 0.83em;
  }
  .model-row:last-child { border-bottom: none; }
  .model-row.active .model-name { color: var(--vscode-activityBarBadge-background); font-weight: 600; }
  .model-name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .model-size { color: var(--vscode-descriptionForeground); white-space: nowrap; }
  .icon-btn {
    background: none; border: none; color: var(--vscode-descriptionForeground);
    cursor: pointer; padding: 0 3px; font-size: 0.9em;
  }
  .icon-btn:hover { color: #f44336; }

  /* ---- Progress bar ---- */
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

  /* ---- Context section ---- */
  .row { display: flex; justify-content: space-between; align-items: center; padding: 2px 0; }
  .label { color: var(--vscode-descriptionForeground); font-size: 0.85em; }
  .value { font-weight: 500; display: flex; align-items: center; gap: 4px; }
  .quality-bar {
    display: inline-block; width: 55px; height: 5px;
    background: var(--vscode-widget-border); border-radius: 3px; overflow: hidden;
    vertical-align: middle;
  }
  .quality-fill { height: 100%; border-radius: 3px; }
  p.summary { font-size: 0.88em; line-height: 1.4; margin: 4px 0; }
  p.muted, .muted { color: var(--vscode-descriptionForeground); font-size: 0.82em; margin: 4px 0; }
  .muted.small { font-size: 0.78em; }
  .insight {
    font-size: 0.85em; line-height: 1.4; margin: 4px 0;
    border-left: 2px solid var(--vscode-activityBarBadge-background); padding-left: 6px;
  }
  ul.concerns { font-size: 0.82em; margin: 4px 0; padding-left: 14px; }
  ul.concerns li { margin: 2px 0; }
  pre.stats {
    font-size: 0.78em; background: var(--vscode-textCodeBlock-background);
    padding: 5px 8px; border-radius: 4px; overflow-x: auto; margin: 4px 0;
    white-space: pre; color: var(--vscode-foreground);
  }

  /* ---- Buttons ---- */
  .btn-row { display: flex; gap: 5px; margin-top: 6px; }
  .btn, button.btn {
    flex: 1; padding: 5px 8px;
    background: var(--vscode-button-background); color: var(--vscode-button-foreground);
    border: none; border-radius: 3px; cursor: pointer; font-size: 0.84em;
  }
  .btn:hover { background: var(--vscode-button-hoverBackground); }
  .btn.secondary {
    background: var(--vscode-button-secondaryBackground);
    color: var(--vscode-button-secondaryForeground);
  }
  .btn.secondary:hover { background: var(--vscode-button-secondaryHoverBackground); }
  .btn.small { padding: 3px 7px; font-size: 0.78em; flex: unset; margin-top: 4px; }
  select {
    background: var(--vscode-dropdown-background); color: var(--vscode-dropdown-foreground);
    border: 1px solid var(--vscode-dropdown-border); border-radius: 3px;
    font-size: 0.83em; padding: 2px 4px;
  }
  .phase-row { display: flex; align-items: center; }
</style>
</head>
<body>

<!-- Section 1: Service Health tiles -->
<div class="section">
  <div class="section-header">
    <span class="section-title">Services</span>
    <button class="link-btn" onclick="refreshHealth()" title="Refresh health">↻</button>
  </div>
  <div class="tiles">
    ${ids.map((id) => serviceTile(state.health[id])).join('')}
  </div>
</div>

<!-- Section 2: Pulse Compact -->
${pulseSectionHtml(state)}

<!-- Section 3: Intelligence / Ollama -->
${intelligenceSectionHtml(state)}

<!-- Section 4: Context Viewer -->
${contextSectionHtml(state)}

<script>
const vscode = acquireVsCodeApi();

function toggleService(id, enabled) {
  vscode.postMessage({ command: 'toggleService', service: id, enabled });
}
function refreshHealth() {
  vscode.postMessage({ command: 'refreshHealth' });
}
function openPulseDashboard() {
  vscode.postMessage({ command: 'openPulseDashboard' });
}
function setDefaultModel(model) {
  vscode.postMessage({ command: 'setDefaultModel', model });
}
function pullDefaultModel() {
  vscode.postMessage({ command: 'pullModel' });
}
function deleteModel(model) {
  vscode.postMessage({ command: 'deleteModel', model });
}
function openOllamaInstall() {
  vscode.postMessage({ command: 'openExternal', url: 'https://ollama.com' });
}
function startOllama() {
  vscode.postMessage({ command: 'startOllama' });
}
function refreshContext() {
  vscode.postMessage({ command: 'refreshContext' });
}
function injectContext() {
  vscode.postMessage({ command: 'injectContext' });
}
function setPhase(phase) {
  vscode.postMessage({ command: 'setPhase', phase });
}

// Accept state updates from the extension host
window.addEventListener('message', (event) => {
  const msg = event.data;
  if (msg.type === 'stateUpdate') {
    // Webview re-render is handled by extension (full HTML replacement)
    // This handler is reserved for future partial updates
  }
});
</script>
</body>
</html>`;
}
