import { vi } from 'vitest';

/**
 * Handler answers one mocked route; url carries the query string of the request. body is sent as
 * JSON, text (when given) as is; headers are added to the response.
 */
export type Handler = (init: RequestInit | undefined, url: URL) => { status?: number; body?: unknown; text?: string; headers?: Record<string, string> };

/** Call is one recorded fetch: key is "METHOD /api/v1/path?query", body the parsed JSON body. */
export interface Call {
  key: string;
  method: string;
  path: string;
  query: URLSearchParams;
  body: unknown;
}

// mockFetch routes fetch calls by "METHOD /api/v1/path?query" (exact) or "METHOD /api/v1/path"
// (any query) to handlers and records the calls. Unrouted requests get a 404.
export function mockFetch(routes: Record<string, Handler>) {
  const calls: Call[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const raw = String(input);
    const url = new URL(raw, 'http://localhost');
    const method = init?.method ?? 'GET';
    const key = `${method} ${raw}`;
    calls.push({ key, method, path: url.pathname, query: url.searchParams, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    const h = routes[key] ?? routes[`${method} ${url.pathname}`];
    if (!h) {
      return new Response(JSON.stringify({ message: 'not found' }), { status: 404 });
    }
    const { status = 200, body, text, headers } = h(init, url);
    return new Response(status === 204 ? null : (text ?? JSON.stringify(body ?? {})), { status, headers });
  });
  vi.stubGlobal('fetch', fn);
  return calls;
}

/** callsTo returns the recorded calls for "METHOD /api/v1/path" (any query). */
export function callsTo(calls: Call[], route: string): Call[] {
  const [method, path] = route.split(' ');
  return calls.filter((c) => c.method === method && c.path === path);
}
