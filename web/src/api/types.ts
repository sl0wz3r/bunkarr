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
  /** restic and rclone as found at start-up (phase4.md §12); missing on an older server. */
  engines?: EngineAvailability;
}

/** EngineBinaryStatus is one engine binary in GET /system/status (engines.BinaryStatus). */
export interface EngineBinaryStatus {
  available: boolean;
  version?: string;
  path?: string;
  /** Why the engine is not available ("restic is not installed"). */
  reason?: string;
}

export interface EngineAvailability {
  restic: EngineBinaryStatus;
  rclone: EngineBinaryStatus;
}

/** EngineSettings is GET and PUT /settings/engines (phase4.md §9.3, §12). */
export interface EngineSettings {
  /** 1–8: engine syncs that upload at once; read at start-up. */
  uploadSlots: number;
  /** 1–1440 minutes an engine command may keep retrying before it is stopped. */
  retryBudgetMinutes: number;
  /** The slot count the job manager uses now (read at start-up). */
  uploadSlotsInEffect: number;
  /** uploadSlots differs from the count in effect: a restart applies it. */
  restartRequired: boolean;
  /** Says when a change applies. */
  note: string;
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
  /** A retention job of one restic destination prunes its repository now (phase4.md §6.5). */
  prune?: boolean;
  /** A verify reads everything once (restic check --read-data; every file on rclone). */
  readData?: boolean;
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
  /** Engine jobs (phase4.md §10.4): the batch running and how many the plan has. */
  batch?: number;
  batches?: number;
  /** The upload limit in force (0 or missing: none). */
  limitBytesPerSec?: number;
  /** When the destination's transfer window closes (missing: no window). */
  windowEndsAt?: string | null;
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
  /** A queued job its transfer window deferred: it does not start before this (phase4.md §9.2). */
  notBefore?: string | null;
  /** How often a transfer window deferred the job. */
  deferrals?: number;
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
  /**
   * Why the schedule's last fire queued no job (a deferred job of its destination covers it,
   * phase4.md §9.2); null or missing when it did.
   */
  lastSkip?: ScheduleSkip | null;
}

export interface ScheduleSkip {
  /** The fire's scheduled time. */
  at: string;
  reason: string;
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
  /**
   * Up to four destinations, each with its schedule (phase4.md §8.5); the fields above mirror
   * targets[0]. Missing on an older server: the single form is the one target. A save always
   * sends it ([] for none): without it the server keeps the stored targets after the first.
   */
  targets?: BackupTarget[];
}

/** BackupTarget is one destination of a Plex DB or *arr backup (phase4.md §8.5). */
export interface BackupTarget {
  destinationId: number;
  cron: string;
  enabled: boolean;
  /** Only for a local destination whose probe reports enforcesModes false (S17). */
  acceptInsecureModes: boolean;
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
  /** The user's password, required by S29 (a backup target off the machine, acceptInsecureModes). */
  currentPassword?: string;
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
  /** sampleMaxBytes: restic and rclone only, the bytes one sample verify reads back at most. */
  verify: { mode: VerifyMode; samplePercent: number; sampleMaxBytes?: number };
  hardlinks: HardlinkMode;
  adoptExisting: AdoptMode;
  mtimeWindowSec: number;
  maxChangePercent: number;
  maxChangeFiles: number;
  /** restic and rclone only (1–32, default 4): parallel transfers. Refused on filecopy. */
  transfers?: number;
  /** restic destinations only (phase4.md §12). */
  restic?: ResticSettings;
  /** rclone destinations only. */
  rclone?: RcloneSettings;
}

export interface ResticSettings {
  /** 4–128 (default 64 remote, 16 local). */
  packSizeMiB: number;
  batchBytes: number;
  batchFiles: number;
  /** 1–90 (default 7). */
  pruneEveryDays: number;
  /** "10%", "5G" or "unlimited". */
  pruneMaxUnused: string;
}

export interface RcloneSettings {
  batchFiles: number;
  batchBytes: number;
}

// ---- Phase 4: destination kinds, engines, encryption, bandwidth (phase4.md §4, §5, §9) ----

export type DestKind = 'local' | 'sftp' | 's3' | 'b2';
export type EngineName = 'filecopy' | 'restic' | 'rclone';
export type EncryptionMode = 'none' | 'restic' | 'crypt';
export type S3Provider = 'AWS' | 'Minio' | 'Wasabi' | 'Cloudflare' | 'Other';

/** HostKey is one pinned SFTP host key: its type and the key in base64 SSH wire format. */
export interface HostKey {
  type: string;
  key: string;
}

