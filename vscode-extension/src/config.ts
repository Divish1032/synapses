import * as vscode from 'vscode';
import * as path from 'path';

function cfg(): vscode.WorkspaceConfiguration {
  return vscode.workspace.getConfiguration('synapses');
}

export function binaryPath(): string {
  return cfg().get<string>('binaryPath') || 'synapses';
}

export function intelligenceUrl(): string {
  return cfg().get<string>('brainUrl') || 'http://localhost:11435';
}

export function scoutUrl(): string {
  return cfg().get<string>('scoutUrl') || 'http://localhost:11436';
}

export function pulseUrl(): string {
  return cfg().get<string>('pulseUrl') || 'http://localhost:11437';
}

export function ollamaUrl(): string {
  return cfg().get<string>('ollamaUrl') || 'http://localhost:11434';
}

export function enableHover(): boolean {
  return cfg().get<boolean>('enableHover') ?? true;
}

export function autoIngest(): boolean {
  return cfg().get<boolean>('autoIngest') ?? true;
}

export function healthPollSec(): number {
  return cfg().get<number>('healthPollInterval') ?? 15;
}

export function pulseRefreshSec(): number {
  return cfg().get<number>('pulseRefreshInterval') ?? 30;
}

export function defaultModel(): string {
  return cfg().get<string>('defaultModel') || 'qwen2.5-coder:1.5b';
}

export function enableLLM(): boolean {
  return cfg().get<boolean>('enableLLM') ?? true;
}

export type SidebarTab = 'home' | 'intelligence' | 'analytics' | 'explorer';

export function sidebarDefaultTab(): SidebarTab {
  return cfg().get<SidebarTab>('sidebarDefaultTab') || 'home';
}

export function analyticsDateRange(): number {
  return cfg().get<number>('analyticsDateRange') ?? 7;
}

export function workspaceRoot(): string {
  return vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? '';
}

export function repoRoot(document: vscode.TextDocument): string {
  return vscode.workspace.getWorkspaceFolder(document.uri)?.uri.fsPath ?? '';
}

export function synapsesJsonPath(root: string): string {
  return path.join(root, 'synapses.json');
}
