import { PulseDashboard, PulseTimelinePoint, PulseTool } from '../types';

function esc(s: string): string {
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function fmtNum(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return String(Math.round(n));
}

function fmtUsd(n: number): string {
  if (n < 0.01) return '<$0.01';
  return `$${n.toFixed(2)}`;
}

function svgSparkline(points: number[], w = 180, h = 40): string {
  if (!points || points.length < 2) return '';
  const max = Math.max(...points, 1);
  const step = w / (points.length - 1);
  const coords = points
    .map((v, i) => `${(i * step).toFixed(1)},${(h - 2 - ((v / max) * (h - 4))).toFixed(1)}`)
    .join(' ');
  // Area fill
  const first = `0,${h}`;
  const last = `${w},${h}`;
  const area = `${first} ${coords} ${last}`;
  return `<svg width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" xmlns="http://www.w3.org/2000/svg">
  <polygon points="${area}" fill="rgba(0,122,204,0.12)" />
  <polyline fill="none" stroke="#007acc" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" points="${coords}"/>
</svg>`;
}

function svgTimeline(points: PulseTimelinePoint[], w = 600, h = 100): string {
  if (!points || points.length < 2) return `<p style="color:#888">Not enough data yet</p>`;
  const values = points.map((p) => p.tokens_saved);
  const max = Math.max(...values, 1);
  const step = w / (points.length - 1);
  const coords = values
    .map((v, i) => `${(i * step).toFixed(1)},${(h - 2 - ((v / max) * (h - 8))).toFixed(1)}`)
    .join(' ');
  const area = `0,${h} ${coords} ${w},${h}`;

  const labels = points
    .filter((_, i) => i % Math.max(1, Math.floor(points.length / 6)) === 0)
    .map((p, i) => {
      const x = (points.indexOf(p) * step).toFixed(1);
      const date = p.date.slice(5); // MM-DD
      return `<text x="${x}" y="${h + 14}" font-size="10" fill="#666" text-anchor="middle">${date}</text>`;
    })
    .join('');

  return `<svg width="100%" viewBox="0 0 ${w} ${h + 20}" xmlns="http://www.w3.org/2000/svg" style="overflow:visible">
  <polygon points="${area}" fill="rgba(0,122,204,0.10)" />
  <polyline fill="none" stroke="#007acc" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" points="${coords}"/>
  ${labels}
</svg>`;
}

function toolBars(tools: PulseTool[], maxCalls: number): string {
  return tools
    .slice(0, 8)
    .map((t) => {
      const pct = maxCalls > 0 ? Math.round((t.calls / maxCalls) * 100) : 0;
      return `
      <div class="bar-row">
        <span class="bar-label" title="${esc(t.name)}">${esc(t.name.replace('synapses.', ''))}</span>
        <div class="bar-track">
          <div class="bar-fill" style="width:${pct}%"></div>
        </div>
        <span class="bar-val">${fmtNum(t.calls)}</span>
        <span class="bar-sub">${Math.round(t.avg_ms)}ms</span>
      </div>`;
    })
    .join('');
}

export function buildPulseHtml(data: PulseDashboard | null): string {
  const noData = !data || !data.summary;

  const body = noData
    ? `<div class="empty">
         <div class="empty-icon">◇</div>
         <p>No analytics data yet.</p>
         <p class="sub">Make a few tool calls with Synapses active — data will appear here.</p>
       </div>`
    : buildDashboard(data);

  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  *, *::before, *::after { box-sizing: border-box; }
  body {
    font-family: var(--vscode-font-family, -apple-system, BlinkMacSystemFont, sans-serif);
    font-size: var(--vscode-font-size, 13px);
    color: var(--vscode-foreground, #ccc);
    background: var(--vscode-editor-background, #1e1e1e);
    margin: 0; padding: 20px 24px;
    max-width: 860px;
  }

  .header {
    display: flex; align-items: baseline; justify-content: space-between;
    margin-bottom: 20px;
    border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
    padding-bottom: 12px;
  }
  .header h1 { font-size: 1.2em; margin: 0; }
  .header .sub { color: var(--vscode-descriptionForeground, #888); font-size: 0.82em; }

  /* Stat cards row */
  .stat-row { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 12px; margin-bottom: 20px; }
  .stat-card {
    background: var(--vscode-textCodeBlock-background, rgba(128,128,128,0.08));
    border: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 8px; padding: 14px 16px;
  }
  .stat-label { font-size: 0.75em; color: var(--vscode-descriptionForeground, #888);
    text-transform: uppercase; letter-spacing: 0.08em; margin-bottom: 6px; }
  .stat-num { font-size: 2em; font-weight: 700; line-height: 1; }
  .stat-sub { font-size: 0.8em; color: var(--vscode-descriptionForeground, #888); margin-top: 4px; }
  .stat-spark { margin-top: 8px; }

  /* Section blocks */
  .block {
    margin-bottom: 20px;
    background: var(--vscode-textCodeBlock-background, rgba(128,128,128,0.05));
    border: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 8px; padding: 14px 16px;
  }
  .block-title {
    font-size: 0.78em; font-weight: 700; letter-spacing: 0.1em;
    text-transform: uppercase; color: var(--vscode-descriptionForeground, #888);
    margin-bottom: 12px;
  }

  /* Timeline */
  .timeline-wrap { overflow-x: auto; }

  /* Tool bars */
  .bar-row { display: flex; align-items: center; gap: 8px; margin: 5px 0; font-size: 0.84em; }
  .bar-label { width: 120px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    color: var(--vscode-foreground); }
  .bar-track { flex: 1; height: 8px; background: var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 4px; overflow: hidden; }
  .bar-fill { height: 100%; background: #007acc; border-radius: 4px; transition: width 0.3s; }
  .bar-val { width: 36px; text-align: right; font-weight: 600; }
  .bar-sub { width: 40px; text-align: right; color: var(--vscode-descriptionForeground, #888); font-size: 0.9em; }

  /* Empty state */
  .empty { text-align: center; padding: 60px 20px; color: var(--vscode-descriptionForeground); }
  .empty-icon { font-size: 3em; margin-bottom: 12px; }
  .empty p { margin: 6px 0; }
  .empty .sub { font-size: 0.85em; }

  /* Refresh indicator */
  .refresh-row { display: flex; justify-content: flex-end; align-items: center; gap: 8px;
    margin-bottom: 12px; }
  .refresh-btn {
    background: none; border: 1px solid var(--vscode-button-secondaryBackground, #555);
    color: var(--vscode-foreground); border-radius: 4px; padding: 3px 10px;
    cursor: pointer; font-size: 0.82em;
  }
  .refresh-btn:hover { background: var(--vscode-button-secondaryBackground); }
  .last-updated { font-size: 0.78em; color: var(--vscode-descriptionForeground); }
</style>
</head>
<body>
<div class="header">
  <h1>◇ Synapses Pulse</h1>
  <span class="sub">Local-first analytics</span>
</div>

<div class="refresh-row">
  <span class="last-updated" id="lastUpdated">Updated just now</span>
  <button class="refresh-btn" onclick="requestRefresh()">↻ Refresh</button>
</div>

${body}

<script>
const vscode = acquireVsCodeApi();
let lastRefresh = Date.now();

function requestRefresh() {
  vscode.postMessage({ command: 'refresh' });
}

// Update "last updated" label
setInterval(() => {
  const sec = Math.round((Date.now() - lastRefresh) / 1000);
  const el = document.getElementById('lastUpdated');
  if (el) el.textContent = sec < 60 ? 'Updated just now' : \`Updated \${Math.round(sec/60)}m ago\`;
}, 10_000);

window.addEventListener('message', (event) => {
  const msg = event.data;
  if (msg.type === 'refresh') {
    lastRefresh = Date.now();
    // Extension will replace HTML — nothing needed here
  }
});
</script>
</body>
</html>`;
}

function buildDashboard(data: PulseDashboard): string {
  const s = data.summary;
  const trendPoints = (data.timeline ?? []).map((p) => p.tokens_saved);
  const tools = data.tools ?? [];
  const maxCalls = tools.reduce((m, t) => Math.max(m, t.calls), 0);

  return `
  <div class="stat-row">
    <div class="stat-card">
      <div class="stat-label">Tokens Saved (7d)</div>
      <div class="stat-num">${fmtNum(s.tokens_saved)}</div>
      <div class="stat-sub">${s.savings_pct.toFixed(0)}% reduction · ${s.compression_ratio.toFixed(1)}:1</div>
      <div class="stat-spark">${svgSparkline(trendPoints)}</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Cost Saved</div>
      <div class="stat-num">${fmtUsd(s.cost_saved_usd)}</div>
      <div class="stat-sub">based on your model pricing</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">Tool Calls</div>
      <div class="stat-num">${fmtNum(s.tool_calls)}</div>
      <div class="stat-sub">avg ${Math.round(s.avg_latency_ms)}ms · ${Math.round(s.cache_hit_rate * 100)}% cache</div>
    </div>
  </div>

  <div class="block">
    <div class="block-title">30-Day Timeline</div>
    <div class="timeline-wrap">
      ${svgTimeline(data.timeline ?? [])}
    </div>
  </div>

  <div class="block">
    <div class="block-title">Top Tools</div>
    ${tools.length ? toolBars(tools, maxCalls) : '<p style="color:#888;font-size:0.85em">No tool data yet</p>'}
  </div>`;
}
