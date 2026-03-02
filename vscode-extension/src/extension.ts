import * as vscode from "vscode";
import { execFile } from "child_process";
import { promisify } from "util";
import * as http from "http";

const execFileAsync = promisify(execFile);

// --------------------------------------------------------------------------
// Configuration helpers
// --------------------------------------------------------------------------

function binaryPath(): string {
  return (
    vscode.workspace
      .getConfiguration("synapses")
      .get<string>("binaryPath") || "synapses"
  );
}

function brainUrl(): string {
  return (
    vscode.workspace
      .getConfiguration("synapses")
      .get<string>("brainUrl") || "http://localhost:11435"
  );
}

function workspaceRoot(): string {
  return vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? "";
}

function repoRoot(document: vscode.TextDocument): string {
  return vscode.workspace.getWorkspaceFolder(document.uri)?.uri.fsPath ?? "";
}

/** Extract the identifier under the cursor. */
function identifierAt(
  document: vscode.TextDocument,
  position: vscode.Position
): string | null {
  const range = document.getWordRangeAtPosition(
    position,
    /[A-Za-z_][A-Za-z0-9_]*/
  );
  return range ? document.getText(range) : null;
}

function baseName(filePath: string): string {
  return filePath.split("/").pop() ?? filePath;
}

function inferLanguage(filePath: string): string {
  const ext = filePath.split(".").pop() ?? "";
  const map: Record<string, string> = {
    go: "go", ts: "typescript", tsx: "typescript",
    js: "javascript", jsx: "javascript", py: "python",
    rs: "rust", java: "java", kt: "kotlin", cs: "csharp",
    rb: "ruby", php: "php", swift: "swift", scala: "scala",
  };
  return map[ext] ?? "plaintext";
}

// --------------------------------------------------------------------------
// Brain HTTP client
// --------------------------------------------------------------------------

interface BrainHealth { status: string }

interface IngestRequest {
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

interface IngestResponse {
  node_id: string;
  summary: string;
  tags: string[];
}

interface ContextPacketRequest {
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

interface ContextPacket {
  root_summary: string;
  insight: string;
  concerns: string[];
  packet_quality: number;
  llm_used: boolean;
  dependency_summaries: Record<string, string>;
}

interface SDLCConfig {
  phase: string;
  quality_mode: string;
}

function httpPost<T>(path: string, body: unknown): Promise<T> {
  return new Promise((resolve, reject) => {
    const base = brainUrl();
    const url = new URL(path, base);
    const payload = JSON.stringify(body);

    const req = http.request(
      {
        hostname: url.hostname,
        port: url.port || 80,
        path: url.pathname + url.search,
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Content-Length": Buffer.byteLength(payload),
        },
      },
      (res) => {
        let data = "";
        res.on("data", (chunk) => (data += chunk));
        res.on("end", () => {
          try {
            resolve(JSON.parse(data) as T);
          } catch {
            reject(new Error(`JSON parse error: ${data.slice(0, 200)}`));
          }
        });
      }
    );
    req.on("error", reject);
    req.setTimeout(10_000, () => {
      req.destroy();
      reject(new Error("request timeout"));
    });
    req.write(payload);
    req.end();
  });
}

function httpGet<T>(path: string): Promise<T> {
  return new Promise((resolve, reject) => {
    const base = brainUrl();
    const url = new URL(path, base);

    const req = http.request(
      {
        hostname: url.hostname,
        port: url.port || 80,
        path: url.pathname + url.search,
        method: "GET",
      },
      (res) => {
        let data = "";
        res.on("data", (chunk) => (data += chunk));
        res.on("end", () => {
          try {
            resolve(JSON.parse(data) as T);
          } catch {
            reject(new Error(`JSON parse error: ${data.slice(0, 200)}`));
          }
        });
      }
    );
    req.on("error", reject);
    req.setTimeout(3_000, () => {
      req.destroy();
      reject(new Error("request timeout"));
    });
    req.end();
  });
}

async function brainHealth(): Promise<boolean> {
  try {
    const h = await httpGet<BrainHealth>("/v1/health");
    return h.status === "ok";
  } catch {
    return false;
  }
}

// --------------------------------------------------------------------------
// Sidebar WebviewView Provider
// --------------------------------------------------------------------------

class SynapsesSidebarProvider implements vscode.WebviewViewProvider {
  public static readonly viewType = "synapses.sidebar";

