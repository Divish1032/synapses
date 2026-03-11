import * as vscode from 'vscode';
import { execFile } from 'child_process';
import { promisify } from 'util';
import { HealthPoller } from '../services/health';
import { OllamaService } from '../services/ollama';
import { BrainService } from '../services/brain';
import { ProjectService } from '../services/project';
import { buildSidebarHtml } from './sidebar-html';
import { httpGet, httpPost } from '../http';
import * as cfg from '../config';
import {
  HealthState, SidebarState, SidebarTab, ContextPacket, SDLCConfig,
  OllamaStatus, OllamaModel, ServiceId, PulseSummary, PulseTimelinePoint,
  PulseAgentStats, BrainCostTier,
} from '../types';

const execFileAsync = promisify(execFile);

function baseName(p: string): string {
  return p.split('/').pop() ?? p;
}

function identifierAt(doc: vscode.TextDocument, pos: vscode.Position): string | null {
  const range = doc.getWordRangeAtPosition(pos, /[A-Za-z_][A-Za-z0-9_]*/);
  return range ? doc.getText(range) : null;
}

async function symbolKindAt(doc: vscode.TextDocument, pos: vscode.Position): Promise<string> {
  try {
    const symbols = await vscode.commands.executeCommand<vscode.DocumentSymbol[]>(
      'vscode.executeDocumentSymbolProvider', doc.uri
    );
    if (!symbols) return 'function';
    const match = findDeepest(symbols, pos);
    if (!match) return 'function';
    const kindMap: Record<number, string> = {
      [vscode.SymbolKind.Function]: 'function',
      [vscode.SymbolKind.Method]: 'method',
      [vscode.SymbolKind.Class]: 'struct',
      [vscode.SymbolKind.Struct]: 'struct',
      [vscode.SymbolKind.Interface]: 'interface',
      [vscode.SymbolKind.Variable]: 'variable',
      [vscode.SymbolKind.Constant]: 'variable',
      [vscode.SymbolKind.Property]: 'variable',
      [vscode.SymbolKind.Enum]: 'struct',
      [vscode.SymbolKind.Module]: 'package',
      [vscode.SymbolKind.Namespace]: 'package',
      [vscode.SymbolKind.Constructor]: 'method',
    };
    return kindMap[match.kind] ?? 'function';
  } catch {
    return 'function';
  }
}

function findDeepest(symbols: vscode.DocumentSymbol[], pos: vscode.Position): vscode.DocumentSymbol | null {
  for (const sym of symbols) {
    if (sym.range.contains(pos)) {
      const child = findDeepest(sym.children, pos);
      return child ?? sym;
    }
  }
  return null;
}

export class SynapsesSidebarProvider implements vscode.WebviewViewProvider {
  public static readonly viewType = 'synapses.sidebar';

  private _view?: vscode.WebviewView;
  private _state: SidebarState;
  private _brainService: BrainService;
  private _projectService: ProjectService;
  private _context?: vscode.ExtensionContext;

