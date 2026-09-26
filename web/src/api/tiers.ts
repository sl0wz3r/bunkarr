// Tiers (docs/design/phase2-3.md §8, §13): the ordered rule set and its revision, the presets and
// condition fields of the rule editor, the preview ("what gets backed up and why"), the manual
// flags, the Library item view (GET /catalog/files/{id}) and the release of kept files.
//
// D1 (the user's decision): with no rules every file is full. A preset is only loaded into the
// editor; nothing here saves rules except saveTierRules, which the user triggers.

import { api, query, toPaged } from './client';
import type { Job, Paged } from './types';

/** Tier is what a destination does with a file: copy it, only list it in manifests, or neither. */
export type Tier = 'full' | 'manifest' | 'skip';

export const TIERS: Tier[] = ['full', 'manifest', 'skip'];

/** TierMatch says how a rule's conditions combine. */
export type TierMatch = 'all' | 'any';

/** TierCondition is one test of a rule; value is absent for operators that take none (never). */
export interface TierCondition {
  field: string;
  op: string;
  value?: unknown;
}

/** TierRule is a stored rule (GET /tiers/rules). */
export interface TierRule {
  id: number;
  priority: number;
  name: string;
  enabled: boolean;
  match: TierMatch;
  conditions: TierCondition[];
  action: Tier;
  /** "Applies at": null is every destination, a list only those, [] none. */
  destinationIds: number[] | null;
  createdAt: string;
  updatedAt: string;
}

/** TierRuleInput is a rule as PUT /tiers/rules, the preview draft and the presets carry it. */
export interface TierRuleInput {
  /** Absent (or 0) for a new rule. */
  id?: number;
  name: string;
  enabled?: boolean;
  match?: TierMatch;
  conditions: TierCondition[];
  action: Tier;
  destinationIds: number[] | null;
}

export interface TierRuleSet {
  revision: number;
  rules: TierRule[];
}

/** TierWarning is a rule value no fresh index knows (a renamed tag or profile, §8.7). Indexes are 0-based. */
export interface TierWarning {
  ruleIndex: number;
  conditionIndex: number;
  message: string;
}

export interface TierRulesSaved extends TierRuleSet {
  warnings: TierWarning[];
}

export interface TierPreset {
  id: string;
  name: string;
  description: string;
  rules: TierRuleInput[];
}

export type TierValueType = 'bool' | 'string' | 'int' | 'ints';

export interface TierSuggestion {
  value: unknown;
  label: string;
}

/** TierField is a condition field of the editor (GET /tiers/fields). */
export interface TierField {
  field: string;
  label: string;
  ops: string[];
  valueType: TierValueType;
  /** Operators that take no value (tautulli.lastWatched never). */
  noValueOps?: string[];
  /** bytes, days or plays for an int value. */
  unit?: string;
  /** catalog, arr, plex, tautulli, seerr, maintainerr or flag. */
  source: string;
  available: boolean;
  /** Why the field is not available (D16: its provider is not built yet). */
  reason?: string;
  suggestions: TierSuggestion[];
}

export type TierResult = 'true' | 'false' | 'unknown';

/** TierReason is one evaluated condition (§8.1). A Seerr user appears by id only. */
export interface TierReason {
  ruleId: number;
  /** -1 for the built-in irreplaceable override. */
  conditionIndex: number;
  field: string;
  op: string;
  value?: unknown;
  /** The fact compared: a tag list, a profile, a size, a date, plays. */
  actual?: unknown;
  result: TierResult;
  source: { kind: string; integrationId?: number };
  /** Why the result is unknown ("Radarr cache is 31 h old"). */
  why?: string;
}

/** TierCount is a number of files and their bytes. */
export interface TierCount {
  files: number;
  bytes: number;
}

export interface TierFullCount extends TierCount {
  /** A hardlink group's content counted once. */
  uniqueBytes: number;
}

export interface TierRuleCount {
  /** 0 for the built-ins; a draft's new rules have the negative of their position (-1 first). */
  ruleId: number;
  name: string;
  action: Tier;
  files: number;
  bytes: number;
}

export interface TierConfigBackup {
  kind: 'plexdb' | 'arr';
  integrationId: number;
  name: string;
  lastBytes: number;
}

export interface TierDestinationPreview {
  destinationId: number;
  name: string;
  stored: TierCount;
  full: TierFullCount;
  manifest: TierCount;
  skip: TierCount;
  unknownPromoted: TierCount;
  toCopy: TierCount;
  kept: TierCount;
  movedToNonFull: TierCount;
  byRule: TierRuleCount[] | null;
  configBackups: TierConfigBackup[] | null;
}

export interface TierUnknownSource {
  integrationId: number;
  name: string;
  reason: string;
}

/** TierPreview is POST /tiers/preview: the saved rules (revision) or a draft ("draft"). */
export interface TierPreview {
  id: string;
  revision: number | 'draft';
  createdAt: string;
  unknownSources: TierUnknownSource[] | null;
  staleReferences: TierWarning[] | null;
  destinations: TierDestinationPreview[] | null;
}

export type TierItemState = 'stored' | 'to-copy' | 'kept' | 'not-copied';

