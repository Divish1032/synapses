import * as vscode from 'vscode';
import { execFile } from 'child_process';
import { promisify } from 'util';
import { HealthPoller } from '../services/health';
import { OllamaService } from '../services/ollama';
import { buildSidebarHtml } from './sidebar-html';
import { httpGet, httpPost } from '../http';
import * as cfg from '../config';
import {
  HealthState, SidebarState, ContextPacket, SDLCConfig,
  OllamaStatus, OllamaModel, ServiceId, PulseSummary, PulseTimelinePoint,
} from '../types';

const execFileAsync = promisify(execFile);

function baseName(p: string): string {
  return p.split('/').pop() ?? p;
}

function identifierAt(doc: vscode.TextDocument, pos: vscode.Position): string | null {
  const range = doc.getWordRangeAtPosition(pos, /[A-Za-z_][A-Za-z0-9_]*/);
  return range ? doc.getText(range) : null;
}

export class SynapsesSidebarProvider implements vscode.WebviewViewProvider {
  public static readonly viewType = 'synapses.sidebar';

  private _view?: vscode.WebviewView;
  private _state: SidebarState;

  constructor(
    private readonly _extensionUri: vscode.Uri,
    private readonly _healthPoller: HealthPoller,
    private readonly _ollamaService: OllamaService
  ) {
    this._state = {
      health: _healthPoller.getState(),
      ollamaStatus: 'stopped',
      ollamaModels: [],
      defaultModel: cfg.defaultModel(),
    };
  }

  public resolveWebviewView(
    webviewView: vscode.WebviewView,
    _ctx: vscode.WebviewViewResolveContext,
    _token: vscode.CancellationToken
  ): void {
    this._view = webviewView;
    webviewView.webview.options = {
      enableScripts: true,
      localResourceRoots: [this._extensionUri],
    };

    this._render();
    this._refreshOllama();
    this._refreshPulse();

    webviewView.webview.onDidReceiveMessage((msg) => this._handleMessage(msg));

    vscode.window.onDidChangeActiveTextEditor(() => {
      this.refreshForActiveEditor();
    });
  }

  public updateHealth(state: HealthState): void {
    this._state = { ...this._state, health: state };
    this._refreshPulse();
    this._render();
  }

  public async refreshForActiveEditor(): Promise<void> {
    const editor = vscode.window.activeTextEditor;
    if (!editor) return;
    const name = identifierAt(editor.document, editor.selection.active);
    if (!name) return;
    await this._refreshContext(name, editor.document);
  }

  public async refreshForEntity(name: string, file: string): Promise<void> {
    const doc = vscode.workspace.textDocuments.find((d) => d.uri.fsPath === file);
    if (doc) await this._refreshContext(name, doc);
  }

  private async _handleMessage(msg: { command: string; [key: string]: unknown }): Promise<void> {
    switch (msg.command) {
      case 'toggleService':
        await this._toggleService(msg.service as ServiceId, msg.enabled as boolean);
        break;
      case 'refreshHealth':
        await this._healthPoller.pollOnce();
        break;
      case 'openPulseDashboard':
        await vscode.commands.executeCommand('synapses.showPulse');
        break;
      case 'setDefaultModel': {
        const model = msg.model as string;
        await vscode.workspace
          .getConfiguration('synapses')
          .update('defaultModel', model, vscode.ConfigurationTarget.Global);
        this._state = { ...this._state, defaultModel: model };
        this._render();
        break;
      }
      case 'pullModel': {
        const model = this._state.defaultModel;
        this._state = { ...this._state, modelPullProgress: { model, pct: 0, status: 'Starting…' } };
        this._render();
        await this._ollamaService.pullModel(model, (pct, status) => {
          this._state = { ...this._state, modelPullProgress: { model, pct, status } };
          this._render();
        });
        this._state = { ...this._state, modelPullProgress: undefined };
        await this._refreshOllama();
        break;
      }
      case 'deleteModel':
        await this._ollamaService.deleteModel(msg.model as string);
        await this._refreshOllama();
        break;
      case 'openExternal':
        await vscode.env.openExternal(vscode.Uri.parse(msg.url as string));
        break;
      case 'startOllama':
        await vscode.commands.executeCommand('synapses.startOllama');
        break;
      case 'refreshContext':
        await this.refreshForActiveEditor();
        break;
      case 'injectContext':
        await vscode.commands.executeCommand('synapses.injectContext');
        break;
      case 'setPhase':
        await this._setPhase(msg.phase as string);
        break;
    }
  }

