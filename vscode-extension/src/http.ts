import * as http from 'http';
import * as https from 'https';

function getDriver(baseUrl: string) {
  return baseUrl.startsWith('https') ? https : http;
}

function resolveJson<T>(res: http.IncomingMessage, data: string, resolve: (v: T) => void, reject: (e: Error) => void): void {
  if (!res.statusCode || res.statusCode < 200 || res.statusCode >= 300) {
    reject(new Error(`HTTP ${res.statusCode}: ${data.slice(0, 200)}`));
    return;
  }
  try {
    resolve(JSON.parse(data) as T);
  } catch {
    reject(new Error(`JSON parse error: ${data.slice(0, 200)}`));
  }
}

function makeOpts(baseUrl: string, urlPath: string, method: string, headers?: Record<string, string | number>) {
  const url = new URL(urlPath, baseUrl);
  return {
    hostname: url.hostname,
    port: url.port || (baseUrl.startsWith('https') ? 443 : 80),
    path: url.pathname + url.search,
    method,
    headers,
  };
}

export function httpGet<T>(baseUrl: string, urlPath: string, timeoutMs = 5000): Promise<T> {
  return new Promise((resolve, reject) => {
    const req = getDriver(baseUrl).request(makeOpts(baseUrl, urlPath, 'GET'), (res) => {
      let data = '';
      res.on('data', (chunk) => (data += chunk));
      res.on('end', () => resolveJson(res, data, resolve, reject));
    });
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => { req.destroy(); reject(new Error('request timeout')); });
    req.end();
  });
}

export function httpPost<T>(baseUrl: string, urlPath: string, body: unknown, timeoutMs = 10000): Promise<T> {
  return new Promise((resolve, reject) => {
    const payload = JSON.stringify(body);
    const req = getDriver(baseUrl).request(
      makeOpts(baseUrl, urlPath, 'POST', {
        'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(payload),
      }),
      (res) => {
        let data = '';
        res.on('data', (chunk) => (data += chunk));
        res.on('end', () => resolveJson(res, data, resolve, reject));
      }
    );
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => { req.destroy(); reject(new Error('request timeout')); });
    req.write(payload);
    req.end();
  });
}

export function httpPut<T>(baseUrl: string, urlPath: string, body: unknown, timeoutMs = 10000): Promise<T> {
  return new Promise((resolve, reject) => {
    const payload = JSON.stringify(body);
    const req = getDriver(baseUrl).request(
      makeOpts(baseUrl, urlPath, 'PUT', {
        'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(payload),
      }),
      (res) => {
        let data = '';
        res.on('data', (chunk) => (data += chunk));
        res.on('end', () => resolveJson(res, data, resolve, reject));
      }
    );
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => { req.destroy(); reject(new Error('request timeout')); });
    req.write(payload);
    req.end();
  });
}

export function httpDelete(baseUrl: string, urlPath: string, body?: unknown, timeoutMs = 10000): Promise<void> {
  return new Promise((resolve, reject) => {
    const payload = body ? JSON.stringify(body) : '';
    const headers: Record<string, string | number> = {};
    if (body) {
      headers['Content-Type'] = 'application/json';
      headers['Content-Length'] = Buffer.byteLength(payload);
    }
    const req = getDriver(baseUrl).request(makeOpts(baseUrl, urlPath, 'DELETE', headers), (res) => {
      let data = '';
      res.on('data', (chunk) => (data += chunk));
      res.on('end', () => {
        if (!res.statusCode || res.statusCode < 200 || res.statusCode >= 300) {
          reject(new Error(`HTTP ${res.statusCode}: ${data.slice(0, 200)}`));
          return;
        }
        resolve();
      });
    });
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => { req.destroy(); reject(new Error('request timeout')); });
    if (payload) req.write(payload);
    req.end();
  });
}
