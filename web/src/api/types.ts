// API types. Phase 0: auth, settings, status. Phase 1: the contract in docs/design/phase1.md §7
// and internal/jobs/contract.go (Job, Item, Progress, ItemCount). Times are RFC 3339 strings,
// sizes are bytes.

export type AuthRequired = 'enabled' | 'disabled_for_local_addresses';

export interface AuthStatus {
  setupRequired: boolean;
  authenticated: boolean;
  via?: 'apikey' | 'session' | 'local';
  username: string | null;
  authenticationRequired: AuthRequired;
}

export interface SystemStatus {
  appName: string;
  version: string;
  commit: string;
  buildDate: string;
  startTime: string;
  uptimeSeconds: number;
  databasePath: string;
  schemaVersion: number;
  configDir: string;
  goVersion: string;
  os: string;
  arch: string;
  isDocker: boolean;
  authenticationMethod: string;
  authenticationRequired: AuthRequired;
}

export interface GeneralSettings {
  apiKey: string;
  authenticationRequired: AuthRequired;
  authenticationMethod: string;
  bindAddress: string;
  port: number;
}

/** Paged is one page of a list endpoint (`page` is 1-based). */
export interface Paged<T> {
  page: number;
  pageSize: number;
  totalRecords: number;
  records: T[];
}

// ---- Jobs (internal/jobs/contract.go) ----

export type JobType = 'scan' | 'sync' | 'plexdb_backup' | 'retention' | 'verify' | 'refresh' | 'arr_backup' | 'manifest_export';

export type JobStatus = 'queued' | 'running' | 'completed' | 'completed_with_warnings' | 'failed' | 'cancelled';

export type JobTrigger = 'schedule' | 'manual' | 'webhook' | 'resume' | 'startup';

export interface JobParams {
  destinationId?: number;
  sourceIds?: number[];
  integrationId?: number;
  allowChanges?: boolean;
  /** A targeted sync: only these paths inside its one source. */
  paths?: string[];
  /** A targeted refresh: only these *arr items (the *arr's own ids). */
  arrItemIds?: number[];
  /** A refresh queues follow-up syncs of the changed items' folders when it ends. */
  syncAfter?: boolean;
  /** A sync that releases the kept files no longer full at the destination (tiers, S15). */
  releaseDemoted?: boolean;
  /** A real release: the release preview (dry run) it applies. */
  releaseOf?: number;
  /** A real release: the tier rule revision that preview evaluated. */
  releaseRevision?: number;
}

export interface JobProgress {
  phase?: string;
  filesTotal: number;
  filesDone: number;
  bytesTotal: number;
  bytesDone: number;
  currentFile?: string;
  bytesPerSec: number;
  etaSeconds: number;
}

export interface Job {
  id: number;
  type: JobType;
  status: JobStatus;
  trigger: JobTrigger;
  dryRun: boolean;
  params: JobParams;
  attempt: number;
  progress: JobProgress;
  /** Runner-specific counters (see SyncStats); null until the job finishes. */
  stats: Record<string, unknown> | null;
  warnings: number;
  summary: string;
  error?: string;
  queuedAt: string;
  startedAt: string | null;
  finishedAt: string | null;
}

/** SyncSourceSummary is one source's part of a sync (SyncStats.sources). */
export interface SyncSourceSummary {
  sourceId: number;
  name: string;
  files: number;
  added: number;
  changed: number;
  deleted: number;
  skipped: number;
  changes: number;
  held: number;
}

/** SyncStats are the stats of a sync job (design §6.1, internal/syncer). */
export interface SyncStats {
  dryRun: boolean;
  filesPlanned: number;
  filesCopied: number;
  filesUpdated: number;
  filesMoved: number;
  filesAdopted: number;
  filesLinked: number;
  filesPromoted: number;
  filesRetained: number;
  filesDisplaced: number;
  filesHeld: number;
  filesFailed: number;
  filesSkipped: number;
  bytesPlanned: number;
  bytesCopied: number;
  durationMs: number;
  /** Per source; empty when a resumed job skipped scanning and planning. */
  sources: SyncSourceSummary[];
}

export type ItemAction = 'copy' | 'update' | 'move' | 'adopt' | 'link' | 'promote' | 'retain' | 'expire' | 'verify' | 'backup' | 'skip';

export type ItemStatus = 'pending' | 'done' | 'failed' | 'skipped' | 'held';

export interface JobItem {
  id: number;
  jobId: number;
  fileId?: number;
  relPath: string;
  action: ItemAction;
  status: ItemStatus;
  bytes: number;
  error?: string;
  detail?: Record<string, unknown> | null;
}

