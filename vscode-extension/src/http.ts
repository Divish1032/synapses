import * as http from 'http';
import * as https from 'https';

function getDriver(baseUrl: string) {
  return baseUrl.startsWith('https') ? https : http;
}

export function httpGet<T>(baseUrl: string, urlPath: string, timeoutMs = 5000): Promise<T> {
  return new Promise((resolve, reject) => {
    const url = new URL(urlPath, baseUrl);
    const driver = getDriver(baseUrl);

    const req = driver.request(
      {
        hostname: url.hostname,
        port: url.port || (baseUrl.startsWith('https') ? 443 : 80),
        path: url.pathname + url.search,
        method: 'GET',
      },
      (res) => {
        let data = '';
        res.on('data', (chunk) => (data += chunk));
        res.on('end', () => {
          try {
            resolve(JSON.parse(data) as T);
          } catch {
            reject(new Error(`JSON parse error: ${data.slice(0, 200)}`));
          }
        });
      }
    );
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => {
      req.destroy();
      reject(new Error('request timeout'));
    });
    req.end();
  });
}

export function httpPost<T>(baseUrl: string, urlPath: string, body: unknown, timeoutMs = 10000): Promise<T> {
  return new Promise((resolve, reject) => {
    const url = new URL(urlPath, baseUrl);
    const payload = JSON.stringify(body);
    const driver = getDriver(baseUrl);

    const req = driver.request(
      {
        hostname: url.hostname,
        port: url.port || (baseUrl.startsWith('https') ? 443 : 80),
        path: url.pathname + url.search,
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(payload),
        },
      },
      (res) => {
        let data = '';
        res.on('data', (chunk) => (data += chunk));
        res.on('end', () => {
          try {
            resolve(JSON.parse(data) as T);
          } catch {
            reject(new Error(`JSON parse error: ${data.slice(0, 200)}`));
          }
        });
      }
    );
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => {
      req.destroy();
      reject(new Error('request timeout'));
    });
    req.write(payload);
    req.end();
  });
}

export function httpDelete(baseUrl: string, urlPath: string, body?: unknown, timeoutMs = 10000): Promise<void> {
  return new Promise((resolve, reject) => {
    const url = new URL(urlPath, baseUrl);
    const payload = body ? JSON.stringify(body) : '';
    const driver = getDriver(baseUrl);

    const req = driver.request(
      {
        hostname: url.hostname,
        port: url.port || (baseUrl.startsWith('https') ? 443 : 80),
        path: url.pathname + url.search,
        method: 'DELETE',
        headers: body
          ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(payload) }
          : {},
      },
      (res) => {
        res.resume();
        res.on('end', () => resolve());
      }
    );
    req.on('error', reject);
    req.setTimeout(timeoutMs, () => {
      req.destroy();
      reject(new Error('request timeout'));
    });
    if (payload) req.write(payload);
    req.end();
  });
}
