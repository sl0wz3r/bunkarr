import { vi } from 'vitest';

type Handler = (init: RequestInit | undefined) => { status?: number; body?: unknown };

// mockFetch routes fetch calls by "METHOD /api/v1/path" to handlers and records the calls.
export function mockFetch(routes: Record<string, Handler>) {
  const calls: { key: string; body: unknown }[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const key = `${init?.method ?? 'GET'} ${String(input)}`;
    calls.push({ key, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    const h = routes[key];
    if (!h) {
      return new Response(JSON.stringify({ message: 'not found' }), { status: 404 });
    }
    const { status = 200, body } = h(init);
    return new Response(status === 204 ? null : JSON.stringify(body ?? {}), { status });
  });
  vi.stubGlobal('fetch', fn);
  return calls;
}