export interface TierPreviewItem {
  destinationId: number;
  fileId: number;
  sourceId: number;
  sourceName: string;
  relPath: string;
  size: number;
  tier: Tier;
  ruleId: number;
  ruleName: string;
  reasons: TierReason[] | null;
  unknown: TierReason[] | null;
  unknownPromoted: boolean;
  /** The file whose decision this is (a sidecar's media file, a hardlink group's name). */
  follows?: string;
  state: TierItemState;
}

export interface TierPreviewItemQuery {
  destinationId?: number;
  tier?: Tier | '';
  /** The deciding rule (0: the built-ins); undefined for every rule. */
  ruleId?: number;
  state?: TierItemState | '';
  search?: string;
  page: number;
  pageSize: number;
}

export interface ExternalIds {
  tmdb?: number;
  imdb?: string;
  tvdb?: number;
  tvmaze?: number;
  mbid?: string;
}

/** ItemFlag is a manual flag (GET /tiers/flags): an *arr item or a path. */
export interface ItemFlag {
  id: number;
  flag: 'irreplaceable';
  kind: 'arr' | 'path';
  integrationId: number | null;
  arrKind?: string;
  arrId?: number;
  externalIds: ExternalIds;
  lastSourceId: number | null;
  lastRelPath: string | null;
  sourceId: number | null;
  /** '' is the whole source. */
  relPath: string | null;
  note: string;
  createdAt: string;
  updatedAt: string;
  resolved: boolean;
  reason?: string;
}

export type FlagTarget = { integrationId: number; kind: string; arrId: number } | { sourceId: number; relPath: string };

export interface FlagInput {
  flag: 'irreplaceable';
  target: FlagTarget;
  note: string;
}

// ---- The Library item view (GET /catalog/files/{id}) ----

export interface ArrItemFacts {
  integrationId: number;
  app: string;
  itemId: number;
  /** movie, series or artist. */
  kind: string;
  arrId: number;
  title: string;
  year: number;
  externalIds: ExternalIds;
  tags: string[] | null;
  qualityProfile: string;
  qualityProfileId: number;
  rootFolder: string;
  monitored: boolean;
  genres: string[] | null;
  folder?: string;
}

export interface ArrFacts {
  state: 'item' | 'unmanaged' | 'unknown';
  why?: string;
  integrationId?: number;
  item?: ArrItemFacts;
  /** Attributed by folder (an extra): episode-level facts are unknown. */
  byFolder?: boolean;
  arrFileId?: number;
  dateAdded?: string;
  quality?: string;
}

export interface PlexFacts {
  known: boolean;
  why?: string;
  integrationId?: number;
  section?: string;
  addedAt?: string;
}

export interface WatchFacts {
  known: boolean;
  why?: string;
  integrationId?: number;
  plays: number;
  lastWatched?: string;
  lowerBound?: boolean;
}

export interface RequestFacts {
  requested: TierResult;
  why?: string;
  integrationId?: number;
  users: number[] | null;
}

export interface MaintainerrFacts {
  pending: TierResult;
  why?: string;
  integrationId?: number;
  deleteAfter?: string;
}

export interface FileFacts {
  arr: ArrFacts;
  /** null: nothing supplies these facts (their conditions are unknown). */
  plex: PlexFacts | null;
  watch: WatchFacts | null;
  requests: RequestFacts | null;
  maintainerr: MaintainerrFacts | null;
  /** The irreplaceable flags that cover the file. */
  flags: number[] | null;
  /** A sidecar's media file, whose decision it takes. */
  follows?: string;
  unknown: { source: string; reason: string }[] | null;
}

export interface FileRecord {
  state: string;
  relPath: string;
  size: number;
  copiedAt: string | null;
  verifiedAt: string | null;
}

export interface FileTierAtDestination {
  destinationId: number;
  destinationName: string;
  tier: Tier;
  ruleId: number;
  ruleName: string;
  reasons: TierReason[] | null;
  unknown: TierReason[] | null;
  unknownPromoted: boolean;
  follows?: string;
  record: FileRecord | null;
}

export interface FileDetail {
  file: { id: number; relPath: string; size: number; mtime: string; hardlinkGroup: string | null };
  source: { id: number; name: string };
  facts: FileFacts;
  tiers: FileTierAtDestination[] | null;
}

// ---- Calls ----

export function getTierRules(): Promise<TierRuleSet> {
  return api<TierRuleSet>('/tiers/rules');
}

/** saveTierRules replaces the whole ordered set; a revision other than the stored one is a 409. */
export function saveTierRules(revision: number, rules: TierRuleInput[]): Promise<TierRulesSaved> {
  return api<TierRulesSaved>('/tiers/rules', { method: 'PUT', body: { revision, rules } });
}

export async function getTierPresets(): Promise<TierPreset[]> {
  return (await api<TierPreset[] | null>('/tiers/presets')) ?? [];
}

export async function getTierFields(): Promise<TierField[]> {
  return ((await api<TierField[] | null>('/tiers/fields')) ?? []).map(uniqueSuggestions);
}

