// Thin fetch wrapper for /api/v1. The session cookie travels automatically (same origin);
// errors carry the server's {"message": "..."}.

import type { Paged } from './types';

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

export async function api<T>(path: string, init: { method?: string; body?: unknown } = {}): Promise<T> {
  return (await apiResponse<T>(path, init)).data;
}

/** apiResponse is api that also returns the response headers (Date, for example). */
export async function apiResponse<T>(path: string, init: { method?: string; body?: unknown } = {}): Promise<{ data: T; headers: Headers }> {
  const res = await fetch(`/api/v1${path}`, {
    method: init.method ?? 'GET',
    credentials: 'same-origin',
    headers: init.body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  });
  if (res.status === 204) {
    return { data: undefined as T, headers: res.headers };
  }
  let data: unknown = null;
  try {
    data = await res.json();
  } catch {
    data = null;
  }
  if (!res.ok) {
    const msg =
      data && typeof data === 'object' && 'message' in data && typeof data.message === 'string'
        ? data.message
        : `Request failed (HTTP ${res.status})`;
    throw new ApiError(res.status, msg);
  }
  return { data: data as T, headers: res.headers };
}

/** QueryValue is a query-string parameter; empty strings, null and undefined are left out. */
export type QueryValue = string | number | boolean | null | undefined;

/** query builds "?a=1&b=x" from params, skipping empty values; '' when nothing is left. */
export function query(params: Record<string, QueryValue>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') {
      continue;
    }
    q.set(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : '';
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * toPaged reads a paged list response ({page, pageSize, totalRecords, records}, the *arr paging
 * resource). A missing or null records list is an empty page.
 */
export function toPaged<T>(raw: unknown, page: number, pageSize: number): Paged<T> {
  if (!isRecord(raw)) {
    return { page, pageSize, totalRecords: 0, records: [] };
  }
  const records = Array.isArray(raw.records) ? (raw.records as T[]) : [];
  return {
    page: typeof raw.page === 'number' ? raw.page : page,
    pageSize: typeof raw.pageSize === 'number' ? raw.pageSize : pageSize,
    totalRecords: typeof raw.totalRecords === 'number' ? raw.totalRecords : records.length,
    records,
  };
}

/** errorMessage turns anything thrown into a message for the user. */
export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
