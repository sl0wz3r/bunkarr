// Destinations (design §7 "Destinations"; docs/design/phase4.md §12 for the restic and rclone
// destinations, their host keys, recovery kit and maintenance).

import { api, ApiError } from './client';
import type { Destination, DestinationInput, DestinationTestResult, EngineTestInput, EngineTestResult, HostKeyInfo, Job, Snapshot } from './types';

export async function listDestinations(): Promise<Destination[]> {
  return (await api<Destination[] | null>('/destinations')) ?? [];
}

export function createDestination(body: DestinationInput): Promise<Destination> {
  return api<Destination>('/destinations', { method: 'POST', body });
}

/** updateDestination saves changes; kind, engine, location and encryption never change (400). */
export function updateDestination(id: number, body: Partial<DestinationInput>): Promise<Destination> {
  return api<Destination>(`/destinations/${id}`, { method: 'PUT', body });
}

/**
 * deleteDestination forgets a destination; the backup data at the target stays. confirmLoseSecret
 * is required (409 otherwise) when its recovery kit custody was never confirmed: the encryption
 * secret is deleted with the row (S21).
 */
export function deleteDestination(id: number, opts: { confirmLoseSecret?: boolean } = {}): Promise<void> {
  return api<void>(`/destinations/${id}${opts.confirmLoseSecret ? '?confirmLoseSecret=true' : ''}`, { method: 'DELETE' });
}

/** testTarget probes a filecopy target before a destination is created. */
export function testTarget(target: string): Promise<DestinationTestResult> {
  return api<DestinationTestResult>('/destinations/test', { method: 'POST', body: { target } });
}

/**
 * testConnection tests the connection of a restic or rclone destination to create (§4.5). It
 * never takes an id and uses only the body's credentials; an SFTP connection without pinned host
 * keys runs no command and answers the presented keys.
 */
export function testConnection(body: EngineTestInput): Promise<EngineTestResult> {
  return api<EngineTestResult>('/destinations/test', { method: 'POST', body });
}

/** testDestination probes an existing destination (filecopy: marker, capabilities, free space). */
export function testDestination(id: number): Promise<DestinationTestResult> {
  return api<DestinationTestResult>(`/destinations/${id}/test`, { method: 'POST' });
}

/** testStoredDestination tests a restic or rclone destination with its stored location and secrets. */
export function testStoredDestination(id: number): Promise<EngineTestResult> {
  return api<EngineTestResult>(`/destinations/${id}/test`, { method: 'POST' });
}

/** fetchHostKeys lists the keys an SFTP server presents, for the user to confirm and pin (§4.6). */
export async function fetchHostKeys(host: string, port: number): Promise<HostKeyInfo[]> {
  return (await api<HostKeyInfo[] | null>('/destinations/sftp/hostkeys', { method: 'POST', body: { host, port } })) ?? [];
}

export function syncDestination(id: number, opts: { dryRun: boolean; allowChanges: boolean }): Promise<Job> {
  return api<Job>(`/destinations/${id}/sync`, { method: 'POST', body: opts });
}

/** verifyDestination queues a verify; readData reads everything once (restic check --read-data). */
export function verifyDestination(id: number, opts: { dryRun?: boolean; readData?: boolean } = {}): Promise<Job> {
  return api<Job>(`/destinations/${id}/verify`, { method: 'POST', body: Object.keys(opts).length ? opts : undefined });
}

/** runRetention queues the destination's retention now; prune makes a restic repository prune in this run. */
export function runRetention(id: number, opts: { dryRun?: boolean; prune?: boolean } = {}): Promise<Job> {
  return api<Job>(`/destinations/${id}/retention`, { method: 'POST', body: opts });
}

/**
 * unlockDestination removes a restic repository's stale locks, or with removeAll every lock
 * (409 while any job of the destination is running or waiting for its window, §6.7).
 */
export function unlockDestination(id: number, removeAll: boolean): Promise<void> {
  return api<void>(`/destinations/${id}/unlock`, { method: 'POST', body: { removeAll } });
}

export async function listSnapshots(id: number): Promise<Snapshot[]> {
  return (await api<Snapshot[] | null>(`/destinations/${id}/snapshots`)) ?? [];
}

/** RecoveryKit is an exported kit: the attachment's file name and its text. */
export interface RecoveryKit {
  filename: string;
  text: string;
}

/** kitFilename reads the file name of a Content-Disposition attachment header (fallback when absent). */
export function kitFilename(disposition: string | null, fallback = 'bunkarr-recovery-kit.txt'): string {
  if (!disposition) return fallback;
  const star = /filename\*\s*=\s*(?:UTF-8|utf-8)''([^;]+)/.exec(disposition);
  if (star) {
    try {
      return decodeURIComponent(star[1].trim());
    } catch {
      return fallback;
    }
  }
  const plain = /filename\s*=\s*("((?:[^"\\]|\\.)*)"|[^;\s]+)/.exec(disposition);
  if (!plain) return fallback;
  const name = plain[2] !== undefined ? plain[2].replace(/\\(.)/g, '$1') : plain[1];
  // Only a plain file name is used: never a path.
  const base = name.split(/[\\/]/).pop()?.trim();
  return base || fallback;
}

/**
 * exportRecoveryKit downloads the destination's recovery kit (§5.2): the only response that
 * carries its encryption secret. It needs a login session and the user's password (403 for the
 * API key and the local-address bypass, 400 for a wrong password). The kit is returned for the
 * caller to save; it is never kept anywhere else.
 */
export async function exportRecoveryKit(id: number, body: { currentPassword: string; includeStorageCredentials: boolean }): Promise<RecoveryKit> {
  const res = await fetch(`/api/v1/destinations/${id}/recovery-kit`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    let msg = `Request failed (HTTP ${res.status})`;
    try {
      const data: unknown = await res.json();
      if (data && typeof data === 'object' && 'message' in data && typeof data.message === 'string') {
        msg = data.message;
      }
    } catch {
      // keep the generic message
    }
    throw new ApiError(res.status, msg);
  }
  return { filename: kitFilename(res.headers.get('Content-Disposition')), text: await res.text() };
}

/**
 * confirmRecoveryKit proves custody (§5.2): the kit's check code, or the encryption password the
 * user typed at create, typed again. 204; a wrong one is 400 and rate limited like logins.
 */
export function confirmRecoveryKit(id: number, body: { checkCode: string } | { secret: string }): Promise<void> {
  return api<void>(`/destinations/${id}/recovery-kit/confirm`, { method: 'POST', body });
}