/** JobListQuery filters GET /jobs. */
export interface JobListQuery {
  state: 'active' | 'finished';
  type?: JobType | '';
  status?: JobStatus | '';
  page: number;
  pageSize: number;
}

/** ItemListQuery filters GET /jobs/{id}/items. */
export interface ItemListQuery {
  action?: ItemAction | '';
  status?: ItemStatus | '';
  /** The tier decision an item records (an item that records none is full). */
  tier?: 'full' | 'manifest' | 'skip' | '';
  /** The deciding tier rule an item records (0: a built-in); undefined for every rule. */
  ruleId?: number;
  page: number;
  pageSize: number;
}

export interface ItemCount {
  action: ItemAction;
  status: ItemStatus;
  files: number;
  bytes: number;
}

/** TierItemCount is a row of GET /jobs/{id}/items/summary?by=tier. */
export interface TierItemCount extends ItemCount {
  tier: 'full' | 'manifest' | 'skip';
}

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

export interface JobLog {
  id: number;
  at: string;
  level: LogLevel;
  message: string;
  fields: Record<string, unknown> | null;
}

export interface Schedule {
  id: number;
  jobType: JobType;
  params: JobParams;
  description: string;
  cron: string;
  enabled: boolean;
  lastRunAt: string | null;
  /** null when disabled or blocked. */
  nextRunAt: string | null;
  /** Why the scheduler refuses the schedule's jobs (its destination or Plex server is disabled); '' when they can run. */
  blockedReason: string;
}

/** CronSchedule is a destination's (or Plex backup's) schedule. */
export interface CronSchedule {
  cron: string;
  enabled: boolean;
}

// ---- Integrations ----

export type IntegrationType = 'plex' | 'sonarr' | 'radarr' | 'lidarr' | 'tautulli' | 'seerr' | 'maintainerr';

export interface PathMapping {
  plex: string;
  local: string;
}

export interface PlexBackupSettings {
  /** 0 or missing: no backup destination chosen. */
  destinationId: number;
  cron: string;
  enabled: boolean;
}

/** PlexIndexSettings configures a Plex integration's library index (design phase2-3 §6.3). */
export interface PlexIndexSettings {
  enabled: boolean;
  cron: string;
  /** 1–720: the index's facts are unknown this long after its last complete refresh. */
  staleAfterHours: number;
}

export interface PlexSettings {
  dataPath: string;
  pathMappings: PathMapping[];
  backup: PlexBackupSettings;
  /** Absent: the library index is off. */
  index?: PlexIndexSettings;
}

export interface Integration {
  id: number;
  type: IntegrationType;
  name: string;
  url: string;
  enabled: boolean;
  hasApiKey: boolean;
  settings: PlexSettings;
  createdAt: string;
  updatedAt: string;
}

/** IntegrationInput is the body of POST/PUT /integrations; an empty apiKey keeps the stored one. */
export interface IntegrationInput {
  type: IntegrationType;
  name: string;
  url: string;
  enabled: boolean;
  apiKey?: string;
  /** Update only: remove the stored token. */
  clearApiKey?: boolean;
  settings: PlexSettings;
  /** Plex only, in place of apiKey: the token of a server chosen in "Sign in with Plex". */
  plexSignIn?: PlexSignInRef;
}

export interface IntegrationTestInput {
  type: IntegrationType;
  url: string;
  apiKey?: string;
  id?: number;
  /** Test with the token of a server chosen in "Sign in with Plex" (the sign-in is not used up). */
  plexSignIn?: PlexSignInRef;
}

// ---- Plex sign-in (design §5): no response ever carries a token ----

/** PlexSignInRef names a signed-in server whose token the server side uses. */
export interface PlexSignInRef {
  id: string;
  serverId: string;
  /** Owned servers without a token of their own: use the plex.tv account token. */
  useAccountToken?: boolean;
}

/** POST /plex/signin: the PIN code appears only inside authUrl. */
export interface PlexSignInCreated {
  id: string;
  authUrl: string;
  expiresAt: string;
}

export type PlexSignInStatusValue = 'pending' | 'authenticated' | 'expired';

/** GET /plex/signin/{id}. */
export interface PlexSignInStatus {
  status: PlexSignInStatusValue;
  expiresAt: string;
  username?: string;
  warning?: string;
}

export interface PlexConnectionChoice {
  uri: string;
  protocol: string;
  address: string;
  port: number;
  local: boolean;
  relay: boolean;
  ipv6: boolean;
}

