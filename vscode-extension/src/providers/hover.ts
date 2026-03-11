import * as vscode from 'vscode';
import { httpGet } from '../http';
import * as cfg from '../config';

function baseName(p: string): string {
  return p.split('/').pop() ?? p;
}

function identifierAt(doc: vscode.TextDocument, pos: vscode.Position): string | null {
  const range = doc.getWordRangeAtPosition(pos, /[A-Za-z_][A-Za-z0-9_]*/);
  return range ? doc.getText(range) : null;
}

function inferLanguage(filePath: string): string {
  const ext = filePath.split('.').pop() ?? '';
  const map: Record<string, string> = {
    go: 'go', ts: 'typescript', tsx: 'typescript',
    js: 'javascript', jsx: 'javascript', py: 'python',
    rs: 'rust', java: 'java', kt: 'kotlin', cs: 'csharp',
    rb: 'ruby', php: 'php', swift: 'swift', scala: 'scala',
  };
  return map[ext] ?? 'plaintext';
}

export class SynapsesHoverProvider implements vscode.HoverProvider {
  private cache = new Map<string, { summary: string; ts: number }>();
  private readonly ttlMs = 60_000;

  async provideHover(
    document: vscode.TextDocument,
    position: vscode.Position,
    token: vscode.CancellationToken
  ): Promise<vscode.Hover | null> {
    if (!cfg.enableHover()) return null;

    const name = identifierAt(document, position);
    if (!name || name.length < 2) return null;

    const root = cfg.repoRoot(document);
    if (!root) return null;
    if (token.isCancellationRequested) return null;

    const relPath = document.uri.fsPath.replace(root + '/', '');
    const nodeId = `${baseName(root)}::${relPath}::${name}`;
    const cached = this.cache.get(nodeId);
    if (cached && Date.now() - cached.ts < this.ttlMs) {
      return this._buildHover(name, cached.summary, document.uri.fsPath);
    }

    try {
      const result = await httpGet<{ summary: string }>(
        cfg.intelligenceUrl(),
        `/v1/summary/${encodeURIComponent(nodeId)}`,
        3000
      );
      if (token.isCancellationRequested) return null;
      if (result.summary) {
        this.cache.set(nodeId, { summary: result.summary, ts: Date.now() });
        return this._buildHover(name, result.summary, document.uri.fsPath);
      }
    } catch {
      // brain not running — no hover
    }
    return null;
  }

  private _buildHover(name: string, summary: string, file: string): vscode.Hover {
    const md = new vscode.MarkdownString('', true);
    md.isTrusted = true;
    md.appendMarkdown(`**${name}** — *${inferLanguage(file)}*\n\n`);
    md.appendMarkdown(summary);
    md.appendMarkdown('\n\n---\n*Synapses*');
    return new vscode.Hover(md);
  }
}

export const HOVER_LANGUAGES = [
  'go', 'typescript', 'typescriptreact', 'javascript', 'javascriptreact',
  'python', 'rust', 'java', 'kotlin', 'csharp', 'ruby', 'php', 'swift', 'scala',
];