  private _view?: vscode.WebviewView;
  private _lastPacket?: ContextPacket;
  private _sdlc?: SDLCConfig;
  private _graphStats?: string;

  constructor(private readonly _extensionUri: vscode.Uri) {}

  public resolveWebviewView(
    webviewView: vscode.WebviewView,
    _context: vscode.WebviewViewResolveContext,
    _token: vscode.CancellationToken
  ): void {
    this._view = webviewView;
    webviewView.webview.options = {
      enableScripts: true,
      localResourceRoots: [this._extensionUri],
    };
    this._render();

    webviewView.webview.onDidReceiveMessage((msg) => {
      if (msg.command === "refreshContext") {
        this.refreshForActiveEditor();
      } else if (msg.command === "injectContext") {
        cmdInjectContext(this._lastPacket);
      } else if (msg.command === "setPhase") {
        cmdSetPhase(msg.phase);
      }
    });

    // Auto-refresh when active editor changes
    vscode.window.onDidChangeActiveTextEditor(() => {
      this.refreshForActiveEditor();
    });
  }

  public async refreshForActiveEditor(): Promise<void> {
    const editor = vscode.window.activeTextEditor;
    if (!editor) return;

    const name = identifierAt(editor.document, editor.selection.active);
    if (!name) return;

    await this._refresh(name, repoRoot(editor.document), editor.document.uri.fsPath);
  }

  public async refreshForEntity(name: string, file: string): Promise<void> {
    const root = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? "";
    await this._refresh(name, root, file);
  }

  private async _refresh(name: string, root: string, file: string): Promise<void> {
    if (!this._view) return;

    // Load stats and SDLC in parallel
    const [stats, sdlc] = await Promise.allSettled([
      this._getGraphStats(root),
      httpGet<SDLCConfig>("/v1/sdlc"),
    ]);

    this._graphStats = stats.status === "fulfilled" ? stats.value : undefined;
    this._sdlc = sdlc.status === "fulfilled" ? sdlc.value : undefined;

    // Try to build context packet
    const nodeId = `${baseName(root)}::${baseName(file)}::${name}`;
    try {
      const packet = await httpPost<ContextPacket>("/v1/context-packet", {
        snapshot: {
          root_node_id: nodeId,
          root_name: name,
          root_type: "function",
          root_file: file,
        },
        enable_llm: false, // fast path — use cached summaries only
      } as ContextPacketRequest);
      this._lastPacket = packet;
    } catch {
      this._lastPacket = undefined;
    }

    this._render();
  }

  private async _getGraphStats(root: string): Promise<string> {
    const { stdout } = await execFileAsync(binaryPath(), [
      "status", "-path", root,
    ]);
    return stdout;
  }

  private _render(): void {
    if (!this._view) return;
    this._view.webview.html = this._buildHtml();
  }

