// Sonarr, Radarr and Lidarr connections and the *arr index (design phase2-3 §4, §6, §13). The
// API key and the webhook key are write-only: the only response that carries a webhook key is
// POST /integrations/{id}/webhook/key.

import { api, ApiError, query } from './client';
import type { Integration, Job, Paged } from './types';

export type ArrType = 'sonarr' | 'radarr' | 'lidarr';

export const ARR_TYPES: ArrType[] = ['sonarr', 'radarr', 'lidarr'];

export const ARR_NAMES: Record<ArrType, string> = { sonarr: 'Sonarr', radarr: 'Radarr', lidarr: 'Lidarr' };

export function isArrType(t: string): t is ArrType {
  return (ARR_TYPES as string[]).includes(t);
}

export interface ArrPathMapping {
  /** The folder as the *arr sees it (inside its container). */
  arr: string;
  /** The same folder as Bunkarr sees it. */
  local: string;
}

export interface RefreshSettings {
  cron: string;
  enabled: boolean;
  /** 1–720: the index's facts are unknown this long after its last complete refresh. */
  staleAfterHours: number;
}

/**
 * ArrSettings is an *arr integration's settings. The backup fields are edited elsewhere and are
 * sent back unchanged.
 */
export interface ArrSettings {
  pathMappings: ArrPathMapping[];
  backupFolder?: string;
  backup?: Record<string, unknown>;
  refresh: RefreshSettings;
  [key: string]: unknown;
}

export const DEFAULT_ARR_REFRESH: RefreshSettings = { cron: '15 */6 * * *', enabled: true, staleAfterHours: 24 };

/** arrSettings reads an integration's settings as ArrSettings (missing fields take the defaults). */
export function arrSettings(it: Integration | null | undefined): ArrSettings {
  const raw = (it?.settings ?? {}) as unknown as Partial<ArrSettings>;
  return {
    ...raw,
    pathMappings: raw.pathMappings ?? [],
    refresh: { ...DEFAULT_ARR_REFRESH, ...(raw.refresh ?? {}) },
  };
}

export interface ArrIntegrationInput {
  type: ArrType;
  name: string;
  url: string;
  enabled: boolean;
  /** Empty or missing keeps the stored key. */
  apiKey?: string;
  settings: ArrSettings;
}

export function createArr(body: ArrIntegrationInput): Promise<Integration> {
  return api<Integration>('/integrations', { method: 'POST', body });
}

export function updateArr(id: number, body: ArrIntegrationInput): Promise<Integration> {
  return api<Integration>(`/integrations/${id}`, { method: 'PUT', body });
}

export interface ArrTestInput {
  type: ArrType;
  url: string;
  apiKey?: string;
  id?: number;
  /** Unsaved settings to test with (path mappings, backup folder). */
  settings?: Partial<ArrSettings>;
}

export interface ArrRootFolderCheck {
  path: string;
  accessible: boolean;
  localPath: string | null;
  sourceId: number | null;
  exists: boolean;
  /** Why the folder is not usable ('' when it is). */
  reason: string;
}

export interface ArrRecycleBin {
  path: string;
  sourceId: number | null;
  relPath: string;
  excluded: boolean;
}

export interface ArrTestResult {
  ok: boolean;
  message: string;
  version?: string;
  appName?: string;
  backup?: { folder: 'ok' | 'missing' | 'unreadable' | 'not-set'; http: 'ok' | 'login-required' | 'unknown' };
  manualBackups?: { count: number; bytes: number };
  rootFolders: ArrRootFolderCheck[] | null;
  recycleBin: ArrRecycleBin | null;
  fileDate?: string;
}

export function testArr(body: ArrTestInput): Promise<ArrTestResult> {
  return api<ArrTestResult>('/integrations/test', { method: 'POST', body });
}

