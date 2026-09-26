// Deleted *arr integrations (docs/design/phase2-3.md §8.3, S14). Deleting an *arr integration
// keeps a record of its folders: a file in them that no live *arr manages stays unknown, so full,
// until the user confirms the removal. Confirming forgets the record; those files are then
// unmanaged and the tier rules decide them. Nothing is removed from the destinations (S15).

import { api } from './client';

/** DeletedIntegration is a deleted *arr integration whose removal is not confirmed yet. */
export interface DeletedIntegration {
  key: string;
  /** The id the integration had. */
  integrationId: number;
  name: string;
  /** Sonarr, Radarr or Lidarr. */
  app: string;
  /** Its mapped root folders (and item folders outside them), as Bunkarr sees them. */
  folders: string[] | null;
  /** Its root folders (as the *arr sees them) that no path mapping covered. */
  unmapped?: string[] | null;
  deletedAt: string;
}

export async function listDeletedIntegrations(): Promise<DeletedIntegration[]> {
  return (await api<DeletedIntegration[] | null>('/integrations/deleted')) ?? [];
}

/** confirmDeletedIntegration forgets a deleted integration's folders (the user confirmed the removal). */
export function confirmDeletedIntegration(key: string): Promise<void> {
  return api<void>(`/integrations/deleted/${encodeURIComponent(key)}`, { method: 'DELETE' });
}

/**
 * coversPath reports whether a record can decide the file at localPath: one of its folders holds
 * it, or it had a root folder without a path mapping (then any file no live *arr manages may have
 * been its). It mirrors the server's check, so the UI offers the confirmation where it matters.
 */
export function coversPath(d: Pick<DeletedIntegration, 'folders' | 'unmapped'>, localPath: string): boolean {
  if ((d.unmapped ?? []).length > 0) return true;
  return (d.folders ?? []).some((f) => f === '/' || localPath === f || localPath.startsWith(`${f}/`));
}