  private _buildHtml(): string {
    const esc = (s: string) =>
      s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

    const phase = this._sdlc?.phase ?? "—";
    const mode = this._sdlc?.quality_mode ?? "—";
    const quality = this._lastPacket?.packet_quality ?? null;
    const qualityPct = quality !== null ? Math.round(quality * 100) : null;
    const qualityColor =
      quality === null ? "#666" :
      quality >= 0.9 ? "#4caf50" :
      quality >= 0.5 ? "#ff9800" : "#f44336";

    const summaryHtml = this._lastPacket?.root_summary
      ? `<p class="summary">${esc(this._lastPacket.root_summary)}</p>`
      : `<p class="muted">No summary — run brain ingest</p>`;

    const insightHtml = this._lastPacket?.insight
      ? `<p class="insight">${esc(this._lastPacket.insight)}</p>`
      : "";

    const concernsHtml = this._lastPacket?.concerns?.length
      ? `<ul class="concerns">${this._lastPacket.concerns.map((c) => `<li>${esc(c)}</li>`).join("")}</ul>`
      : "";

    const depCount = Object.keys(this._lastPacket?.dependency_summaries ?? {}).length;

    // Stats block (compact)
    const statsLines = (this._graphStats ?? "")
      .split("\n")
      .filter((l) => /Files|Functions|Methods|CALLS|IMPLEMENTS/.test(l))
      .map((l) => l.trim())
      .slice(0, 6);
    const statsHtml = statsLines.length
      ? `<pre class="stats">${statsLines.map(esc).join("\n")}</pre>`
      : "";

    return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  body {
    font-family: var(--vscode-font-family);
    font-size: var(--vscode-font-size);
    color: var(--vscode-foreground);
    padding: 8px 12px;
    margin: 0;
  }
  h3 { font-size: 0.9em; text-transform: uppercase; letter-spacing: 0.08em;
       color: var(--vscode-descriptionForeground); margin: 12px 0 4px; }
  .row { display: flex; justify-content: space-between; align-items: center;
         padding: 2px 0; }
  .label { color: var(--vscode-descriptionForeground); font-size: 0.88em; }
  .value { font-weight: 500; }
  .quality-bar {
    display: inline-block; width: 60px; height: 6px;
    background: var(--vscode-widget-border); border-radius: 3px; overflow: hidden;
    vertical-align: middle; margin-right: 4px;
  }
  .quality-fill { height: 100%; border-radius: 3px; }
  pre.stats {
    font-size: 0.82em; background: var(--vscode-textCodeBlock-background);
    padding: 6px 8px; border-radius: 4px; overflow-x: auto; margin: 4px 0;
    white-space: pre; color: var(--vscode-foreground);
  }
  p.summary { font-size: 0.9em; line-height: 1.4; margin: 4px 0; }
  p.insight { font-size: 0.88em; line-height: 1.4; margin: 4px 0;
              border-left: 2px solid var(--vscode-activityBarBadge-background);
              padding-left: 6px; }
  ul.concerns { font-size: 0.85em; margin: 4px 0; padding-left: 16px; }
  ul.concerns li { margin: 2px 0; }
  p.muted { color: var(--vscode-descriptionForeground); font-size: 0.85em; margin: 4px 0; }
  button {
    display: block; width: 100%; padding: 5px 8px; margin-top: 6px;
    background: var(--vscode-button-background); color: var(--vscode-button-foreground);
    border: none; border-radius: 3px; cursor: pointer; font-size: 0.88em;
  }
  button:hover { background: var(--vscode-button-hoverBackground); }
  button.secondary {
    background: var(--vscode-button-secondaryBackground);
    color: var(--vscode-button-secondaryForeground);
  }
  button.secondary:hover { background: var(--vscode-button-secondaryHoverBackground); }
  select {
    background: var(--vscode-dropdown-background); color: var(--vscode-dropdown-foreground);
    border: 1px solid var(--vscode-dropdown-border); border-radius: 3px;
    font-size: 0.85em; padding: 2px 4px;
  }
</style>
</head>
<body>

<h3>SDLC</h3>
<div class="row">
  <span class="label">Phase</span>
  <select id="phaseSelect" onchange="setPhase(this.value)">
    ${["planning","development","testing","review","deployment"].map(
      (p) => `<option value="${p}"${p === phase ? " selected" : ""}>${p}</option>`
    ).join("")}
  </select>
</div>
<div class="row">
  <span class="label">Mode</span>
  <span class="value">${esc(mode)}</span>
</div>

<h3>Context Quality</h3>
<div class="row">
  <span class="label">Packet quality</span>
  <span class="value">
    ${qualityPct !== null
      ? `<span class="quality-bar"><span class="quality-fill" style="width:${qualityPct}%;background:${qualityColor}"></span></span>${qualityPct}%`
      : "—"}
  </span>
</div>
${qualityPct !== null ? `<div class="row"><span class="label">Dep summaries</span><span class="value">${depCount}</span></div>` : ""}

<h3>Summary</h3>
${summaryHtml}

${insightHtml ? `<h3>Insight</h3>${insightHtml}` : ""}
${concernsHtml ? `<h3>Concerns</h3>${concernsHtml}` : ""}

<h3>Graph</h3>
${statsHtml || `<p class="muted">Run synapses index to build graph</p>`}

<button onclick="refreshContext()">↻ Refresh Context</button>
<button class="secondary" onclick="injectContext()">⇥ Copy Context to Clipboard</button>

<script>
  const vscode = acquireVsCodeApi();
  function refreshContext() { vscode.postMessage({ command: 'refreshContext' }); }
  function injectContext() { vscode.postMessage({ command: 'injectContext' }); }
  function setPhase(phase) { vscode.postMessage({ command: 'setPhase', phase }); }
</script>
</body>
</html>`;
  }
}

// --------------------------------------------------------------------------
// Auto-ingest on save
// --------------------------------------------------------------------------

async function autoIngestOnSave(document: vscode.TextDocument): Promise<void> {
  if (!vscode.workspace.getConfiguration("synapses").get<boolean>("autoIngest", true)) {
    return;
  }

  const alive = await brainHealth();
  if (!alive) return;

  const root = repoRoot(document);
  if (!root) return;

  const fileName = baseName(document.uri.fsPath);
  const relFile = document.uri.fsPath.replace(root + "/", "");
  const nodeId = `${baseName(root)}::${relFile}`;

  const body: IngestRequest = {
    node_id: nodeId,
    node_name: fileName,
    node_type: "file",
    file: relFile,
    signature: `file: ${relFile}`,
  };

  try {
    await httpPost<IngestResponse>("/v1/ingest", body);
  } catch {
    // auto-ingest is best-effort, never show errors for it
  }
}

// --------------------------------------------------------------------------
// Commands
// --------------------------------------------------------------------------

async function cmdInjectContext(packet?: ContextPacket): Promise<void> {
  if (!packet) {
    vscode.window.showWarningMessage("Synapses: no context packet — refresh first.");
    return;
  }

  const lines: string[] = [
    `[synapses context packet]`,
    `quality: ${Math.round(packet.packet_quality * 100)}%`,
  ];
  if (packet.root_summary) lines.push(`summary: ${packet.root_summary}`);
  if (packet.insight) lines.push(`insight: ${packet.insight}`);
  if (packet.concerns?.length) {
    lines.push(`concerns: ${packet.concerns.join("; ")}`);
  }
  const depCount = Object.keys(packet.dependency_summaries ?? {}).length;
  if (depCount > 0) lines.push(`dependencies: ${depCount} summaries loaded`);

  const text = lines.join("\n");
  await vscode.env.clipboard.writeText(text);
  vscode.window.showInformationMessage(
    `Synapses: context packet copied to clipboard (${text.length} chars)`
  );
}

async function cmdSetPhase(phase: string): Promise<void> {
  try {
    await httpPost("/v1/sdlc/phase", { phase });
    vscode.window.showInformationMessage(`Synapses: SDLC phase set to ${phase}`);
  } catch {
    vscode.window.showWarningMessage("Synapses: could not set SDLC phase — is brain running?");
  }
}

async function cmdReindex(): Promise<void> {
  const root = workspaceRoot();
  if (!root) {
    vscode.window.showWarningMessage("Synapses: no workspace folder open.");
    return;
  }

  vscode.window.withProgress(
    {
      location: vscode.ProgressLocation.Notification,
      title: "Synapses: re-indexing…",
      cancellable: false,
    },
    async () => {
      try {
        await execFileAsync(binaryPath(), ["index", "-path", root]);
        vscode.window.showInformationMessage("Synapses: re-index complete.");
      } catch (err: unknown) {
        const msg = err instanceof Error ? err.message : String(err);
        vscode.window.showErrorMessage(`Synapses: re-index failed: ${msg}`);
      }
    }
  );
}

async function cmdShowContextPanel(): Promise<void> {
  const editor = vscode.window.activeTextEditor;
  if (!editor) return;

  const name =
    identifierAt(editor.document, editor.selection.active) ??
    (await vscode.window.showInputBox({ prompt: "Entity name" }));
  if (!name) return;

  const root = repoRoot(editor.document);
  const nodeId = `${baseName(root)}::${baseName(editor.document.uri.fsPath)}::${name}`;

  let packet: ContextPacket | null = null;
  try {
    packet = await httpPost<ContextPacket>("/v1/context-packet", {
      snapshot: {
        root_node_id: nodeId,
        root_name: name,
        root_type: "function",
        root_file: editor.document.uri.fsPath,
      },
      enable_llm: true,
    } as ContextPacketRequest);
  } catch {
    vscode.window.showWarningMessage("Synapses: brain sidecar not responding.");
    return;
  }

  const panel = vscode.window.createWebviewPanel(
    "synapses.context",
    `Synapses: ${name}`,
    vscode.ViewColumn.Beside,
    {}
  );
  panel.webview.html = renderContextPanel(name, packet, editor.document.uri.fsPath);
}

// --------------------------------------------------------------------------
// Context panel webview
// --------------------------------------------------------------------------

function renderContextPanel(name: string, packet: ContextPacket, file: string): string {
  const esc = (s: string) =>
    s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

  const qualityPct = Math.round(packet.packet_quality * 100);
  const qualityColor = packet.packet_quality >= 0.9 ? "#4caf50" : packet.packet_quality >= 0.5 ? "#ff9800" : "#f44336";

  const depRows = Object.entries(packet.dependency_summaries ?? {})
    .map(([k, v]) => `<tr><td>${esc(k.split("::").pop() ?? k)}</td><td>${esc(v)}</td></tr>`)
    .join("");

  const concernItems = (packet.concerns ?? [])
    .map((c) => `<li>${esc(c)}</li>`)
    .join("");

  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  body { font-family: var(--vscode-font-family); font-size: var(--vscode-font-size);
         color: var(--vscode-foreground); padding: 20px; max-width: 900px; }
  h1 { font-size: 1.3em; margin-bottom: 4px; }
  .meta { color: var(--vscode-descriptionForeground); font-size: 0.85em; margin-bottom: 16px; }
  h2 { font-size: 1em; margin: 20px 0 6px; border-bottom: 1px solid var(--vscode-widget-border); padding-bottom: 4px; }
  .quality { display: flex; align-items: center; gap: 8px; }
  .quality-bar { width: 120px; height: 8px; background: var(--vscode-widget-border);
                  border-radius: 4px; overflow: hidden; }
  .quality-fill { height: 100%; border-radius: 4px; }
  .summary { line-height: 1.5; }
  .insight { line-height: 1.5; border-left: 3px solid var(--vscode-activityBarBadge-background);
              padding-left: 10px; margin: 8px 0; }
  ul { padding-left: 20px; }
  li { margin: 4px 0; line-height: 1.4; }
  table { border-collapse: collapse; width: 100%; margin-top: 8px; font-size: 0.9em; }
  th, td { text-align: left; padding: 6px 8px; border-bottom: 1px solid var(--vscode-widget-border); }
  th { font-weight: 600; }
  td:first-child { font-family: monospace; white-space: nowrap; min-width: 140px; }
  .badge { display: inline-block; background: var(--vscode-badge-background);
            color: var(--vscode-badge-foreground); padding: 1px 6px; border-radius: 3px;
            font-size: 0.8em; margin-left: 6px; }
</style>
</head>
<body>
<h1>${esc(name)} <span class="badge">context packet</span></h1>
<p class="meta">${esc(file)}</p>

<h2>Quality</h2>
<div class="quality">
  <div class="quality-bar">
    <div class="quality-fill" style="width:${qualityPct}%;background:${qualityColor}"></div>
  </div>
  <strong>${qualityPct}%</strong>
  ${packet.llm_used ? `<span class="badge">LLM enriched</span>` : ""}
</div>

${packet.root_summary ? `<h2>Summary</h2><p class="summary">${esc(packet.root_summary)}</p>` : ""}
${packet.insight ? `<h2>Insight</h2><div class="insight">${esc(packet.insight)}</div>` : ""}
${concernItems ? `<h2>Concerns</h2><ul>${concernItems}</ul>` : ""}
${depRows ? `<h2>Dependencies (${Object.keys(packet.dependency_summaries).length})</h2>
<table>
  <thead><tr><th>Node</th><th>Summary</th></tr></thead>
  <tbody>${depRows}</tbody>
</table>` : ""}

</body>
</html>`;
}

// --------------------------------------------------------------------------
// Hover provider (updated to use brain HTTP API for summaries)
// --------------------------------------------------------------------------

class SynapsesHoverProvider implements vscode.HoverProvider {
  private cache = new Map<string, { summary: string; ts: number }>();
  private readonly ttlMs = 60_000;