/** IndexStats are the counts of the last full refresh (index_state.stats). */
export interface IndexStats {
  items?: number;
  files?: number;
  filesMapped?: number;
  filesUnmapped?: number;
  filesMismatched?: number;
  unmappedFolders?: { rootFolder: string; files: number; reason: string }[];
  inaccessibleRootFolders?: string[];
  recycleBin?: { path: string; localPath: string | null; sourceId: number | null; relPath: string; excluded: boolean } | null;
  fileDate?: string;
  guardHeld?: boolean;
}

/** GET /integrations/{id}/index. */
export interface IndexView {
  status: 'never' | 'ok' | 'failed';
  refreshedAt: string | null;
  attemptedAt: string | null;
  error: string | null;
  appVersion: string;
  stats: IndexStats;
  fresh: boolean;
  instanceMatches: boolean;
  staleAfterHours: number;
  /** Why the index is not fresh. */
  reason?: string;
}

export function getIndex(id: number): Promise<IndexView> {
  return api<IndexView>(`/integrations/${id}/index`);
}

export function refreshIntegration(id: number, body: { dryRun?: boolean; allowChanges?: boolean } = {}): Promise<Job> {
  return api<Job>(`/integrations/${id}/refresh`, { method: 'POST', body });
}

/** revealWebhookKey returns the webhook key; rotate replaces it first (the old one stops working). */
export async function webhookKey(id: number, rotate: boolean): Promise<string> {
  const r = await api<{ key: string }>(`/integrations/${id}/webhook/key`, { method: 'POST', body: { rotate } });
  return r.key;
}

/** WebhookEvent is a received webhook (GET /webhooks/events). */
export interface WebhookEvent {
  id: number;
  integrationId: number | null;
  source: string;
  eventType: string;
  class: string;
  receivedAt: string;
  processedAt: string | null;
  outcome: string | null;
  jobId: number | null;
  truncated: boolean;
  summary: { title?: string; itemIds?: number[]; files?: string[] };
}

/** GET /integrations/{id}/webhook. */
export interface WebhookInfo {
  path: string;
  genericPath: string;
  hasKey: boolean;
  lastEventAt: string | null;
  lastTestAt: string | null;
  last24h: number;
  warnings: string[];
  recent: WebhookEvent[];
}

/** WEBHOOK_POLL_MS is how often an open webhook panel re-reads its activity (a Test pressed in the *arr shows up). */
export const WEBHOOK_POLL_MS = 5000;

/** getWebhookInfo returns the webhook activity, or null while the server does not provide it. */
export async function getWebhookInfo(id: number): Promise<WebhookInfo | null> {
  try {
    return await api<WebhookInfo>(`/integrations/${id}/webhook`);
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) {
      return null;
    }
    throw e;
  }
}

/** webhookPath is the integration's own webhook route. */
export function webhookPath(type: ArrType, id: number): string {
  return `/api/v1/webhook/${type}/${id}`;
}

export type UnmappedReason = 'unmapped' | 'no-source' | 'mismatched';

export interface UnmappedFile {
  integrationId: number;
  path: string;
  localPath?: string;
  size: number;
  reason: UnmappedReason;
  catalogSize?: number;
}

export function listUnmapped(integrationId: number, page: number, pageSize: number): Promise<Paged<UnmappedFile>> {
  return api<Paged<UnmappedFile>>(`/catalog/unmapped${query({ integrationId, page, pageSize })}`);
}

/** The webhook triggers to tick in each app (design §16). */
export const WEBHOOK_TRIGGERS: Record<ArrType, string[]> = {
  radarr: ['On Import', 'On Upgrade', 'On Rename', 'On Movie Added', 'On Movie Delete', 'On Movie File Delete', 'On Movie File Delete For Upgrade'],
  sonarr: ['On Import', 'On Upgrade', 'On Rename', 'On Series Add', 'On Series Delete', 'On Episode File Delete', 'On Episode File Delete For Upgrade'],
  lidarr: ['On Release Import', 'On Upgrade', 'On Rename', 'On Track Retag', 'On Artist Add', 'On Album Delete', 'On Artist Delete'],
};
