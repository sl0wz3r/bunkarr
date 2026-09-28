// Test fixtures shaped like the Phase 1 API (docs/design/phase1.md §7).

import type { Destination, Integration, Job, Paged, Source } from '@/api/types';

export const MiB = 1024 * 1024;
export const GiB = 1024 * MiB;

const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString();

export function job(over: Partial<Job> = {}): Job {
  return {
    id: 7,
    type: 'sync',
    status: 'running',
    trigger: 'manual',
    dryRun: false,
    params: { destinationId: 1 },
    attempt: 1,
    progress: {
      phase: 'copying',
      filesTotal: 200,
      filesDone: 50,
      bytesTotal: 4 * GiB,
      bytesDone: 1 * GiB,
      currentFile: 'Movies/Heat (1995)/Heat.mkv',
      bytesPerSec: 50 * MiB,
      etaSeconds: 3725,
    },
    stats: null,
    warnings: 0,
    summary: '',
    queuedAt: minutesAgo(10),
    startedAt: minutesAgo(9),
    finishedAt: null,
    ...over,
  };
}

export function destination(over: Partial<Destination> = {}): Destination {
  return {
    id: 1,
    name: 'UNAS',
    engine: 'filecopy',
    target: '/backup',
    enabled: true,
    sourceIds: [1],
    fsType: 'nfs',
    capabilities: { hardlinks: true, unstableInodes: false, caseInsensitive: false, invalidChars: '', trailingDotSpace: true, mtimeGranularityNs: 1, fsType: 'nfs', checkedAt: minutesAgo(60), probeVersion: 1 },
    schedule: { cron: '0 2 * * *', enabled: true },
    verifySchedule: { cron: '0 3 * * 0', enabled: true },
    settings: { verify: { mode: 'sample', samplePercent: 5 }, hardlinks: 'recreate', adoptExisting: 'size+mtime', mtimeWindowSec: 0, maxChangePercent: 10, maxChangeFiles: 1000 },
    retention: { deletedDays: 30, plexDbDaily: 14, plexDbWeekly: 8 },
    kind: 'local',
    remote: {},
    hasCredentials: {},
    encryption: { mode: 'none', origin: '', kitExportedAt: null, kitConfirmedAt: null },
    bandwidth: { uploadKiBps: 0, downloadKiBps: 0, timetable: [], window: null },
    pending: false,
    waitingUntil: null,
    lastJob: null,
    lastSync: null,
    createdAt: minutesAgo(600),
    updatedAt: minutesAgo(600),
    ...over,
  };
}

export function source(over: Partial<Source> = {}): Source {
  return {
    id: 1,
    name: 'Movies',
    path: '/media/movies',
    destFolder: 'movies',
    exclude: [],
    enabled: true,
    plexIntegrationId: null,
    plexSectionId: '',
    plexPath: '',
    arrIntegrationId: null,
    fsType: 'ext4',
    lastScanAt: minutesAgo(30),
    lastScanStatus: 'ok',
    stats: { files: 1200, bytes: 3 * 1024 * GiB, uniqueBytes: 2 * 1024 * GiB, hardlinkGroups: 40, hardlinkedFiles: 80, skipped: 2 },
    createdAt: minutesAgo(600),
    updatedAt: minutesAgo(600),
    ...over,
  };
}

export function plexIntegration(over: Partial<Integration> = {}): Integration {
  return {
    id: 3,
    type: 'plex',
    name: 'Plex',
    url: 'http://plex:32400',
    enabled: true,
    hasApiKey: true,
    settings: { dataPath: '/plex', pathMappings: [{ plex: '/data', local: '/media' }], backup: { destinationId: 1, cron: '0 6 * * *', enabled: true } },
    createdAt: minutesAgo(600),
    updatedAt: minutesAgo(600),
    ...over,
  };
}