  async provideHover(
    document: vscode.TextDocument,
    position: vscode.Position,
    token: vscode.CancellationToken
  ): Promise<vscode.Hover | null> {
    if (!vscode.workspace.getConfiguration("synapses").get<boolean>("enableHover", true)) {
      return null;
    }

    const name = identifierAt(document, position);
    if (!name || name.length < 2) return null;

    const root = repoRoot(document);
    if (!root) return null;
    if (token.isCancellationRequested) return null;

    const nodeId = `${baseName(root)}::${baseName(document.uri.fsPath)}::${name}`;
    const cacheKey = nodeId;
    const cached = this.cache.get(cacheKey);
    if (cached && Date.now() - cached.ts < this.ttlMs) {
      return this._buildHover(name, cached.summary, document.uri.fsPath);
    }

    try {
      const result = await httpGet<{ summary: string }>(`/v1/summary/${encodeURIComponent(nodeId)}`);
      if (token.isCancellationRequested) return null;
      if (result.summary) {
        this.cache.set(cacheKey, { summary: result.summary, ts: Date.now() });
        return this._buildHover(name, result.summary, document.uri.fsPath);
      }
    } catch {
      // brain not running — no hover
    }
    return null;
  }

  private _buildHover(name: string, summary: string, file: string): vscode.Hover {
    const md = new vscode.MarkdownString("", true);
    md.isTrusted = true;
    md.appendMarkdown(`**${name}** — *${inferLanguage(file)}*\n\n`);
    md.appendMarkdown(summary);
    md.appendMarkdown("\n\n---\n*Synapses*");
    return new vscode.Hover(md);
  }
}

// --------------------------------------------------------------------------
// Extension lifecycle
// --------------------------------------------------------------------------

export function activate(context: vscode.ExtensionContext): void {
  const sidebarProvider = new SynapsesSidebarProvider(context.extensionUri);

  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider(
      SynapsesSidebarProvider.viewType,
      sidebarProvider,
      { webviewOptions: { retainContextWhenHidden: true } }
    )
  );

