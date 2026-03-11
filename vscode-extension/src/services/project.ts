import { execFile } from 'child_process';
import { promisify } from 'util';
import * as cfg from '../config';
import {
  ProjectIdentity, GraphSummary, EntityInfo, Violation,
  SuggestedRule,
} from '../types';

const execFileAsync = promisify(execFile);

export class ProjectService {
  async getProjectIdentity(root: string): Promise<ProjectIdentity | null> {
    try {
      const { stdout } = await execFileAsync(
        cfg.binaryPath(), ['status', '-path', root, '-json'], { timeout: 5000 }
      );
      const data = JSON.parse(stdout);

      const summary: GraphSummary = {
        files: data.files ?? 0,
        packages: data.packages ?? 0,
        functions: data.functions ?? 0,
        methods: data.methods ?? 0,
        structs: data.structs ?? 0,
        interfaces: data.interfaces ?? 0,
        edges: data.edges ?? 0,
      };

      const keyEntities: EntityInfo[] = (data.key_entities ?? []).map(
        (e: Record<string, unknown>) => ({
          id: String(e.id ?? ''),
          name: String(e.name ?? ''),
          type: String(e.type ?? 'function'),
          file: String(e.file ?? ''),
          line: Number(e.line ?? 0),
          fanin: Number(e.fanin ?? 0),
          fanout: Number(e.fanout ?? 0),
        })
      );

      const suggestedRules: SuggestedRule[] = (data.suggested_rules ?? []).map(
        (r: Record<string, unknown>) => ({
          id: String(r.id ?? ''),
          description: String(r.description ?? ''),
          confidence: Number(r.confidence ?? 0),
        })
      );

      const totalNodes = summary.files + summary.functions + summary.methods +
        summary.structs + summary.interfaces;
      let scale: ProjectIdentity['scale'] = 'micro';
      if (totalNodes >= 2000) scale = 'large';
      else if (totalNodes >= 500) scale = 'medium';
      else if (totalNodes >= 100) scale = 'small';

      return {
        repo_id: String(data.repo_id ?? root.split('/').pop() ?? ''),
        summary,
        key_entities: keyEntities,
        scale: (data.scale as ProjectIdentity['scale']) ?? scale,
        suggested_rules: suggestedRules,
        languages: data.languages ?? [],
      };
    } catch {
      return null;
    }
  }

  async getViolations(root: string): Promise<Violation[]> {
    try {
      const { stdout } = await execFileAsync(
        cfg.binaryPath(), ['violations', '-path', root, '-json'], { timeout: 5000 }
      );
      const data = JSON.parse(stdout);
      return (data.violations ?? data ?? []).map(
        (v: Record<string, unknown>) => ({
          rule_id: String(v.rule_id ?? ''),
          rule_name: String(v.rule_name ?? v.rule_id ?? ''),
          severity: (v.severity as Violation['severity']) ?? 'warning',
          from: String(v.from ?? ''),
          to: String(v.to ?? ''),
          message: String(v.message ?? ''),
        })
      );
    } catch {
      return [];
    }
  }

  async getGraphSummary(root: string): Promise<GraphSummary | null> {
    const identity = await this.getProjectIdentity(root);
    return identity?.summary ?? null;
  }
}
