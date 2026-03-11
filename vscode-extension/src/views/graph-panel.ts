import * as vscode from 'vscode';
import { execFile } from 'child_process';
import { promisify } from 'util';
import * as cfg from '../config';
import { buildGraphHtml } from './graph-html';
import { EntityInfo } from '../types';

const execFileAsync = promisify(execFile);

export class GraphExplorerPanel {
  private static _instance?: GraphExplorerPanel;

  private _panel: vscode.WebviewPanel;

  private constructor(extensionUri: vscode.Uri) {
    this._panel = vscode.window.createWebviewPanel(
      'synapses.graphExplorer',
      'Synapses Graph Explorer',
      vscode.ViewColumn.Beside,
      {
        enableScripts: true,
        localResourceRoots: [extensionUri],
        retainContextWhenHidden: true,
      }
    );

    this._panel.webview.html = buildGraphHtml(null);

    this._panel.webview.onDidReceiveMessage(async (msg) => {
      switch (msg.command) {
        case 'search':
          await this._search(msg.query as string);
          break;
        case 'navigate': {
          const root = cfg.workspaceRoot();
          if (!root) break;
          const file = msg.file as string;
          const line = (msg.line as number) || 0;
          const fullPath = file.startsWith('/') ? file : `${root}/${file}`;
          const uri = vscode.Uri.file(fullPath);
          const pos = new vscode.Position(Math.max(0, line - 1), 0);
          await vscode.window.showTextDocument(uri, { selection: new vscode.Range(pos, pos) });
          break;
        }
      }
    });

    this._panel.onDidDispose(() => {
      GraphExplorerPanel._instance = undefined;
    });
  }

  static show(extensionUri: vscode.Uri): void {
    if (GraphExplorerPanel._instance) {
      GraphExplorerPanel._instance._panel.reveal(vscode.ViewColumn.Beside);
      return;
    }
    GraphExplorerPanel._instance = new GraphExplorerPanel(extensionUri);
  }

  private async _search(query: string): Promise<void> {
    const root = cfg.workspaceRoot();
    if (!root) {
      this._panel.webview.html = buildGraphHtml({ query, error: 'No workspace folder open.' });
      return;
    }

    try {
      // Query the synapses CLI for entity info
      const { stdout } = await execFileAsync(
        cfg.binaryPath(),
        ['query', '-entity', query, '-path', root, '-json'],
        { timeout: 5000 }
      );
      const data = JSON.parse(stdout);

      const entity: EntityInfo & { summary?: string } = {
        id: String(data.id ?? ''),
        name: String(data.name ?? query),
        type: String(data.type ?? 'function'),
        file: String(data.file ?? ''),
        line: Number(data.line ?? 0),
        fanin: Number(data.fanin ?? 0),
        fanout: Number(data.fanout ?? 0),
        summary: data.summary ? String(data.summary) : undefined,
      };

      const callers: EntityInfo[] = (data.callers ?? []).map(
        (c: Record<string, unknown>) => ({
          id: String(c.id ?? ''),
          name: String(c.name ?? ''),
          type: String(c.type ?? 'function'),
          file: String(c.file ?? ''),
          line: Number(c.line ?? 0),
          fanin: Number(c.fanin ?? 0),
          fanout: Number(c.fanout ?? 0),
        })
      );

      const callees: EntityInfo[] = (data.callees ?? []).map(
        (c: Record<string, unknown>) => ({
          id: String(c.id ?? ''),
          name: String(c.name ?? ''),
          type: String(c.type ?? 'function'),
          file: String(c.file ?? ''),
          line: Number(c.line ?? 0),
          fanin: Number(c.fanin ?? 0),
          fanout: Number(c.fanout ?? 0),
        })
      );

      // Optionally get mermaid output
      let mermaid: string | undefined;
      try {
        const mermaidResult = await execFileAsync(
          cfg.binaryPath(),
          ['export', '-format', 'mermaid', '-entity', query, '-path', root],
          { timeout: 5000 }
        );
        mermaid = mermaidResult.stdout.trim() || undefined;
      } catch {
        // mermaid export is optional
      }

      this._panel.webview.html = buildGraphHtml({ query, entity, callers, callees, mermaid });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      this._panel.webview.html = buildGraphHtml({ query, error: `Entity not found: ${msg}` });
    }
  }
}