  // Hover provider for all supported languages
  const supportedLanguages = [
    "go", "typescript", "typescriptreact", "javascript", "javascriptreact",
    "python", "rust", "java", "kotlin", "csharp", "ruby", "php", "swift", "scala",
  ];
  const hoverProvider = new SynapsesHoverProvider();
  for (const lang of supportedLanguages) {
    context.subscriptions.push(
      vscode.languages.registerHoverProvider(lang, hoverProvider)
    );
  }

  // Auto-ingest on save
  context.subscriptions.push(
    vscode.workspace.onDidSaveTextDocument((doc) => {
      autoIngestOnSave(doc);
      // Also refresh sidebar if visible
      sidebarProvider.refreshForEntity(
        baseName(doc.uri.fsPath).replace(/\.[^.]+$/, ""),
        doc.uri.fsPath
      );
    })
  );

  // Commands
  context.subscriptions.push(
    vscode.commands.registerCommand("synapses.showContext", cmdShowContextPanel),
    vscode.commands.registerCommand("synapses.reindex", cmdReindex),
    vscode.commands.registerCommand("synapses.injectContext", () =>
      sidebarProvider.refreshForActiveEditor().then(() =>
        cmdInjectContext((sidebarProvider as any)._lastPacket)
      )
    )
  );
}

export function deactivate(): void {}
