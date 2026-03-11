import * as vscode from 'vscode';
import { httpGet } from '../http';
import * as cfg from '../config';
import { buildPulseHtml } from './pulse-html';
import { PulseDashboard } from '../types';

export class PulseDashboardPanel {
  private static _instance?: PulseDashboardPanel;

  private _panel: vscode.WebviewPanel;
  private _refreshTimer?: ReturnType<typeof setInterval>;

  private constructor(extensionUri: vscode.Uri) {
    this._panel = vscode.window.createWebviewPanel(
      'synapses.pulse',
      'Synapses Pulse — Analytics',
      vscode.ViewColumn.Beside,
      {
        enableScripts: true,
        localResourceRoots: [extensionUri],
        retainContextWhenHidden: true,
      }
    );

    this._panel.webview.html = buildPulseHtml(null);
    this._loadData();

    this._panel.webview.onDidReceiveMessage((msg) => {
      if (msg.command === 'refresh') {
        this._loadData();
      }
    });

    this._panel.onDidDispose(() => {
      this._dispose();
    });

    // Auto-refresh every N seconds while visible
    const intervalMs = cfg.pulseRefreshSec() * 1000;
    this._refreshTimer = setInterval(() => {
      if (this._panel.visible) {
        this._loadData();
      }
    }, intervalMs);
  }

  /** Create or reveal the pulse dashboard panel */
  static show(extensionUri: vscode.Uri): void {
    if (PulseDashboardPanel._instance) {
      PulseDashboardPanel._instance._panel.reveal(vscode.ViewColumn.Beside);
      return;
    }
    PulseDashboardPanel._instance = new PulseDashboardPanel(extensionUri);
  }

  private async _loadData(): Promise<void> {
    try {
      const [summary, timeline, tools] = await Promise.allSettled([
        httpGet<PulseDashboard['summary']>(cfg.pulseUrl(), '/v1/summary?days=7', 5000),
        httpGet<{ points: PulseDashboard['timeline'] }>(cfg.pulseUrl(), '/v1/timeline?days=30&granularity=daily', 5000),
        httpGet<{ tools: PulseDashboard['tools'] }>(cfg.pulseUrl(), '/v1/tools?days=7', 5000),
      ]);

      const dashboard: PulseDashboard = {
        summary: summary.status === 'fulfilled' ? summary.value : null as unknown as PulseDashboard['summary'],
        timeline: timeline.status === 'fulfilled' ? (timeline.value.points ?? []) : [],
        tools: tools.status === 'fulfilled' ? (tools.value.tools ?? []) : [],
      };

      if (dashboard.summary) {
        this._panel.webview.html = buildPulseHtml(dashboard);
      }
    } catch {
      // pulse unreachable — keep existing view
    }
  }

  private _dispose(): void {
    if (this._refreshTimer) {
      clearInterval(this._refreshTimer);
    }
    PulseDashboardPanel._instance = undefined;
  }
}
