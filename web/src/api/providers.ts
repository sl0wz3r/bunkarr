// Tautulli, Seerr and Maintainerr connections and the Plex library index (design phase2-3 §4.4–4.6,
// §6.2–6.3, §13). Keys are write-only (hasApiKey); Maintainerr has no API key at all.

import { api } from './client';
import type { RefreshSettings } from './arr';
import type { Integration, PlexIndexSettings, PlexSettings } from './types';

export type ProviderType = 'tautulli' | 'seerr' | 'maintainerr';

export const PROVIDER_TYPES: ProviderType[] = ['tautulli', 'seerr', 'maintainerr'];

export const PROVIDER_NAMES: Record<ProviderType, string> = { tautulli: 'Tautulli', seerr: 'Seerr', maintainerr: 'Maintainerr' };

export function isProviderType(t: string): t is ProviderType {
  return (PROVIDER_TYPES as string[]).includes(t);
}

/** DEFAULT_REFRESH is each type's refresh default (design §4.2). */
export const DEFAULT_REFRESH: Record<ProviderType, RefreshSettings> = {
  tautulli: { cron: '0 2 * * *', enabled: true, staleAfterHours: 72 },
  seerr: { cron: '30 2 * * *', enabled: true, staleAfterHours: 72 },
  maintainerr: { cron: '45 */6 * * *', enabled: true, staleAfterHours: 24 },
};

/** NEEDS_PLEX says whether a type must be linked to a Plex server (Seerr: optional, for the rating-key fallback). */
export const NEEDS_PLEX: Record<ProviderType, boolean> = { tautulli: true, seerr: false, maintainerr: true };

/** HAS_KEY says whether a type has an API key (Maintainerr has no API authentication). */
export const HAS_KEY: Record<ProviderType, boolean> = { tautulli: true, seerr: true, maintainerr: false };

export interface ProviderSettings {
  /** 0: not linked. */
  plexIntegrationId: number;
  refresh: RefreshSettings;
  [key: string]: unknown;
}

/** providerSettings reads an integration's settings (missing fields take the type's defaults). */
export function providerSettings(it: Integration | null | undefined, type: ProviderType): ProviderSettings {
  const raw = (it?.settings ?? {}) as unknown as Partial<ProviderSettings>;
  return {
    ...raw,
    plexIntegrationId: raw.plexIntegrationId ?? 0,
    refresh: { ...DEFAULT_REFRESH[type], ...(raw.refresh ?? {}) },
  };
}

export interface ProviderIntegrationInput {
  type: ProviderType;
  name: string;
  url: string;
  enabled: boolean;
  /** Empty or missing keeps the stored key; never sent for Maintainerr. */
  apiKey?: string;
  settings: ProviderSettings;
}

export function createProvider(body: ProviderIntegrationInput): Promise<Integration> {
  return api<Integration>('/integrations', { method: 'POST', body });
}

export function updateProvider(id: number, body: ProviderIntegrationInput): Promise<Integration> {
  return api<Integration>(`/integrations/${id}`, { method: 'PUT', body });
}

export interface ProviderTestInput {
  type: ProviderType;
  url: string;
  apiKey?: string;
  id?: number;
  /** Unsaved settings to test with (the linked Plex server). */
  settings?: Partial<ProviderSettings>;
}

export interface ProviderTestResult {
  ok: boolean;
  message: string;
  version?: string;
  appName?: string;
  /** Tautulli, Maintainerr: whether it works with the linked Plex server; null when not linked or unknown. */
  plexMatches: boolean | null;
}

export function testProvider(body: ProviderTestInput): Promise<ProviderTestResult> {
  return api<ProviderTestResult>('/integrations/test', { method: 'POST', body });
}

export interface SeerrUser {
  id: number;
  /** A user name, or "Seerr user #id" (never an e-mail address). */
  label: string;
}

export async function listSeerrUsers(id: number): Promise<SeerrUser[]> {
  return (await api<SeerrUser[] | null>(`/integrations/${id}/seerr/users`)) ?? [];
}

export const DEFAULT_PLEX_INDEX: PlexIndexSettings = { enabled: false, cron: '0 1 * * *', staleAfterHours: 72 };

/** plexIndex reads a Plex integration's library index settings (off by default). */
export function plexIndex(it: Integration | null | undefined): PlexIndexSettings {
  const s = (it?.settings ?? {}) as Partial<PlexSettings>;
  const ix = { ...DEFAULT_PLEX_INDEX, ...(s.index ?? {}) };
  if (!ix.cron) {
    ix.cron = DEFAULT_PLEX_INDEX.cron;
  }
  return ix;
}

/**
 * savePlexIndex saves a Plex integration's library index settings; every other field is sent
 * back as it is (the stored token is kept: no apiKey is sent).
 */
export function savePlexIndex(it: Integration, index: PlexIndexSettings): Promise<Integration> {
  return api<Integration>(`/integrations/${it.id}`, {
    method: 'PUT',
    body: { type: it.type, name: it.name, url: it.url, enabled: it.enabled, settings: { ...it.settings, index } },
  });
}

/** Cache counts of a provider refresh (index_state.stats). */
export interface ProviderCacheStats {
  plexIntegrationId?: number;
  guardHeld?: boolean;
  // Tautulli
  plays?: number;
  ratingKeys?: number;
  sections?: string[] | number;
  sectionsWithoutHistory?: string[];
  usersWithoutHistory?: number;
  // Seerr
  requests?: number;
  counted?: number;
  // Maintainerr
  collections?: number;
  pending?: number;
  undecided?: number;
  plexIndexFresh?: boolean;
  // Plex index
  items?: number;
  files?: number;
  filesUnmapped?: number;
}