export function paged<T>(records: T[], totalRecords = records.length, page = 1, pageSize = 25): Paged<T> {
  return { page, pageSize, totalRecords, records };
}

// Phase 4 (docs/design/phase4.md): restic and rclone destinations off-site.

export const hostKeys = [
  { type: 'ssh-ed25519', fingerprint: 'SHA256:qK8bJ3n0ZC0Fq0sXg3c3r6s0y0cN1r0mJ9P1d2E3f4g', key: 'AAAAC3NzaC1lZDI1NTE5AAAAIEd25519keyforthetests' },
  { type: 'ecdsa-sha2-nistp256', fingerprint: 'SHA256:W1xEcdsaFingerprintForTheTests0000000000000', key: 'AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYecdsakey' },
];

/** resticDestination is an SFTP restic destination whose recovery kit is not confirmed yet. */
export function resticDestination(over: Partial<Destination> = {}): Destination {
  return destination({
    id: 9,
    name: 'Offsite',
    engine: 'restic',
    kind: 'sftp',
    target: 'sftp://bunkarr@backup.example.net:22/srv/bunkarr',
    fsType: 'sftp',
    capabilities: null,
    remote: { host: 'backup.example.net', port: 22, user: 'bunkarr', path: '/srv/bunkarr', hostKeys: hostKeys.map(({ type, key }) => ({ type, key })) },
    hasCredentials: { password: true },
    encryption: { mode: 'restic', origin: 'generated', kitExportedAt: null, kitConfirmedAt: null },
    blockedReason: 'export and confirm the recovery kit first',
    settings: {
      verify: { mode: 'sample', samplePercent: 5, sampleMaxBytes: 4 * GiB },
      hardlinks: 'recreate',
      adoptExisting: 'size+mtime',
      mtimeWindowSec: 0,
      maxChangePercent: 10,
      maxChangeFiles: 1000,
      transfers: 4,
      restic: { packSizeMiB: 64, batchBytes: 64 * GiB, batchFiles: 20000, pruneEveryDays: 7, pruneMaxUnused: '10%' },
    },
    retention: { deletedDays: 30, plexDbDaily: 14, plexDbWeekly: 8, snapshotDaily: 7, snapshotWeekly: 4, snapshotMonthly: 6, snapshotYearly: 0 },
    retentionSchedule: { cron: '30 4 * * *', enabled: true },
    engineVersion: 'restic 0.18.1',
    engineState: null,
    ...over,
  });
}

/** rcloneDestination is an S3 rclone destination with crypt, its kit confirmed. */
export function rcloneDestination(over: Partial<Destination> = {}): Destination {
  return destination({
    id: 10,
    name: 'Wasabi',
    engine: 'rclone',
    kind: 's3',
    target: 's3:https://s3.eu-central-2.wasabisys.com/media-backup/bunkarr',
    fsType: 's3',
    capabilities: null,
    remote: {
      provider: 'Wasabi',
      endpoint: 'https://s3.eu-central-2.wasabisys.com',
      region: 'eu-central-2',
      bucket: 'media-backup',
      prefix: 'bunkarr',
      storageClass: '',
      forcePathStyle: false,
      caCert: '',
    },
    hasCredentials: { accessKeyId: true, secretAccessKey: true },
    encryption: { mode: 'crypt', origin: 'generated', kitExportedAt: minutesAgo(90), kitConfirmedAt: minutesAgo(80) },
    settings: {
      verify: { mode: 'sample', samplePercent: 5, sampleMaxBytes: 16 * GiB },
      hardlinks: 'recreate',
      adoptExisting: 'size+mtime',
      mtimeWindowSec: 0,
      maxChangePercent: 10,
      maxChangeFiles: 1000,
      transfers: 4,
      rclone: { batchFiles: 1000, batchBytes: 64 * GiB },
    },
    retentionSchedule: { cron: '30 4 * * *', enabled: true },
    engineVersion: 'rclone v1.74.1',
    ...over,
  });
}
