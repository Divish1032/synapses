import * as vscode from "vscode";
import { execFile } from "child_process";
import { promisify } from "util";

const execFileAsync = promisify(execFile);

// --------------------------------------------------------------------------
// Types matching `synapses query` JSON output
// --------------------------------------------------------------------------
interface EdgeRef {
  name: string;
  file: string;
  line: number;
  type: string;
}

interface QueryResult {
  name: string;
  type: string;
  file: string;
  line: number;
  doc?: string;
  signature?: string;
  callers: EdgeRef[];
  callees: EdgeRef[];
  metadata?: Record<string, string>;
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

function binaryPath(): string {
  return (
    vscode.workspace
      .getConfiguration("synapses")
      .get<string>("binaryPath") || "synapses"
  );
}

function repoRoot(document: vscode.TextDocument): string {
  const ws = vscode.workspace.getWorkspaceFolder(document.uri);
  return ws?.uri.fsPath ?? "";
}

/** Run `synapses query -path <root> -entity <name>` and return parsed JSON. */
async function queryEntity(
  root: string,
  name: string
): Promise<QueryResult | null> {
  try {
    const { stdout } = await execFileAsync(binaryPath(), [
      "query",
      "-path",
      root,
      "-entity",
      name,
    ]);
    return JSON.parse(stdout) as QueryResult;
  } catch {
    return null;
  }
}

/** Extract the identifier under the cursor from the document. */
function identifierAt(
  document: vscode.TextDocument,
  position: vscode.Position
): string | null {
  const range = document.getWordRangeAtPosition(
    position,
    /[A-Za-z_][A-Za-z0-9_]*/
  );
  if (!range) return null;
  return document.getText(range);
}

// --------------------------------------------------------------------------
// Markdown rendering
// --------------------------------------------------------------------------

function renderHover(result: QueryResult): vscode.MarkdownString {
  const md = new vscode.MarkdownString("", true);
  md.isTrusted = true;
  md.supportHtml = false;

  // Header
  md.appendMarkdown(`**${result.name}** \`${result.type}\`\n\n`);

  // Signature / doc
  if (result.signature) {
    md.appendCodeblock(result.signature, inferLanguage(result.file));
  }
  if (result.doc) {
    md.appendMarkdown(`${result.doc}\n\n`);
  }

  // Location
  const relFile = result.file.replace(/.*\/([^/]+\/[^/]+)$/, "$1");
  md.appendMarkdown(`*${relFile}:${result.line}*\n\n`);

  // Callers
  if (result.callers.length > 0) {
    md.appendMarkdown(`**Callers** (${result.callers.length})\n\n`);
    for (const c of result.callers.slice(0, 5)) {
      md.appendMarkdown(`- \`${c.name}\` *${baseName(c.file)}:${c.line}*\n`);
    }
    if (result.callers.length > 5) {
      md.appendMarkdown(`- *…${result.callers.length - 5} more*\n`);
    }
    md.appendMarkdown("\n");
  }

  // Callees
  if (result.callees.length > 0) {
    md.appendMarkdown(`**Calls** (${result.callees.length})\n\n`);
    for (const c of result.callees.slice(0, 5)) {
      md.appendMarkdown(`- \`${c.name}\` *${baseName(c.file)}:${c.line}*\n`);
    }
    if (result.callees.length > 5) {
      md.appendMarkdown(`- *…${result.callees.length - 5} more*\n`);
    }
    md.appendMarkdown("\n");
  }

  // Metadata badges (churn, complexity, coverage, cpu_pct)
  const meta = result.metadata ?? {};
  const badges: string[] = [];
  if (meta["churn"]) badges.push(`churn: ${meta["churn"]}`);
  if (meta["complexity"]) badges.push(`complexity: ${meta["complexity"]}`);
  if (meta["coverage"]) {
    const pct = Math.round(parseFloat(meta["coverage"]) * 100);
    badges.push(`coverage: ${pct}%`);
  }
  if (meta["cpu_pct"]) badges.push(`cpu: ${meta["cpu_pct"]}%`);
  if (badges.length > 0) {
    md.appendMarkdown(`*${badges.join(" · ")}*\n\n`);
  }

  md.appendMarkdown("---\n*Synapses*");
  return md;
}

function baseName(filePath: string): string {
  return filePath.split("/").pop() ?? filePath;
}

function inferLanguage(filePath: string): string {
  const ext = filePath.split(".").pop() ?? "";
  const map: Record<string, string> = {
    go: "go",
    ts: "typescript",
    tsx: "typescript",
    js: "javascript",
    jsx: "javascript",
    py: "python",
    rs: "rust",
    java: "java",
    kt: "kotlin",
    cs: "csharp",
    rb: "ruby",
    php: "php",
    swift: "swift",
    scala: "scala",
  };
  return map[ext] ?? "plaintext";
}

// --------------------------------------------------------------------------
// Hover provider
// --------------------------------------------------------------------------

class SynapsesHoverProvider implements vscode.HoverProvider {
  private cache = new Map<string, { result: QueryResult; ts: number }>();
  private readonly ttlMs = 30_000;