/** GET /plex/signin/{id}/servers (owned first). */
export interface PlexServerChoice {
  id: string;
  name: string;
  owned: boolean;
  productVersion: string;
  platform: string;
  hasAccessToken: boolean;
  connections: PlexConnectionChoice[];
}

/** One tested connection of POST /plex/signin/{id}/servers/{serverId}/test. */
export interface PlexProbeResult {
  uri: string;
  local: boolean;
  relay: boolean;
  derived: boolean;
  protocol: string;
  ok: boolean;
  identityMatches: boolean;
  tokenAccepted: boolean;
  version?: string;
  latencyMs: number;
  message: string;
}

export interface PlexProbeAnswer {
  /** A working https, non-relay connection; null when none. */
  recommended: string | null;
  /** Best first. */
  results: PlexProbeResult[];
}

export interface IntegrationTestResult {
  ok: boolean;
  message: string;
  version?: string;
  machineIdentifier?: string;
  /** Plex butler window, when the server reports it (GET /:/prefs ButlerStartHour/EndHour). */
  butlerStartHour?: number;
  butlerEndHour?: number;
}

export interface PlexLocation {
  /** The folder as Plex sees it. */
  path: string;
  /** The same folder as Bunkarr sees it, after the path mappings ('' when no mapping applies). */
  localPath: string;
  exists: boolean;
}

export interface PlexSection {
  key: string;
  title: string;
  type: string;
  locations: PlexLocation[];
}

// ---- Sources and catalog ----

export interface SourceStats {
  files: number;
  bytes: number;
  uniqueBytes: number;
  hardlinkGroups: number;
  hardlinkedFiles: number;
  skipped: number;
}

export type ScanStatus = 'ok' | 'failed' | 'warnings';

/** Source is a media folder Bunkarr backs up. Unset strings are '' (not null). */
export interface Source {
  id: number;
  name: string;
  path: string;
  destFolder: string;
  exclude: string[];
  enabled: boolean;
  plexIntegrationId: number | null;
  /** '' when the source was not imported from Plex. */
  plexSectionId: string;
  plexPath: string;
  arrIntegrationId: number | null;
  /** '' until the first successful scan after the source was saved. */
  fsType: string;
  lastScanAt: string | null;
  /** '' when never scanned. */
  lastScanStatus: ScanStatus | '';
  stats: SourceStats;
  createdAt: string;
  updatedAt: string;
}

/**
 * SourceInput is the body of POST/PUT /sources. On PUT the Plex and *arr links are replaced:
 * leaving them out clears them.
 */
export interface SourceInput {
  name: string;
  path: string;
  /** Empty: the server derives it from the name (create) or keeps it (update). */
  destFolder?: string;
  exclude: string[];
  enabled: boolean;
  plexIntegrationId?: number | null;
  plexSectionId?: string;
  plexPath?: string;
  arrIntegrationId?: number | null;
}

export interface SourceTestResult {
  ok: boolean;
  /** The tested path, resolved. */
  path: string;
  exists: boolean;
  isDir: boolean;
  fsType: string;
  fuse: boolean;
  entries: number;
  message: string;
  warnings: string[] | null;
}

export type FileFilter = 'all' | 'hardlinked' | 'deleted';

export interface CatalogFile {
  id: number;
  relPath: string;
  size: number;
  mtime: string;
  hardlinkGroup: string | null;
  nlink: number;
  deleted: boolean;
  /** When a scan no longer found the file (deleted files only). */
  deletedAt?: string;
}

/** FileListQuery filters GET /sources/{id}/files. */
export interface FileListQuery {
  page: number;
  pageSize: number;
  search?: string;
  filter?: FileFilter;
}

export interface CatalogStats {
  sources: number;
  files: number;
  bytes: number;
  uniqueBytes: number;
  hardlinkGroups: number;
  hardlinkedFiles: number;
}

export interface DirEntry {
  name: string;
  path: string;
}

export interface DirListing {
  path: string;
  /** '' at the filesystem root. */
  parent: string;
  directories: DirEntry[] | null;
  /** More subdirectories exist than the server lists. */
  truncated?: boolean;
}

// ---- Destinations ----

export interface Capabilities {
  hardlinks: boolean;
  /**
   * Inode numbers do not tell whether two names are one file (a CIFS mount with noserverino):
   * such names are compared by content. Jobs check it again before they compare two names.
   */
  unstableInodes: boolean;
  caseInsensitive: boolean;
  invalidChars: string;
  trailingDotSpace: boolean;
  mtimeGranularityNs: number;
  fsType: string;
  checkedAt: string;
  /** The probe version that found these (0 or missing: an older one; the next job probes again). */
  probeVersion: number;
  /**
   * The destination keeps the permission bits a file is given (not an SMB share without POSIX
   * extensions). Missing or false until a probe that checks it: *arr backups need it or their
   * integration's acceptInsecureModes.
   */
  enforcesModes?: boolean;
}