/** HostKeyInfo is one key an SFTP server presents (POST /destinations/sftp/hostkeys). */
export interface HostKeyInfo extends HostKey {
  /** "SHA256:…" */
  fingerprint: string;
}

export interface SftpRemote {
  host: string;
  port: number;
  user: string;
  path: string;
  hostKeys: HostKey[];
}

export interface S3Remote {
  provider: S3Provider;
  /** '' for AWS: the region's endpoint. */
  endpoint: string;
  region: string;
  bucket: string;
  prefix: string;
  storageClass: string;
  forcePathStyle: boolean;
  /** One PEM certificate for a self-signed endpoint ('' = the system roots). */
  caCert: string;
}

export interface B2Remote {
  bucket: string;
  prefix: string;
}

/** DestinationRemote is a destination's `remote` as the API returns it: its kind's object, {} for local. */
export type DestinationRemote = Partial<SftpRemote> & Partial<S3Remote> & Partial<B2Remote>;

export type CredentialField = 'privateKey' | 'privateKeyPassphrase' | 'password' | 'accessKeyId' | 'secretAccessKey' | 'keyId' | 'applicationKey';

/** CredentialsInput is write-only: a field that is sent replaces the stored one; never send '' or {}. */
export type CredentialsInput = Partial<Record<CredentialField, string>>;

/** EncryptionInfo is a destination's encryption and recovery kit custody (S21, §5.2). */
export interface EncryptionInfo {
  mode: EncryptionMode;
  /** Who chose the secret: generated, user, or '' (no secret). */
  origin: 'generated' | 'user' | '';
  kitExportedAt: string | null;
  kitConfirmedAt: string | null;
}

/** EncryptionInput chooses the encryption at create; it cannot change afterwards. */
export interface EncryptionInput {
  mode?: EncryptionMode;
  /** Default true; false with the user's secret. */
  generate?: boolean;
  /** Required with mode none (rclone): the provider can read every file. S29. */
  acceptUnencrypted?: boolean;
  secret?: string;
  /**
   * rclone crypt's password2 (the salt), with the user's secret only: attaching a crypt remote
   * whose kit lists a password2 needs both of its crypt passwords. Omitted: rclone's default salt.
   */
  secret2?: string;
}

export type Weekday = 'mon' | 'tue' | 'wed' | 'thu' | 'fri' | 'sat' | 'sun';

/** BandwidthEntry is one timetable line: on each of days from `from` to `to`, these limits (KiB/s, 0 = unlimited). */
export interface BandwidthEntry {
  days: Weekday[];
  from: string;
  to: string;
  uploadKiBps: number;
  downloadKiBps: number;
}

/** TransferWindow: sync, verify and retention run only inside it (phase4.md §9.2). */
export interface TransferWindow {
  days: Weekday[];
  from: string;
  to: string;
  /** 0–120 (default 15). */
  graceMinutes: number;
  /** Let a file larger than the window start alone at its opening and run past its end. */
  allowOverrun: boolean;
}

/** Bandwidth is a destination's limits, timetable and window (phase4.md §9.1). */
export interface Bandwidth {
  uploadKiBps: number;
  downloadKiBps: number;
  timetable: BandwidthEntry[] | null;
  /** null: always open. */
  window: TransferWindow | null;
}

/** EngineState is a restic or rclone destination's engine state (enginerun.EngineState). */
export interface EngineState {
  destinationId: number;
  engineVersion: string;
  lastPruneAt: string | null;
  lastCheckAt: string | null;
  readSubsetNext: number;
  lastCleanupAt: string | null;
  /** The upload rate recent syncs measured (bytes/s); null when unknown. */
  throughputBps: number | null;
  /** Repository or remote figures (snapshotCount, repositoryBytes, …). */
  stats: Record<string, unknown> | null;
  updatedAt: string;
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
  /**
   * restic only (phase4.md §6.5): days (0–3650), ISO weeks (0–520), months (0–120) and years
   * (0–100) that keep their newest complete snapshot; 0 keeps none of that period.
   */
  snapshotDaily?: number;
  snapshotWeekly?: number;
  snapshotMonthly?: number;
  snapshotYearly?: number;
}