  async provideHover(
    document: vscode.TextDocument,
    position: vscode.Position,
    token: vscode.CancellationToken
  ): Promise<vscode.Hover | null> {
    if (
      !vscode.workspace
        .getConfiguration("synapses")
        .get<boolean>("enableHover", true)
    ) {
      return null;
    }

    const name = identifierAt(document, position);
    if (!name || name.length < 2) return null;

    const root = repoRoot(document);
    if (!root) return null;

    if (token.isCancellationRequested) return null;

    const cacheKey = `${root}::${name}`;
    const cached = this.cache.get(cacheKey);
    if (cached && Date.now() - cached.ts < this.ttlMs) {
      return new vscode.Hover(renderHover(cached.result));
    }

    const result = await queryEntity(root, name);
    if (!result || token.isCancellationRequested) return null;

    this.cache.set(cacheKey, { result, ts: Date.now() });
    return new vscode.Hover(renderHover(result));
  }
}

// --------------------------------------------------------------------------
// Commands
// --------------------------------------------------------------------------

async function cmdShowEntity(): Promise<void> {
  const editor = vscode.window.activeTextEditor;
  if (!editor) return;

  const name = await vscode.window.showInputBox({
    prompt: "Entity name to look up",
    value: identifierAt(editor.document, editor.selection.active) ?? "",
  });
  if (!name) return;

  const root = repoRoot(editor.document);
  const result = await queryEntity(root, name);
  if (!result) {
    vscode.window.showWarningMessage(
      `Synapses: entity "${name}" not found in index.`
    );
    return;
  }

  const panel = vscode.window.createWebviewPanel(
    "synapses.entity",
    `Synapses: ${result.name}`,
    vscode.ViewColumn.Beside,
    {}
  );
  panel.webview.html = renderWebview(result);
}

async function cmdReindex(): Promise<void> {
  const folders = vscode.workspace.workspaceFolders;
  if (!folders?.length) {
    vscode.window.showWarningMessage("Synapses: no workspace folder open.");
    return;
  }
  const root = folders[0].uri.fsPath;

  vscode.window.withProgress(
    {
      location: vscode.ProgressLocation.Notification,
      title: "Synapses: re-indexing…",
      cancellable: false,
    },
    async () => {
      try {
        await execFileAsync(binaryPath(), ["index", "-path", root, "-reindex"]);
        vscode.window.showInformationMessage("Synapses: re-index complete.");
      } catch (err: unknown) {
        const msg = err instanceof Error ? err.message : String(err);
        vscode.window.showErrorMessage(`Synapses: re-index failed: ${msg}`);
      }
    }
  );
}

// --------------------------------------------------------------------------
// Webview rendering (for cmdShowEntity panel)
// --------------------------------------------------------------------------

function renderWebview(result: QueryResult): string {
  const esc = (s: string) =>
    s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

  const callerRows = result.callers
    .map(
      (c) =>
        `<tr><td>${esc(c.name)}</td><td>${esc(c.type)}</td><td>${esc(c.file.split("/").pop() ?? "")}:${c.line}</td></tr>`
    )
    .join("");

  const calleeRows = result.callees
    .map(
      (c) =>
        `<tr><td>${esc(c.name)}</td><td>${esc(c.type)}</td><td>${esc(c.file.split("/").pop() ?? "")}:${c.line}</td></tr>`
    )
    .join("");

  const metaRows = Object.entries(result.metadata ?? {})
    .map(([k, v]) => `<tr><td>${esc(k)}</td><td>${esc(v)}</td></tr>`)
    .join("");

  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>${esc(result.name)}</title>
<style>
  body { font-family: var(--vscode-font-family); font-size: var(--vscode-font-size); color: var(--vscode-foreground); padding: 16px; }
  h1 { font-size: 1.2em; margin-bottom: 4px; }
  .badge { display: inline-block; background: var(--vscode-badge-background); color: var(--vscode-badge-foreground); border-radius: 3px; padding: 1px 6px; font-size: 0.85em; margin-left: 6px; }
  pre { background: var(--vscode-textCodeBlock-background); padding: 8px; border-radius: 4px; overflow-x: auto; }
  table { border-collapse: collapse; width: 100%; margin-top: 8px; }
  th, td { text-align: left; padding: 4px 8px; border-bottom: 1px solid var(--vscode-widget-border); }
  th { font-weight: 600; }
  h2 { font-size: 1em; margin: 16px 0 4px; }
  .loc { font-size: 0.85em; color: var(--vscode-descriptionForeground); }
</style>
</head>
<body>
<h1>${esc(result.name)}<span class="badge">${esc(result.type)}</span></h1>
<p class="loc">${esc(result.file)}:${result.line}</p>
${result.signature ? `<pre>${esc(result.signature)}</pre>` : ""}
${result.doc ? `<p>${esc(result.doc)}</p>` : ""}
${
  result.callers.length
    ? `<h2>Callers (${result.callers.length})</h2>
<table><thead><tr><th>Name</th><th>Type</th><th>Location</th></tr></thead>
<tbody>${callerRows}</tbody></table>`
    : ""
}
${
  result.callees.length
    ? `<h2>Calls (${result.callees.length})</h2>
<table><thead><tr><th>Name</th><th>Type</th><th>Location</th></tr></thead>
<tbody>${calleeRows}</tbody></table>`
    : ""
}
${
  metaRows
    ? `<h2>Metadata</h2>
<table><tbody>${metaRows}</tbody></table>`
    : ""
}
</body>
</html>`;
}

// --------------------------------------------------------------------------
// Extension lifecycle
// --------------------------------------------------------------------------

export function activate(context: vscode.ExtensionContext): void {
  const supportedLanguages = [
    "go",
    "typescript",
    "typescriptreact",
    "javascript",
    "javascriptreact",
    "python",
    "rust",
    "java",
    "kotlin",
    "csharp",
    "ruby",
    "php",
    "swift",
    "scala",
  ];

  const hoverProvider = new SynapsesHoverProvider();
  for (const lang of supportedLanguages) {
    context.subscriptions.push(
      vscode.languages.registerHoverProvider(lang, hoverProvider)
    );
  }

  context.subscriptions.push(
    vscode.commands.registerCommand("synapses.showEntity", cmdShowEntity),
    vscode.commands.registerCommand("synapses.reindex", cmdReindex)
  );
}

export function deactivate(): void {}
