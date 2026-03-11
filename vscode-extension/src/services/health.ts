import { EventEmitter } from 'events';
import { execFile } from 'child_process';
import { promisify } from 'util';
import { httpGet } from '../http';
import * as cfg from '../config';
import { HealthState, ServiceId, ServiceHealth } from '../types';

const execFileAsync = promisify(execFile);

const DEFAULT_POLL_MS = 15_000;
const MIN_BACKOFF_MS  = 2_000;
const MAX_BACKOFF_MS  = 60_000;

function initState(): HealthState {
  const ids: ServiceId[] = ['core', 'intelligence', 'scout', 'pulse'];
  const state = {} as HealthState;
  for (const id of ids) {
    state[id] = { id, status: 'offline' };
  }
  return state;
}

export class HealthPoller extends EventEmitter {
  private _state: HealthState = initState();
  private _timer: ReturnType<typeof setInterval> | null = null;
  private _backoff: Map<ServiceId, number> = new Map();
  private _nextPoll: Map<ServiceId, number> = new Map();

  start(intervalMs = DEFAULT_POLL_MS): void {
    this.pollOnce();
    this._timer = setInterval(() => this.pollOnce(), intervalMs);
  }

  stop(): void {
    if (this._timer) {
      clearInterval(this._timer);
      this._timer = null;
    }
  }

  dispose(): void {
    this.stop();
  }

  getState(): HealthState {
    return { ...this._state };
  }

  async pollOnce(): Promise<HealthState> {
    const now = Date.now();
    const polls = (['core', 'intelligence', 'scout', 'pulse'] as ServiceId[])
      .filter((id) => (this._nextPoll.get(id) ?? 0) <= now)
      .map((id) => this._pollService(id));

    await Promise.allSettled(polls);
    this.emit('update', this.getState());
    return this.getState();
  }

  private async _pollService(id: ServiceId): Promise<void> {
    const start = Date.now();
    try {
      const health = await this._checkService(id, start);
      this._state[id] = health;
      this._backoff.delete(id);        // reset backoff on success
      this._nextPoll.set(id, 0);       // poll again at next normal interval
    } catch {
      const prev = this._backoff.get(id) ?? MIN_BACKOFF_MS;
      const next = Math.min(prev * 2, MAX_BACKOFF_MS);
      this._backoff.set(id, next);
      this._nextPoll.set(id, Date.now() + next);
      this._state[id] = { id, status: 'offline' };
    }
  }

  private async _checkService(id: ServiceId, start: number): Promise<ServiceHealth> {
    switch (id) {
      case 'core':
        return this._checkCore(start);
      case 'intelligence':
        return this._checkHttp(id, cfg.intelligenceUrl(), '/v1/health', start);
      case 'scout':
        return this._checkHttp(id, cfg.scoutUrl(), '/v1/health', start);
      case 'pulse':
        return this._checkHttp(id, cfg.pulseUrl(), '/v1/health', start);
    }
  }

  private async _checkCore(start: number): Promise<ServiceHealth> {
    const { stdout } = await execFileAsync(cfg.binaryPath(), ['version'], { timeout: 3000 });
    const latencyMs = Date.now() - start;
    const version = stdout.trim().split(/\s+/).pop() ?? undefined;
    return { id: 'core', status: 'online', version, latencyMs };
  }

  private async _checkHttp(
    id: ServiceId,
    baseUrl: string,
    path: string,
    start: number
  ): Promise<ServiceHealth> {
    const res = await httpGet<{ status: string; version?: string }>(baseUrl, path, 3000);
    const latencyMs = Date.now() - start;
    const status = res.status === 'ok' ? 'online' : 'degraded';
    return { id, status, version: res.version, latencyMs };
  }
}