  constructor(
    private readonly _extensionUri: vscode.Uri,
    private readonly _healthPoller: HealthPoller,
    private readonly _ollamaService: OllamaService,
    context?: vscode.ExtensionContext
  ) {
    this._brainService = new BrainService();
    this._projectService = new ProjectService();
    this._context = context;

    const savedCollapsed = context?.workspaceState.get<Record<string, boolean>>('synapses.collapsed') ?? {};

    this._state = {
      activeTab: cfg.sidebarDefaultTab(),
      health: _healthPoller.getState(),
      ollamaStatus: 'stopped',
      ollamaModels: [],
      defaultModel: cfg.defaultModel(),
      analyticsDateRange: cfg.analyticsDateRange(),
      collapsedSections: savedCollapsed,
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
    this._refreshTabData(this._state.activeTab);

    webviewView.webview.onDidReceiveMessage((msg) => this._handleMessage(msg));

    vscode.window.onDidChangeActiveTextEditor(() => {
      this.refreshForActiveEditor();
    });
  }

  public updateHealth(state: HealthState): void {
    this._state = { ...this._state, health: state };
    if (this._state.activeTab === 'home') {
      this._refreshProjectIdentity();
    }
    if (this._state.activeTab === 'analytics') {
      this._refreshPulse();
    }
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
      case 'switchTab': {
        const tab = msg.tab as SidebarTab;
        this._state = { ...this._state, activeTab: tab };
        this._render();
        this._refreshTabData(tab);
        break;
      }
      case 'toggleSection': {
        const section = msg.section as string;
        const collapsed = { ...this._state.collapsedSections };
        collapsed[section] = !collapsed[section];
        this._state = { ...this._state, collapsedSections: collapsed };
        this._context?.workspaceState.update('synapses.collapsed', collapsed);
        this._render();
        break;
      }
      case 'setDateRange': {
        const days = msg.days as number;
        this._state = { ...this._state, analyticsDateRange: days };
        this._refreshPulse();
        break;
      }
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
      case 'deleteModel': {
        const modelName = msg.model as string;
        const confirm = await vscode.window.showWarningMessage(
          `Delete model "${modelName}"? This cannot be undone.`,
          { modal: true },
          'Delete'
        );
        if (confirm !== 'Delete') break;
        await this._ollamaService.deleteModel(modelName);
        await this._refreshOllama();
        break;
      }
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
      case 'navigateToEntity': {
        const file = msg.file as string;
        const line = (msg.line as number) || 0;
        const root = cfg.workspaceRoot();
        if (!root) break;
        const fullPath = file.startsWith('/') ? file : `${root}/${file}`;
        const uri = vscode.Uri.file(fullPath);
        const pos = new vscode.Position(Math.max(0, line - 1), 0);
        await vscode.window.showTextDocument(uri, { selection: new vscode.Range(pos, pos) });
        break;
      }
      case 'showGraphExplorer':
        await vscode.commands.executeCommand('synapses.showGraphExplorer');
        break;
    }
  }

