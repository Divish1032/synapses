import * as vscode from 'vscode';
import { execFile } from 'child_process';
import { HealthPoller } from './services/health';
import { OllamaService } from './services/ollama';
import { SynapsesSidebarProvider } from './views/sidebar';
import { StatusBarManager } from './providers/status-bar';
import { SynapsesHoverProvider, HOVER_LANGUAGES } from './providers/hover';
import { registerCommands } from './commands';
import * as cfg from './config';
import { httpPost } from './http';
import { IngestRequest, IngestResponse } from './types';

export function activate(context: vscode.ExtensionContext): void {
  // 0. Silently ensure sidecars are running (idempotent — already running = no-op)
  execFile(cfg.binaryPath(), ['daemon', 'start', '--quiet'], () => {/* fire-and-forget */});

  // 1. Create services
  const healthPoller = new HealthPoller();
  const ollamaService = new OllamaService();

  // 2. Create providers
  const sidebar = new SynapsesSidebarProvider(context.extensionUri, healthPoller, ollamaService, context);
  const statusBar = new StatusBarManager();
  const hoverProvider = new SynapsesHoverProvider();

  // 3. Wire health updates → sidebar + status bar
  healthPoller.on('update', (state) => {
    sidebar.updateHealth(state);
    statusBar.update(state);
  });

  // 4. Register sidebar webview
  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider(
      SynapsesSidebarProvider.viewType,
      sidebar,
      { webviewOptions: { retainContextWhenHidden: true } }
    )
  );

  // 5. Register hover providers for all supported languages
  for (const lang of HOVER_LANGUAGES) {
    context.subscriptions.push(
      vscode.languages.registerHoverProvider(lang, hoverProvider)
    );
  }

  // 6. Register all commands
  registerCommands(context, sidebar, ollamaService);

  // 7. Auto-ingest on save
  context.subscriptions.push(
    vscode.workspace.onDidSaveTextDocument((doc) => {
      autoIngestOnSave(doc);
      const name = doc.uri.fsPath.split('/').pop()?.replace(/\.[^.]+$/, '') ?? '';
      sidebar.refreshForEntity(name, doc.uri.fsPath);
    })
  );

  // 8. Add disposables
  context.subscriptions.push(statusBar, {
    dispose: () => healthPoller.stop(),
  });

  // 9. Start health polling
  healthPoller.start(cfg.healthPollSec() * 1000);
}

// deactivate: sidecars are intentionally left running.
// They are system-level services managed by `synapses daemon`.
// Use `synapses daemon stop` or the VS Code toggle switches to shut them down.
export function deactivate(): void {}

// ---------------------------------------------------------------------------
// Auto-ingest helper
// ---------------------------------------------------------------------------

async function autoIngestOnSave(document: vscode.TextDocument): Promise<void> {
  if (!cfg.autoIngest()) return;

  const root = cfg.repoRoot(document);
  if (!root) return;

  const relFile = document.uri.fsPath.replace(root + '/', '');
  const baseName = (p: string) => p.split('/').pop() ?? p;
  const fileName = baseName(document.uri.fsPath);
  const nodeId = `${baseName(root)}::${relFile}`;

  const body: IngestRequest = {
    node_id: nodeId,
    node_name: fileName,
    node_type: 'file',
    file: relFile,
    signature: `file: ${relFile}`,
  };

  try {
    await httpPost<IngestResponse>(cfg.intelligenceUrl(), '/v1/ingest', body, 5000);
  } catch {
    // auto-ingest is best-effort — never show errors
  }
}