  private async _toggleService(id: ServiceId, enabled: boolean): Promise<void> {
    if (id === 'core' && !enabled) {
      const answer = await vscode.window.showWarningMessage(
        'This will deregister Synapses from this project. .mcp.json will be removed and CLAUDE.md will be cleaned. Continue?',
        { modal: true },
        'Deregister'
      );
      if (answer !== 'Deregister') return;
      await vscode.commands.executeCommand('synapses.deregisterProject');
    } else if (id === 'core' && enabled) {
      await vscode.commands.executeCommand('synapses.registerProject');
    } else {
      await vscode.commands.executeCommand('synapses.toggleSidecar', id, enabled);
    }
  }

  private async _refreshOllama(): Promise<void> {
    const [status, models] = await Promise.allSettled([
      this._ollamaService.getStatus(),
      this._ollamaService.listModels(),
    ]);
    this._state = {
      ...this._state,
      ollamaStatus: status.status === 'fulfilled' ? status.value : ('stopped' as OllamaStatus),
      ollamaModels: models.status === 'fulfilled' ? models.value : [] as OllamaModel[],
    };
    this._render();
  }

  private async _refreshPulse(): Promise<void> {
    if (this._state.health.pulse?.status !== 'online') {
      if (this._state.pulse) {
        this._state = { ...this._state, pulse: undefined, pulseTrend: undefined };
        this._render();
      }
      return;
    }
    try {
      const summary = await httpGet<PulseSummary>(cfg.pulseUrl(), '/v1/summary?days=7', 3000);
      const timeline = await httpGet<{ points: PulseTimelinePoint[] }>(cfg.pulseUrl(), '/v1/timeline?days=7&granularity=daily', 3000);
      this._state = {
        ...this._state,
        pulse: summary,
        pulseTrend: timeline.points ?? [],
      };
      this._render();
    } catch {
      // pulse unreachable — leave existing stats
    }
  }

  private async _refreshContext(name: string, doc: vscode.TextDocument): Promise<void> {
    const root = cfg.repoRoot(doc);
    if (!root || this._state.health.intelligence?.status !== 'online') return;

    const [statsResult, sdlcResult] = await Promise.allSettled([
      execFileAsync(cfg.binaryPath(), ['status', '-path', root]),
      httpGet<SDLCConfig>(cfg.intelligenceUrl(), '/v1/sdlc', 3000),
    ]);

    this._state = {
      ...this._state,
      graphStats: statsResult.status === 'fulfilled' ? statsResult.value.stdout : undefined,
      sdlc: sdlcResult.status === 'fulfilled' ? sdlcResult.value : undefined,
    };

    const nodeId = `${baseName(root)}::${baseName(doc.uri.fsPath)}::${name}`;
    try {
      const packet = await httpPost<ContextPacket>(cfg.intelligenceUrl(), '/v1/context-packet', {
        snapshot: { root_node_id: nodeId, root_name: name, root_type: 'function', root_file: doc.uri.fsPath },
        enable_llm: false,
      }, 5000);
      this._state = { ...this._state, contextPacket: packet };
    } catch {
      this._state = { ...this._state, contextPacket: undefined };
    }

    this._render();
  }

  private async _setPhase(phase: string): Promise<void> {
    try {
      await httpPost<unknown>(cfg.intelligenceUrl(), '/v1/sdlc/phase', { phase }, 3000);
      vscode.window.showInformationMessage(`Synapses: SDLC phase set to ${phase}`);
    } catch {
      vscode.window.showWarningMessage('Synapses: could not set SDLC phase — is brain running?');
    }
  }

  private _render(): void {
    if (!this._view) return;
    this._view.webview.html = buildSidebarHtml(this._state);
  }

  public getLastPacket(): ContextPacket | undefined {
    return this._state.contextPacket;
  }
}
