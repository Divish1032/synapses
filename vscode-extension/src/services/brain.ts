import { httpGet, httpPost } from '../http';
import * as cfg from '../config';
import { BrainHealthExtended, PatternHint, ADR, SDLCConfig } from '../types';

export class BrainService {
  async getHealthExtended(): Promise<BrainHealthExtended> {
    return httpGet<BrainHealthExtended>(cfg.intelligenceUrl(), '/v1/health', 3000);
  }

  async getPatterns(limit = 20): Promise<PatternHint[]> {
    try {
      const res = await httpGet<{ patterns: PatternHint[] }>(
        cfg.intelligenceUrl(), `/v1/patterns?limit=${limit}`, 3000
      );
      return res.patterns ?? [];
    } catch {
      return [];
    }
  }

  async getADRs(): Promise<ADR[]> {
    try {
      const res = await httpGet<{ adrs: ADR[] }>(cfg.intelligenceUrl(), '/v1/adrs', 3000);
      return res.adrs ?? [];
    } catch {
      return [];
    }
  }

  async getSDLC(): Promise<SDLCConfig | null> {
    try {
      return await httpGet<SDLCConfig>(cfg.intelligenceUrl(), '/v1/sdlc', 3000);
    } catch {
      return null;
    }
  }

  async setPhase(phase: string): Promise<void> {
    await httpPost<unknown>(cfg.intelligenceUrl(), '/v1/sdlc/phase', { phase }, 3000);
  }
}
