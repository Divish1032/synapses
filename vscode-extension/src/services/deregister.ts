import * as fs from 'fs';
import * as path from 'path';

/** Remove the synapses block from .claude/CLAUDE.md */
function cleanClaudeMd(root: string): void {
  const claudeMdPath = path.join(root, '.claude', 'CLAUDE.md');
  if (!fs.existsSync(claudeMdPath)) return;

  const content = fs.readFileSync(claudeMdPath, 'utf8');
  // Remove everything between <!-- synapses:start --> and <!-- synapses:end --> (inclusive)
  const cleaned = content.replace(/<!-- synapses:start -->[\s\S]*?<!-- synapses:end -->\n?/g, '');
  fs.writeFileSync(claudeMdPath, cleaned, 'utf8');
}

/** Remove the synapses server entry from .mcp.json */
function cleanMcpJson(root: string): void {
  const mcpPath = path.join(root, '.mcp.json');
  if (!fs.existsSync(mcpPath)) return;

  let json: Record<string, unknown>;
  try {
    json = JSON.parse(fs.readFileSync(mcpPath, 'utf8'));
  } catch {
    return;
  }

  const servers = json.mcpServers as Record<string, unknown> | undefined;
  if (servers && 'synapses' in servers) {
    delete servers['synapses'];
    if (Object.keys(servers).length === 0) {
      // No other servers — remove the file entirely
      fs.unlinkSync(mcpPath);
    } else {
      fs.writeFileSync(mcpPath, JSON.stringify(json, null, 2) + '\n', 'utf8');
    }
  } else {
    // Not a synapses-managed .mcp.json — remove entirely if empty
    fs.unlinkSync(mcpPath);
  }
}

/** Remove synapses.json from the project root */
function removeSynapsesJson(root: string): void {
  const p = path.join(root, 'synapses.json');
  if (fs.existsSync(p)) fs.unlinkSync(p);
}

/** Full project deregistration — removes all Synapses artifacts */
export async function deregisterProject(root: string): Promise<void> {
  cleanMcpJson(root);
  cleanClaudeMd(root);
  removeSynapsesJson(root);
}

/** Read synapses.json safely; returns null if missing or invalid */
export function readSynapsesJson(root: string): Record<string, unknown> | null {
  const p = path.join(root, 'synapses.json');
  if (!fs.existsSync(p)) return null;
  try {
    return JSON.parse(fs.readFileSync(p, 'utf8')) as Record<string, unknown>;
  } catch {
    return null;
  }
}

/** Write synapses.json */
export function writeSynapsesJson(root: string, data: Record<string, unknown>): void {
  const p = path.join(root, 'synapses.json');
  fs.writeFileSync(p, JSON.stringify(data, null, 2) + '\n', 'utf8');
}

// Default config blocks to add when enabling a sidecar
const DEFAULTS: Record<string, unknown> = {
  intelligence: { url: 'http://localhost:11435', timeout_sec: 5, enable_llm: true },
  scout: { url: 'http://localhost:11436', timeout_sec: 30 },
  pulse: { url: 'http://localhost:11437', timeout_sec: 2 },
};

// synapses.json key names for each sidecar
const JSON_KEYS: Record<string, string> = {
  intelligence: 'brain',
  scout: 'scout',
  pulse: 'pulse',
};

/** Enable or disable a non-core sidecar by editing synapses.json */
export function toggleSidecarConfig(root: string, sidecar: string, enabled: boolean): void {
  const json = readSynapsesJson(root) ?? {};
  const key = JSON_KEYS[sidecar];
  if (!key) return;

  if (enabled) {
    if (!json[key]) {
      json[key] = DEFAULTS[sidecar] ?? {};
    }
  } else {
    delete json[key];
  }

  writeSynapsesJson(root, json);
}
