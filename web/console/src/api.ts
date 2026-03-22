// api.ts — single module for all daemon API calls.
// Handles CSRF token fetch + attachment, and provides typed helpers.

const BASE = import.meta.env.VITE_API_URL ?? "";

let csrfToken: string | null = null;

export async function api<T>(path: string, opts?: RequestInit): Promise<T> {
  // Fetch CSRF token on first mutation
  if (!csrfToken) {
    const resp = await fetch(`${BASE}/api/admin/csrf-token`);
    csrfToken = (await resp.json()).token;
  }
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(opts?.headers as Record<string, string>),
  };
  if (opts?.method && opts.method !== "GET") {
    headers["X-CSRF-Token"] = csrfToken!;
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
