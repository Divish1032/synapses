// api.ts — single module for all daemon API calls.
// Handles CSRF token fetch + attachment, and provides typed helpers.

const BASE = import.meta.env.VITE_API_URL ?? "";

let csrfToken: string | null = null;
let csrfPromise: Promise<string> | null = null;

// Deduplicated CSRF token fetch — prevents race condition when multiple
// concurrent API calls all see csrfToken === null.
async function ensureCSRF(): Promise<string> {
  if (csrfToken) return csrfToken;
  if (csrfPromise) return csrfPromise;
  csrfPromise = fetch(`${BASE}/api/admin/csrf-token`)
    .then((resp) => resp.json())
    .then((data) => {
      csrfToken = data.token;
      csrfPromise = null;
      return csrfToken!;
    })
    .catch((err) => {
      csrfPromise = null;
      throw err;
    });
  return csrfPromise;
}

export async function api<T>(path: string, opts?: RequestInit): Promise<T> {
  const token = await ensureCSRF();
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(opts?.headers as Record<string, string>),
  };
  if (opts?.method && opts.method !== "GET") {
    headers["X-CSRF-Token"] = token;
  }
  const res = await fetch(`${BASE}${path}`, { ...opts, headers });
  if (res.status === 403) {
    csrfToken = null;
    throw new Error("CSRF token expired — retry");
  }
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  return res.json();
}

// Convenience for GET (no CSRF needed on GET, but token still fetched for later)
export async function get<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    signal: AbortSignal.timeout(5000),
  });
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  return res.json();
}

export function apiBase(): string {
  return BASE;
}

// ── MCP REST Tool API ─────────────────────────────────────────────────────────
// Calls any MCP tool via POST /v1/tools/{name}?project=<path>
// Returns the first text content block parsed as JSON, or raw text.
export async function callTool<T = any>(
  toolName: string,
  project: string,
  args?: Record<string, unknown>,
): Promise<T> {
  const res = await api<{ content: Array<{ type: string; text: string }>; isError?: boolean }>(
    `/v1/tools/${toolName}?project=${encodeURIComponent(project)}`,
    { method: "POST", body: JSON.stringify(args ?? {}) },
  );
  if (res.isError) {
    const msg = res.content?.[0]?.text ?? "tool error";
    throw new Error(msg);
  }
  const text = res.content?.find((c) => c.type === "text")?.text;
  if (!text) return {} as T;
  try { return JSON.parse(text); } catch { return text as unknown as T; }
}

// ── SSE streaming helper ──────────────────────────────────────────────────────
// For endpoints like POST /api/admin/ollama/pull that stream SSE events.
// Returns an AbortController so callers can cancel the stream on unmount.
export function streamSSE(
  path: string,
  body: Record<string, unknown>,
  onEvent: (data: any) => void,
  onDone?: () => void,
  onError?: (err: Error) => void,
): AbortController {
  const controller = new AbortController();

  (async () => {
    try {
      const token = await ensureCSRF();
      const res = await fetch(`${BASE}${path}`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": token,
        },
        body: JSON.stringify(body),
        signal: controller.signal,
      });
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      const reader = res.body?.getReader();
      if (!reader) return;
      const decoder = new TextDecoder();
      let buf = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const lines = buf.split("\n");
        buf = lines.pop() ?? "";
        for (const line of lines) {
          const trimmed = line.trim();
          if (trimmed.startsWith("data:")) {
            try {
              const parsed = JSON.parse(trimmed.slice(5).trim());
              onEvent(parsed);
            } catch { /* skip malformed */ }
          }
        }
      }
      onDone?.();
    } catch (err: any) {
      if (err.name === "AbortError") return; // cancelled by unmount — expected
      onError?.(err);
    }
  })();

  return controller;
}
