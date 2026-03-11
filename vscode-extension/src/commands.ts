import * as vscode from 'vscode';
import { execFile } from 'child_process';
import { promisify } from 'util';
import { httpPost } from './http';
import * as cfg from './config';
import { deregisterProject, toggleSidecarConfig } from './services/deregister';
import { OllamaService } from './services/ollama';
import { SynapsesSidebarProvider } from './views/sidebar';
import { PulseDashboardPanel } from './views/pulse-panel';
import { GraphExplorerPanel } from './views/graph-panel';
import { ContextPacket, ServiceId } from './types';

const execFileAsync = promisify(execFile);

// ---------------------------------------------------------------------------
// Existing commands (preserved from original)
// ---------------------------------------------------------------------------

export async function cmdReindex(): Promise<void> {
  const root = cfg.workspaceRoot();
  if (!root) {
    vscode.window.showWarningMessage('Synapses: no workspace folder open.');
    return;
  }
  vscode.window.withProgress(
    { location: vscode.ProgressLocation.Notification, title: 'Synapses: re-indexing…', cancellable: false },
    async () => {
      try {
        await execFileAsync(cfg.binaryPath(), ['index', '-path', root]);
        vscode.window.showInformationMessage('Synapses: re-index complete.');
      } catch (err: unknown) {
        vscode.window.showErrorMessage(`Synapses: re-index failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }
  );
}

export async function cmdInjectContext(getPacket: () => ContextPacket | undefined): Promise<void> {
  const packet = getPacket();
  if (!packet) {
    vscode.window.showWarningMessage('Synapses: no context packet — refresh first.');
    return;
  }

  const lines: string[] = [`[synapses context packet]`, `quality: ${Math.round(packet.packet_quality * 100)}%`];
  if (packet.root_summary) lines.push(`summary: ${packet.root_summary}`);
  if (packet.insight) lines.push(`insight: ${packet.insight}`);
  if (packet.concerns?.length) lines.push(`concerns: ${packet.concerns.join('; ')}`);
  const depCount = Object.keys(packet.dependency_summaries ?? {}).length;
  if (depCount > 0) lines.push(`dependencies: ${depCount} summaries loaded`);

  const text = lines.join('\n');
  await vscode.env.clipboard.writeText(text);
  vscode.window.showInformationMessage(`Synapses: context packet copied to clipboard (${text.length} chars)`);
}

export async function cmdSetPhase(phase: string): Promise<void> {
  try {
    await httpPost<unknown>(cfg.intelligenceUrl(), '/v1/sdlc/phase', { phase });
    vscode.window.showInformationMessage(`Synapses: SDLC phase set to ${phase}`);
  } catch {
    vscode.window.showWarningMessage('Synapses: could not set SDLC phase — is brain running?');
  }
}

// ---------------------------------------------------------------------------
// New commands
// ---------------------------------------------------------------------------

export function cmdShowPulseDashboard(extensionUri: vscode.Uri): void {
  PulseDashboardPanel.show(extensionUri);
}

export async function cmdDeregisterProject(): Promise<void> {
  const root = cfg.workspaceRoot();
  if (!root) {
    vscode.window.showWarningMessage('Synapses: no workspace folder open.');
    return;
  }
  await deregisterProject(root);
  vscode.window.showInformationMessage(`Synapses: deregistered from ${root.split('/').pop()}`);
}

export async function cmdRegisterProject(): Promise<void> {
  const root = cfg.workspaceRoot();
  if (!root) {
    vscode.window.showWarningMessage('Synapses: no workspace folder open.');
    return;
  }
  vscode.window.withProgress(
    { location: vscode.ProgressLocation.Notification, title: 'Synapses: registering project…', cancellable: false },
    async () => {
      try {
        await execFileAsync(cfg.binaryPath(), ['init', '-path', root]);
        await execFileAsync(cfg.binaryPath(), ['index', '-path', root]);
        vscode.window.showInformationMessage('Synapses: project registered and indexed.');
      } catch (err: unknown) {
        vscode.window.showErrorMessage(`Synapses: registration failed: ${err instanceof Error ? err.message : String(err)}`);
      }
    }
  );
}

// Maps ServiceId to the daemon --service name
const DAEMON_NAME: Partial<Record<ServiceId, string>> = {
  intelligence: 'brain',
  scout: 'scout',
  pulse: 'pulse',
};

export async function cmdToggleSidecar(sidecar: ServiceId, enabled: boolean): Promise<void> {
  const root = cfg.workspaceRoot();
  if (!root) {
    vscode.window.showWarningMessage('Synapses: no workspace folder open.');
    return;
  }

  // Attempt daemon start/stop first — only update config on success
  const serviceName = DAEMON_NAME[sidecar];
  if (serviceName) {
    const sub = enabled ? 'start' : 'stop';
    try {
      await execFileAsync(cfg.binaryPath(), ['daemon', sub, '--service', serviceName, '--quiet']);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      vscode.window.showWarningMessage(`Synapses: failed to ${sub} ${sidecar}: ${msg}`);
      return; // Don't update config if daemon operation failed
    }
  }

  // Only update synapses.json after daemon operation succeeds
  toggleSidecarConfig(root, sidecar, enabled);

  const action = enabled ? 'enabled and started' : 'disabled and stopped';
  vscode.window.showInformationMessage(`Synapses: ${sidecar} ${action}.`);
}

export async function cmdPullModel(ollamaService: OllamaService): Promise<void> {
  const model = await vscode.window.showInputBox({
    prompt: 'Ollama model name to pull',
    value: cfg.defaultModel(),
    placeHolder: 'qwen2.5-coder:1.5b',
  });
  if (!model) return;

  await vscode.window.withProgress(
    { location: vscode.ProgressLocation.Notification, title: `Pulling ${model}…`, cancellable: false },
    async (progress) => {
      await ollamaService.pullModel(model, (pct, status) => {
        progress.report({ message: `${status} (${pct}%)`, increment: 0 });
      });
    }
  );
  vscode.window.showInformationMessage(`Synapses: ${model} pulled successfully.`);
}

export async function cmdRunDoctor(): Promise<void> {
  const root = cfg.workspaceRoot();
  const panel = vscode.window.createWebviewPanel(
    'synapses.doctor',
    'Synapses: Diagnostics',
    vscode.ViewColumn.Beside,
    {}
  );

  panel.webview.html = `<pre style="font-family:monospace;padding:16px;color:#ccc;background:#1e1e1e">Running diagnostics…</pre>`;

  try {
    const args = ['doctor'];
    if (root) args.push('-path', root);
    const { stdout, stderr } = await execFileAsync(cfg.binaryPath(), args, { timeout: 15000 });
    const output = (stdout + stderr).trim() || '(no output)';
    panel.webview.html = `<pre style="font-family:monospace;padding:16px;color:#ccc;background:#1e1e1e;white-space:pre-wrap">${output
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')}</pre>`;
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    panel.webview.html = `<pre style="font-family:monospace;padding:16px;color:#f44336;background:#1e1e1e;white-space:pre-wrap">Error: ${msg}</pre>`;
  }
}

export function cmdShowGraphExplorer(extensionUri: vscode.Uri): void {
  GraphExplorerPanel.show(extensionUri);
}

export function cmdOpenSettings(): void {
  vscode.commands.executeCommand('workbench.action.openSettings', 'synapses');
}

export async function cmdStartOllama(): Promise<void> {
  // On macOS/Linux, start ollama via the CLI
  try {
    execFile('ollama', ['serve'], (err) => {
      if (err && !err.message.includes('already')) {
        vscode.window.showWarningMessage(`Synapses: could not start Ollama: ${err.message}`);
      }
    });
    vscode.window.showInformationMessage('Synapses: starting Ollama server…');
  } catch {
    vscode.window.showWarningMessage('Synapses: could not start Ollama. Please start it manually.');
  }
}

// ---------------------------------------------------------------------------
// Register all commands
// ---------------------------------------------------------------------------

export function registerCommands(
  context: vscode.ExtensionContext,
  sidebar: SynapsesSidebarProvider,
  ollamaService: OllamaService
): void {
  context.subscriptions.push(
    vscode.commands.registerCommand('synapses.showContext', () =>
      sidebar.refreshForActiveEditor()
    ),
    vscode.commands.registerCommand('synapses.reindex', cmdReindex),
    vscode.commands.registerCommand('synapses.injectContext', () =>
      cmdInjectContext(() => sidebar.getLastPacket())
    ),
    vscode.commands.registerCommand('synapses.showPulse', () =>
      cmdShowPulseDashboard(context.extensionUri)
    ),
    vscode.commands.registerCommand('synapses.deregisterProject', cmdDeregisterProject),
    vscode.commands.registerCommand('synapses.registerProject', cmdRegisterProject),
    vscode.commands.registerCommand('synapses.toggleSidecar',
      (sidecar: ServiceId, enabled: boolean) => cmdToggleSidecar(sidecar, enabled)
    ),
    vscode.commands.registerCommand('synapses.pullModel', () => cmdPullModel(ollamaService)),
    vscode.commands.registerCommand('synapses.showGraphExplorer', () =>
      cmdShowGraphExplorer(context.extensionUri)
    ),
    vscode.commands.registerCommand('synapses.runDoctor', cmdRunDoctor),
    vscode.commands.registerCommand('synapses.openSettings', cmdOpenSettings),
    vscode.commands.registerCommand('synapses.startOllama', cmdStartOllama),
  );
}
