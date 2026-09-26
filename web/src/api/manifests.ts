// Manifests (docs/design/phase2-3.md §11, §13): the versions written to a destination, the
// manifest_export job, the verified download of a version and the on-the-spot export. Downloads
// are fetched (fetchManifestFile) so an error answer (429 busy, 409 damaged, 500) is shown to the
// user instead of being saved by the browser as if it were the manifest.

import { api, ApiError, query } from './client';
import type { Job } from './types';

export type ManifestFormat = 'json' | 'csv';

/** ManifestVersion is a recorded manifest version (the API's Manifest). */
export interface ManifestVersion {
  id: number;
  destinationId: number;
  /** The job that wrote the version; 0 once that job's history was deleted. */
  jobId: number;
  createdAt: string;
  /** The version directory, relative to the destination target (.bunkarr/manifests/<version>). */
  path: string;
  format: number;
  itemCount: number;
  fileCount: number;
  /** Size of the files the manifest lists. */
  bytes: number;
  /** "sha256:<hex>" of manifest.json as written. */
  checksum: string;
  /** damaged: the version's files were missing or did not match when read back; not downloadable. */
  integrity: 'ok' | 'damaged';
}

/** ManifestExportStats are a manifest_export job's stats. */
export interface ManifestExportStats {
  dryRun: boolean;
  items: number;
  unlocatedItems: number;
  files: number;
  bytes: number;
  staleIntegrations: number;
  unchanged: boolean;
  damagedFound: number;
  manifestId?: number;
  path?: string;
  versionsPruned: number;
  durationMs: number;
}

export async function listManifests(destinationId: number): Promise<ManifestVersion[]> {
  return (await api<ManifestVersion[] | null>(`/destinations/${destinationId}/manifests`)) ?? [];
}

/** startManifestExport queues a manifest_export job (a dry run writes nothing). */
export function startManifestExport(destinationId: number, opts: { dryRun: boolean }): Promise<Job> {
  return api<Job>(`/destinations/${destinationId}/manifest`, { method: 'POST', body: opts });
}

/** manifestDownloadUrl is the verified download of a recorded version. */
export function manifestDownloadUrl(id: number, format: ManifestFormat): string {
  return `/api/v1/manifests/${id}/download${query({ format })}`;
}

/**
 * manifestExportUrl is a manifest built on the spot: of every enabled source, or of one
 * destination's view (its tiers, kept files and records) when destinationId is given.
 */
export function manifestExportUrl(format: ManifestFormat, destinationId?: number): string {
  return `/api/v1/manifest/export${query({ format, destinationId })}`;
}

/** A fetched download: its body and the file name the server gave it. */
export interface DownloadedFile {
  blob: Blob;
  filename: string;
}

/**
 * filenameFromDisposition reads the file name of a Content-Disposition header (filename*=UTF-8''…
 * first, then filename="…" or filename=token); null when there is none. Only the last path
 * segment is kept.
 */
export function filenameFromDisposition(header: string | null): string | null {
  if (!header) return null;
  let name: string | null = null;
  const ext = /filename\*\s*=\s*([^;]+)/i.exec(header);
  if (ext) {
    const v = ext[1].trim().replace(/^"(.*)"$/, '$1');
    const i = v.indexOf("''");
    try {
      name = decodeURIComponent(i >= 0 ? v.slice(i + 2) : v);
    } catch {
      name = null;
    }
  }
  if (!name) {
    const plain = /filename\s*=\s*("((?:[^"\\]|\\.)*)"|[^;\s]+)/i.exec(header);
    if (plain) {
      name = plain[2] !== undefined ? plain[2].replace(/\\(.)/g, '$1') : plain[1];
    }
  }
  const base = name?.split(/[\\/]/).pop()?.trim();
  return base ? base : null;
}

/**
 * fetchManifestFile downloads href (a manifestDownloadUrl or manifestExportUrl). A failed answer
 * throws ApiError with the server's message (and when to try again for a 429), so nothing is saved.
 */
export async function fetchManifestFile(href: string): Promise<DownloadedFile> {
  const res = await fetch(href, { credentials: 'same-origin' });
  if (!res.ok) {
    let message = `The download failed (HTTP ${res.status})`;
    try {
      const data: unknown = await res.json();
      if (data && typeof data === 'object' && 'message' in data && typeof data.message === 'string' && data.message) {
        message = data.message;
      }
    } catch {
      // not JSON: keep the generic message
    }
    const retry = Number(res.headers.get('Retry-After'));
    if (res.status === 429 && Number.isFinite(retry) && retry > 0) {
      message += ` Try again in ${retry} s.`;
    }
    throw new ApiError(res.status, message);
  }
  const blob = await res.blob();
  const format = new URL(href, 'http://bunkarr.invalid').searchParams.get('format') || 'json';
  return { blob, filename: filenameFromDisposition(res.headers.get('Content-Disposition')) ?? `bunkarr-manifest.${format}` };
}
