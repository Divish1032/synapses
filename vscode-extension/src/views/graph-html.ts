import { EntityInfo } from '../types';

function esc(s: string): string {
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function entityIcon(type: string): string {
  return ({
    'function': 'fn\u25B8', struct: '\u25C6', 'interface': '\u25C7', method: '\u25B9',
    variable: '\u25CF', 'package': '\u25A1', file: '\u25AB',
  })[type] ?? '\u00B7';
}

interface GraphData {
  query?: string;
  entity?: EntityInfo & { summary?: string };
  callers?: EntityInfo[];
  callees?: EntityInfo[];
  mermaid?: string;
  error?: string;
}

export function buildGraphHtml(data: GraphData | null): string {
  const body = data?.entity ? buildEntityView(data) :
    data?.error ? `<div class="error">${esc(data.error)}</div>` :
    `<div class="empty"><p>Search for an entity to explore the code graph.</p></div>`;

  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  *, *::before, *::after { box-sizing: border-box; }
  :focus-visible { outline: 2px solid var(--vscode-focusBorder); outline-offset: 2px; }
  body {
    font-family: var(--vscode-font-family, -apple-system, BlinkMacSystemFont, sans-serif);
    font-size: var(--vscode-font-size, 13px);
    color: var(--vscode-foreground, #ccc);
    background: var(--vscode-editor-background, #1e1e1e);
    margin: 0; padding: 20px 24px; max-width: 860px;
  }
  .header { margin-bottom: 16px; }
  .header h1 { font-size: 1.2em; margin: 0 0 12px; }
  .search-row { display: flex; gap: 8px; }
  .search-input {
    flex: 1; padding: 6px 10px;
    background: var(--vscode-input-background); color: var(--vscode-input-foreground);
    border: 1px solid var(--vscode-input-border, rgba(128,128,128,0.3));
    border-radius: 4px; font-size: 0.9em;
  }
  .search-btn {
    padding: 6px 14px;
    background: var(--vscode-button-background); color: var(--vscode-button-foreground);
    border: none; border-radius: 4px; cursor: pointer; font-size: 0.9em;
  }
  .search-btn:hover { background: var(--vscode-button-hoverBackground); }
  .entity-card {
    background: var(--vscode-textCodeBlock-background, rgba(128,128,128,0.08));
    border: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 8px; padding: 14px 16px; margin: 16px 0;
  }
  .entity-name { font-size: 1.2em; font-weight: 700; }
  .entity-type {
    display: inline-block; padding: 1px 7px; border-radius: 10px;
    font-size: 0.75em; font-weight: 600; margin-left: 8px;
    background: var(--vscode-badge-background); color: var(--vscode-badge-foreground);
  }
  .entity-file {
    font-size: 0.82em; color: var(--vscode-descriptionForeground); margin: 4px 0;
    cursor: pointer;
  }
  .entity-file:hover { text-decoration: underline; }
  .entity-summary { font-size: 0.88em; line-height: 1.5; margin: 8px 0; }
  .entity-stats { display: flex; gap: 16px; font-size: 0.85em; color: var(--vscode-descriptionForeground); }
  .block {
    margin: 16px 0;
    background: var(--vscode-textCodeBlock-background, rgba(128,128,128,0.05));
    border: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.2));
    border-radius: 8px; padding: 14px 16px;
  }
  .block-title {
    font-size: 0.78em; font-weight: 700; letter-spacing: 0.1em;
    text-transform: uppercase; color: var(--vscode-descriptionForeground);
    margin-bottom: 10px;
  }
  .ref-row {
    display: flex; align-items: center; gap: 6px; padding: 4px 0;
    font-size: 0.85em; cursor: pointer; transition: background 0.15s;
    border-bottom: 1px solid var(--vscode-widget-border, rgba(128,128,128,0.08));
  }
  .ref-row:hover { background: var(--vscode-list-hoverBackground, rgba(128,128,128,0.1)); }
  .ref-row:last-child { border-bottom: none; }
  .ref-icon { width: 22px; text-align: center; flex-shrink: 0; }
  .ref-name { font-weight: 500; flex: 1; }
  .ref-file { color: var(--vscode-descriptionForeground); font-size: 0.88em; }
  pre.mermaid {
    font-size: 0.82em; background: var(--vscode-textCodeBlock-background);
    padding: 10px 12px; border-radius: 6px; overflow-x: auto;
    white-space: pre; color: var(--vscode-foreground);
  }
  .empty { text-align: center; padding: 60px 20px; color: var(--vscode-descriptionForeground); }
  .error { color: var(--vscode-errorForeground, #f44336); padding: 20px; }
</style>
</head>
<body>
<div class="header">
  <h1>\u2B21 Graph Explorer</h1>
  <div class="search-row">
    <input class="search-input" type="text" id="searchInput" placeholder="Search entity name..."
      value="${esc(data?.query ?? '')}" onkeydown="if(event.key==='Enter')searchEntity()">
    <button class="search-btn" onclick="searchEntity()">Search</button>
  </div>
</div>
${body}
<script>
const vscode = acquireVsCodeApi();
function searchEntity() {
  const q = document.getElementById('searchInput').value.trim();
  if (q) vscode.postMessage({ command: 'search', query: q });
}
function navigate(file, line) {
  vscode.postMessage({ command: 'navigate', file, line });
}
</script>
</body>
</html>`;
}

function buildEntityView(data: GraphData): string {
  const e = data.entity!;
  const callers = data.callers ?? [];
  const callees = data.callees ?? [];

  let callersBlock = '';
  if (callers.length > 0) {
    const rows = callers.map((c) =>
      `<div class="ref-row" onclick="navigate('${esc(c.file)}', ${c.line})">
        <span class="ref-icon">${entityIcon(c.type)}</span>
        <span class="ref-name">${esc(c.name)}</span>
        <span class="ref-file">${esc(c.file.split('/').pop() ?? '')}:${c.line}</span>
      </div>`
    ).join('');
    callersBlock = `<div class="block">
      <div class="block-title">Callers (${callers.length})</div>
      ${rows}
    </div>`;
  }

  let calleesBlock = '';
  if (callees.length > 0) {
    const rows = callees.map((c) =>
      `<div class="ref-row" onclick="navigate('${esc(c.file)}', ${c.line})">
        <span class="ref-icon">${entityIcon(c.type)}</span>
        <span class="ref-name">${esc(c.name)}</span>
        <span class="ref-file">${esc(c.file.split('/').pop() ?? '')}:${c.line}</span>
      </div>`
    ).join('');
    calleesBlock = `<div class="block">
      <div class="block-title">Callees (${callees.length})</div>
      ${rows}
    </div>`;
  }

  let mermaidBlock = '';
  if (data.mermaid) {
    mermaidBlock = `<div class="block">
      <div class="block-title">Graph</div>
      <pre class="mermaid">${esc(data.mermaid)}</pre>
    </div>`;
  }

  return `
  <div class="entity-card">
    <span class="entity-name">${esc(e.name)}</span>
    <span class="entity-type">${e.type}</span>
    <div class="entity-file" onclick="navigate('${esc(e.file)}', ${e.line})">${esc(e.file)}:${e.line}</div>
    ${e.summary ? `<div class="entity-summary">${esc(e.summary)}</div>` : ''}
    <div class="entity-stats">
      <span>\u2193 ${e.fanin} callers</span>
      <span>\u2191 ${e.fanout} callees</span>
    </div>
  </div>
  ${callersBlock}
  ${calleesBlock}
  ${mermaidBlock}`;
}
