import * as vscode from 'vscode';
import { HealthState } from '../types';

export class StatusBarManager implements vscode.Disposable {
  private _item: vscode.StatusBarItem;

  constructor() {
    this._item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 10);
    this._item.command = 'workbench.view.extension.synapses';
    this._item.tooltip = 'Synapses — click to open control center';
    this._item.text = '$(symbol-class) Synapses';
    this._item.show();
  }

  update(state: HealthState): void {
    const total = 4;
    const online = Object.values(state).filter((s) => s.status === 'online').length;
    const allOnline = online === total;
    const allOffline = online === 0;

    if (allOnline) {
      this._item.text = '$(symbol-class) Synapses';
      this._item.backgroundColor = undefined;
      this._item.tooltip = 'Synapses — all services online';
    } else if (allOffline) {
      this._item.text = '$(warning) Synapses: offline';
      this._item.backgroundColor = new vscode.ThemeColor('statusBarItem.warningBackground');
      this._item.tooltip = 'Synapses — no services running';
    } else {
      this._item.text = `$(symbol-class) Synapses: ${online}/${total}`;
      this._item.backgroundColor = undefined;
      const offlineNames = Object.values(state)
        .filter((s) => s.status !== 'online' && s.status !== 'disabled')
        .map((s) => s.id)
        .join(', ');
      this._item.tooltip = `Synapses — offline: ${offlineNames}`;
    }
  }

  dispose(): void {
    this._item.dispose();
  }
}