/**
 * uniqueSuggestions lists each suggested value once. Several suppliers can offer the same value (a
 * Plex section from a source's setting and from the library index; a user id that two Seerr
 * servers share): one entry keeps the distinct labels, so the editor has one option (and one React
 * key) per value, and every label the value stands for.
 */
export function uniqueSuggestions(field: TierField): TierField {
  const byValue = new Map<string, TierSuggestion>();
  for (const s of field.suggestions ?? []) {
    const k = `${typeof s.value}:${String(s.value)}`;
    const seen = byValue.get(k);
    if (!seen) {
      byValue.set(k, { ...s });
    } else if (!seen.label.split(' / ').includes(s.label)) {
      seen.label = `${seen.label} / ${s.label}`;
    }
  }
  return { ...field, suggestions: [...byValue.values()] };
}

/** previewTiers evaluates the saved rules (rules undefined) or a draft; nothing is written. */
export function previewTiers(body: { rules?: TierRuleInput[]; destinationIds?: number[] }): Promise<TierPreview> {
  return api<TierPreview>('/tiers/preview', { method: 'POST', body });
}

/** listPreviewItems pages a preview's files; an expired preview (10 min) is a 404. */
export async function listPreviewItems(id: string, q: TierPreviewItemQuery): Promise<Paged<TierPreviewItem>> {
  const raw = await api<unknown>(`/tiers/preview/${encodeURIComponent(id)}/items${query({ ...q })}`);
  return toPaged<TierPreviewItem>(raw, q.page, q.pageSize);
}

export async function listFlags(): Promise<ItemFlag[]> {
  return (await api<ItemFlag[] | null>('/tiers/flags')) ?? [];
}

export function addFlag(body: FlagInput): Promise<ItemFlag> {
  return api<ItemFlag>('/tiers/flags', { method: 'POST', body });
}

export function deleteFlag(id: number): Promise<void> {
  return api<void>(`/tiers/flags/${id}`, { method: 'DELETE' });
}

export function getCatalogFile(id: number): Promise<FileDetail> {
  return api<FileDetail>(`/catalog/files/${id}`);
}

// ---- Release (§8.5, §12.1) ----

/** ReleaseParams are the release fields of POST /destinations/{id}/sync. */
export interface ReleaseParams {
  releaseDemoted?: boolean;
  releaseOf?: number;
  releaseRevision?: number;
}

export interface SyncRequest extends ReleaseParams {
  dryRun: boolean;
  allowChanges: boolean;
}

/** startSync queues a sync of a destination with the release fields when given. */
export function startSync(destinationId: number, body: SyncRequest): Promise<Job> {
  return api<Job>(`/destinations/${destinationId}/sync`, { method: 'POST', body });
}

/**
 * startReleasePreview queues the dry run that lists the kept files a release would free. Nothing
 * is changed; its job is where "Apply release" confirms it.
 */
export function startReleasePreview(destinationId: number): Promise<Job> {
  return startSync(destinationId, { dryRun: true, allowChanges: false, releaseDemoted: true });
}

/** tierRevisionOf reads stats.tierRevision of a sync (0 when it recorded none). */
export function tierRevisionOf(job: Pick<Job, 'stats'>): number {
  const v = job.stats?.tierRevision;
  return typeof v === 'number' && Number.isInteger(v) && v > 0 ? v : 0;
}

/** isReleasePreview reports whether a job is a release preview (a dry-run sync with releaseDemoted). */
export function isReleasePreview(job: Pick<Job, 'type' | 'dryRun' | 'params'>): boolean {
  return job.type === 'sync' && job.dryRun && !!job.params?.releaseDemoted;
}

/**
 * releaseParamsOf gives a follow-up real sync of job the release it stands for: a release preview
 * is confirmed by its own id and the revision it evaluated; a real release (whose held changes are
 * applied) passes on the preview and revision it confirmed. Any other job gives none.
 */
export function releaseParamsOf(job: Pick<Job, 'id' | 'type' | 'dryRun' | 'params' | 'stats'>): ReleaseParams {
  if (job.type !== 'sync' || !job.params?.releaseDemoted) {
    return {};
  }
  if (job.dryRun) {
    return { releaseDemoted: true, releaseOf: job.id, releaseRevision: tierRevisionOf(job) };
  }
  return { releaseDemoted: true, releaseOf: job.params.releaseOf, releaseRevision: job.params.releaseRevision };
}

/**
 * applyRelease starts the real release of a finished release preview: it runs what the guard
 * would hold (allowChanges) and releases only that preview's records, and only while the rules
 * are still at the revision it evaluated (else 409).
 */
export function applyRelease(preview: Pick<Job, 'id' | 'type' | 'dryRun' | 'params' | 'stats'>): Promise<Job> {
  const destinationId = preview.params?.destinationId;
  if (!destinationId || !isReleasePreview(preview)) {
    return Promise.reject(new Error('This job is not a release preview.'));
  }
  const revision = tierRevisionOf(preview);
  if (!revision) {
    return Promise.reject(new Error('This preview recorded no rule revision; run the release preview again.'));
  }
  return startSync(destinationId, { dryRun: false, allowChanges: true, ...releaseParamsOf(preview) });
}