export type VerifyMode = 'off' | 'sample' | 'full';
export type HardlinkMode = 'recreate' | 'copy';
export type AdoptMode = 'size+mtime' | 'size+hash' | 'off';

export interface DestinationSettings {
  verify: { mode: VerifyMode; samplePercent: number };
  hardlinks: HardlinkMode;
  adoptExisting: AdoptMode;
  mtimeWindowSec: number;
  maxChangePercent: number;
  maxChangeFiles: number;
}

export interface Retention {
  deletedDays: number;
  plexDbDaily: number;
  plexDbWeekly: number;
  /** *arr backup versions kept per integration (1–365; the server fills 14 when missing). */
  arrDaily?: number;
  /** ISO weeks that keep their newest *arr backup version (1–520; default 8). */
  arrWeekly?: number;
  /** Days that keep their newest manifest version (1–3650; the server fills 30 when missing). */
  manifestDays?: number;
  /** ISO weeks that keep their newest manifest version (0–520, 0 keeps none; missing is 12). */
  manifestWeeks?: number;
}

export interface Destination {
  id: number;
  name: string;
  engine: 'filecopy';
  target: string;
  enabled: boolean;
  sourceIds: number[];
  fsType: string;
  /** The probe's findings (design §3); zero values until the first probe. */
  capabilities: Partial<Capabilities> | null;
  /** {cron: '', enabled: false} when the destination has no schedule of that kind. */
  schedule: CronSchedule;
  verifySchedule: CronSchedule;
  settings: DestinationSettings;
  retention: Retention;
  /** The newest job of any type that worked on the destination. */
  lastJob: Job | null;
  /** The newest sync (queued, running or finished; previews excluded): the last sync status. */
  lastSync: Job | null;
  createdAt: string;
  updatedAt: string;
}

export interface DestinationInput {
  name: string;
  engine: 'filecopy';
  target: string;
  enabled: boolean;
  sourceIds: number[];
  schedule: CronSchedule;
  verifySchedule: CronSchedule;
  settings: DestinationSettings;
  retention: Retention;
  /** Create only: adopt the id of an existing .bunkarr/destination.json marker. */
  attach?: boolean;
  /** Create only: accept a target on the root/config filesystem or on tmpfs/overlay. */
  allowLocal?: boolean;
}

export type MarkerStatus = 'ok' | 'missing' | 'mismatch' | 'foreign';

export interface DestinationTestResult {
  ok: boolean;
  marker: MarkerStatus;
  writable: boolean;
  fsType: string;
  local: boolean;
  capabilities: Partial<Capabilities> | null;
  freeBytes: number;
  totalBytes: number;
  entries: number;
  message: string;
  warnings: string[] | null;
}

export interface Snapshot {
  id: number;
  destinationId: number;
  /** plexdb: a Plex DB version; arr: an *arr backup zip (never downloadable). */
  kind?: 'plexdb' | 'arr';
  /** The integration backed up; 0 once that integration was deleted. */
  integrationId: number;
  /** The job that recorded the version; 0 once that job's history was deleted. */
  jobId: number;
  /** The version directory, relative to the destination target. */
  path: string;
  createdAt: string;
  size: number;
  method: string;
  integrity: 'ok' | 'failed';
  manifest: Record<string, unknown> | null;
}

// ---- Notifications ----

export interface Notification {
  id: number;
  name: string;
  kind: 'apprise';
  enabled: boolean;
  apiUrl: string;
  configKey: string;
  hasUrls: boolean;
  onFailure: boolean;
  onWarning: boolean;
  onSuccess: boolean;
  createdAt: string;
  updatedAt: string;
}

/**
 * NotificationInput is the body of POST/PUT /notifications. `urls` is write-only (Apprise URLs,
 * comma separated); leaving it out on PUT keeps the stored URLs.
 */
export interface NotificationInput {
  name: string;
  kind: 'apprise';
  enabled: boolean;
  apiUrl: string;
  configKey: string;
  urls?: string;
  onFailure: boolean;
  onWarning: boolean;
  onSuccess: boolean;
}

/** NotificationTestInput tests a form; with id and no urls the stored URLs are used. */
export interface NotificationTestInput extends NotificationInput {
  id?: number;
}

export interface TestResult {
  ok: boolean;
  message: string;
}
