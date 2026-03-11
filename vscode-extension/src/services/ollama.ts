import * as http from 'http';
import { execFile } from 'child_process';
import { promisify } from 'util';
import { ollamaUrl } from '../config';
import { OllamaStatus, OllamaModel } from '../types';

const execFileAsync = promisify(execFile);

interface OllamaTagsResponse {
  models: { name: string; size: number; modified_at: string }[];
}

export class OllamaService {
  async getStatus(): Promise<OllamaStatus> {
    try {
      await this.listModels();
      return 'running';
    } catch {
      // Check if ollama is installed but not running
      try {
        await execFileAsync('which', ['ollama'], { timeout: 2000 });
        return 'stopped';
      } catch {
        return 'not-installed';
      }
    }
  }

  async listModels(): Promise<OllamaModel[]> {
    const url = ollamaUrl();
    const res = await httpGetOllama<OllamaTagsResponse>(url, '/api/tags', 3000);
    return (res.models ?? []).map((m) => ({
      name: m.name,
      size: m.size,
      modified: m.modified_at,
    }));
  }

  async pullModel(
    name: string,
    onProgress: (pct: number, status: string) => void
  ): Promise<void> {
    return new Promise((resolve, reject) => {
      const url = new URL('/api/pull', ollamaUrl());
      const payload = JSON.stringify({ name, stream: true });

      const req = http.request(
        {
          hostname: url.hostname,
          port: url.port || 11434,
          path: url.pathname,
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            'Content-Length': Buffer.byteLength(payload),
          },
        },
        (res) => {
          let buffer = '';
          res.on('data', (chunk: Buffer) => {
            buffer += chunk.toString();
            const lines = buffer.split('\n');
            buffer = lines.pop() ?? '';
            for (const line of lines) {
              if (!line.trim()) continue;
              try {
                const evt = JSON.parse(line) as {
                  status: string;
                  completed?: number;
                  total?: number;
                };
                const pct =
                  evt.total && evt.completed
                    ? Math.round((evt.completed / evt.total) * 100)
                    : 0;
                onProgress(pct, evt.status ?? '');
              } catch {
                // ignore malformed NDJSON lines
              }
            }
          });
          res.on('end', () => resolve());
          res.on('error', reject);
        }
      );

      req.on('error', reject);
      req.setTimeout(300_000, () => {
        req.destroy();
        reject(new Error('pull timeout'));
      });
      req.write(payload);
      req.end();
    });
  }

  async deleteModel(name: string): Promise<void> {
    return new Promise((resolve, reject) => {
      const url = new URL('/api/delete', ollamaUrl());
      const payload = JSON.stringify({ name });

      const req = http.request(
        {
          hostname: url.hostname,
          port: url.port || 11434,
          path: url.pathname,
          method: 'DELETE',
          headers: {
            'Content-Type': 'application/json',
            'Content-Length': Buffer.byteLength(payload),
          },
        },
        (res) => {
          res.resume();
          res.on('end', () => resolve());
        }
      );
      req.on('error', reject);
      req.setTimeout(10_000, () => {
        req.destroy();
        reject(new Error('delete timeout'));
      });
      req.write(payload);
      req.end();
    });
  }
}

function httpGetOllama<T>(baseUrl: string, path: string, timeoutMs: number): Promise<T> {
  return new Promise((resolve, reject) => {
    const url = new URL(path, baseUrl);
    const req = http.request(
      {
        hostname: url.hostname,
        port: url.port || 11434,
        path: url.pathname,
        method: 'GET',
      },
      (res) => {
        let data = '';
        res.on('data', (chunk) => (data += chunk));
        res.on('end', () => {
          try {
            resolve(JSON.parse(data) as T);
          } catch {
            reject(new Error(`Ollama JSON parse error: ${data.slice(0, 100)}`));
          }
        });
      }
    );
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => {
      req.destroy();
      reject(new Error('Ollama request timeout'));
    });
    req.end();
  });
}