export interface Destination {
  id: number;
  name: string;
  engine: EngineName;
  /** The location type (phase4.md §4.1); local for every Phase 1–3 destination. */
  kind: DestKind;
  /** A local destination's path, or a remote's display location (sftp://user@host:22/path, s3:…, b2:…). */
  target: string;
  enabled: boolean;
  sourceIds: number[];
  fsType: string;
  /** The probe's findings (design §3); zero values until the first probe. */
  capabilities: Partial<Capabilities> | null;
  /** {cron: '', enabled: false} when the destination has no schedule of that kind. */
  schedule: CronSchedule;
  verifySchedule: CronSchedule;
  /** restic and rclone only: the destination's own retention schedule. */
  retentionSchedule?: CronSchedule;
  settings: DestinationSettings;
  retention: Retention;
  /** A remote kind's non-secret location ({} for local). */
  remote: DestinationRemote;
  /** The storage credential fields that are stored (never a value). */
  hasCredentials: Partial<Record<CredentialField, boolean>> | null;
  encryption: EncryptionInfo;
  bandwidth: Bandwidth;
  /** A create that initialized a repository did not finish: no job runs; create it again or delete it. */
  pending: boolean;
  /** Why no job but a dry run may run (the recovery kit, a create that did not finish, the engine); '' or missing when they can. */
  blockedReason?: string;
  /** Warnings of the create or update that returned this value (not stored). */
  warnings?: string[];
  /** The installed engine's version (restic and rclone). */
  engineVersion?: string;
  engineState?: EngineState | null;
  /** When a job of the destination that waits for its transfer window starts again; null when none waits. */
  waitingUntil?: string | null;
  /** The newest job of any type that worked on the destination. */
  lastJob: Job | null;
  /** The newest sync (queued, running or finished; previews excluded): the last sync status. */
  lastSync: Job | null;
  createdAt: string;
  updatedAt: string;
}

export interface DestinationInput {
  name: string;
  engine: EngineName;
  /** Default local. */
  kind?: DestKind;
  /** A local destination's path (ignored for remote kinds). */
  target: string;
  /** A remote kind's location; on update only hostKeys and caCert may differ (S29). */
  remote?: SftpRemote | S3Remote | B2Remote;
  /** Write-only; on update only the fields to replace (S29). */
  credentials?: CredentialsInput;
  /** Create only. */
  encryption?: EncryptionInput;
  bandwidth?: Bandwidth;
  enabled: boolean;
  sourceIds: number[];
  schedule: CronSchedule;
  verifySchedule: CronSchedule;
  /** restic and rclone only. */
  retentionSchedule?: CronSchedule;
  settings: DestinationSettings;
  retention: Retention;
  /** Create only: adopt the id of an existing .bunkarr/destination.json marker, or an existing repository. */
  attach?: boolean;
  /** Create only: accept a target on the root/config filesystem or on tmpfs/overlay. */
  allowLocal?: boolean;
  /** The user's password for the changes of S29 (off-site kinds, credentials, host keys, sources, no encryption). */
  currentPassword?: string;
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

/** RepositoryState is a restic test's repository finding. */
export type RepositoryState = 'missing' | 'exists' | 'wrong-password' | 'locked';

/** RemoteMarkerState is an rclone test's marker finding. */
export type RemoteMarkerState = 'missing' | 'ok' | 'unreadable' | 'foreign';

/** EngineTestResult answers a test of a restic or rclone destination (phase4.md §4.5). */
export interface EngineTestResult {
  ok: boolean;
  reachable: boolean;
  /** restic: missing, exists, wrong-password, locked. */
  repository?: RepositoryState;
  /** rclone: missing, ok, unreadable, foreign. */
  marker?: RemoteMarkerState;
  /** The repository id (restic) or the marker's id (rclone). */
  id?: string;
  markerName?: string;
  entries: number;
  /** An SFTP server's presented keys when none are pinned yet: confirm and pin them. */
  hostKeys?: HostKeyInfo[];
  freeBytes: number | null;
  engineVersion?: string;
  message?: string;
  warnings?: string[] | null;
}

/** EngineTestInput is POST /destinations/test for a restic or rclone destination (never an id). */
export interface EngineTestInput {
  kind: DestKind;
  engine: 'restic' | 'rclone';
  target?: string;
  remote?: SftpRemote | S3Remote | B2Remote;
  credentials?: CredentialsInput;
  encryption?: EncryptionInput;
}

export interface Snapshot {
  id: number;
  destinationId: number;
  /** plexdb: a Plex DB version; arr: an *arr backup zip (never downloadable); media: a restic snapshot of one source and batch. */
  kind?: 'plexdb' | 'arr' | 'media';
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
  /** The engine's reference (a restic snapshot id). */
  engineRef?: string;
  /** media snapshots only. */
  sourceId?: number;
  batch?: number;
  complete?: boolean;
  files?: number;
  dataAdded?: number;
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