  // Tab-aware data loading — only fetch what the active tab needs
  private async _refreshTabData(tab: SidebarTab): Promise<void> {
    switch (tab) {
      case 'home':
        this._refreshProjectIdentity();
        this._refreshPulse(); // for ROI banner
        break;
      case 'intelligence':
        this._refreshOllama();
        this._refreshBrainHealth();
        this._refreshPatterns();
        this._refreshADRs();
        this._refreshSDLC();
        break;
      case 'analytics':
        this._refreshPulse();
        this._refreshAgents();
        this._refreshBrainCosts();
        break;
      case 'explorer':
        this._refreshProjectIdentity();
        this._refreshViolations();
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

  // ── Data refresh methods ──────────────────────────────────────────────

  private async _refreshProjectIdentity(): Promise<void> {
    const root = cfg.workspaceRoot();
    if (!root) return;
    const identity = await this._projectService.getProjectIdentity(root);
    if (identity) {
      this._state = {
        ...this._state,
        projectIdentity: identity,
        keyEntities: identity.key_entities,
        suggestedRules: identity.suggested_rules,
        graphSummary: identity.summary,
      };
      this._render();
    }
  }

  private async _refreshBrainHealth(): Promise<void> {
    if (this._state.health.intelligence?.status !== 'online') return;
    try {
      const health = await this._brainService.getHealthExtended();
      this._state = { ...this._state, brainHealth: health };
      this._render();
    } catch {
      // brain not reachable
    }
  }

  private async _refreshPatterns(): Promise<void> {
    if (this._state.health.intelligence?.status !== 'online') return;
    const patterns = await this._brainService.getPatterns(20);
    this._state = { ...this._state, patterns };
    this._render();
  }

  private async _refreshADRs(): Promise<void> {
    if (this._state.health.intelligence?.status !== 'online') return;
    const adrs = await this._brainService.getADRs();
    this._state = { ...this._state, adrs };
    this._render();
  }

  private async _refreshSDLC(): Promise<void> {
    if (this._state.health.intelligence?.status !== 'online') return;
    const sdlc = await this._brainService.getSDLC();
    if (sdlc) {
      this._state = { ...this._state, sdlc };
      this._render();
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
    const days = this._state.analyticsDateRange;
    try {
      const [summaryRes, timelineRes] = await Promise.allSettled([
        httpGet<PulseSummary>(cfg.pulseUrl(), `/v1/summary?days=${days}`, 3000),
        httpGet<{ points: PulseTimelinePoint[] }>(cfg.pulseUrl(), `/v1/timeline?days=${days}&granularity=daily`, 3000),
      ]);
      this._state = {
        ...this._state,
        pulse: summaryRes.status === 'fulfilled' ? summaryRes.value : this._state.pulse,
        pulseTrend: timelineRes.status === 'fulfilled' ? (timelineRes.value.points ?? []) : this._state.pulseTrend,
      };
      this._render();
    } catch {
      // pulse unreachable
    }
  }

  private async _refreshAgents(): Promise<void> {
    if (this._state.health.pulse?.status !== 'online') return;
    try {
      const res = await httpGet<{ agents: PulseAgentStats[] }>(cfg.pulseUrl(), '/v1/agents', 3000);
      this._state = { ...this._state, pulseAgents: res.agents ?? [] };
      this._render();
    } catch {
      // pulse unreachable
    }
  }

  private async _refreshBrainCosts(): Promise<void> {
    if (this._state.health.pulse?.status !== 'online') return;
    try {
      const res = await httpGet<{ costs: BrainCostTier[] }>(cfg.pulseUrl(), '/v1/brain-costs', 3000);
      this._state = { ...this._state, brainCosts: res.costs ?? [] };
      this._render();
    } catch {
      // pulse unreachable
    }
  }

  private async _refreshViolations(): Promise<void> {
    const root = cfg.workspaceRoot();
    if (!root) return;
    const violations = await this._projectService.getViolations(root);
    this._state = { ...this._state, violations };
    this._render();
  }

  private async _refreshContext(name: string, doc: vscode.TextDocument): Promise<void> {
    const root = cfg.repoRoot(doc);
    if (!root || this._state.health.intelligence?.status !== 'online') return;

    const editor = vscode.window.activeTextEditor;
    const pos = editor?.document === doc ? editor.selection.active : new vscode.Position(0, 0);
    const rootType = await symbolKindAt(doc, pos);

    const [statsResult, sdlcResult] = await Promise.allSettled([
      execFileAsync(cfg.binaryPath(), ['status', '-path', root]),
      httpGet<SDLCConfig>(cfg.intelligenceUrl(), '/v1/sdlc', 3000),
    ]);

    this._state = {
      ...this._state,
      graphStats: statsResult.status === 'fulfilled' ? statsResult.value.stdout : undefined,
      sdlc: sdlcResult.status === 'fulfilled' ? sdlcResult.value : undefined,
    };

    const relPath = doc.uri.fsPath.replace(root + '/', '');
    const nodeId = `${baseName(root)}::${relPath}::${name}`;
    try {
      const packet = await httpPost<ContextPacket>(cfg.intelligenceUrl(), '/v1/context-packet', {
        snapshot: { root_node_id: nodeId, root_name: name, root_type: rootType, root_file: doc.uri.fsPath },
        enable_llm: cfg.enableLLM(),
      }, 5000);
      this._state = { ...this._state, contextPacket: packet };
    } catch {
      this._state = { ...this._state, contextPacket: undefined };
    }

    this._render();
  }

  private async _setPhase(phase: string): Promise<void> {
    try {
      await this._brainService.setPhase(phase);
      vscode.window.showInformationMessage(`Synapses: SDLC phase set to ${phase}`);
      this._refreshSDLC();
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
