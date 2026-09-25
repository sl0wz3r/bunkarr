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
