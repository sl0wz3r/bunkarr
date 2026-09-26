// *arr backups (design phase2-3 §10, §13): an *arr integration's backup settings, "Back up now"
// and its versions. The versions (zips holding the *arr's API key and passwords) are never served:
// only their snapshot rows, which name files, sizes and hashes.

import { api } from './client';
import type { Integration, Job, Snapshot } from './types';
import type { CronPreset } from '@/lib/cron';

/**
 * ArrBackupSettings is settings.backup of a Sonarr, Radarr or Lidarr integration (a type alias, so
 * it fits ArrSettings' open backup record).
 */
export type ArrBackupSettings = {
  /** 0: no backup destination chosen. */
  destinationId: number;
  cron: string;
  enabled: boolean;
  /** 1–90: a scheduled backup of the *arr younger than this is copied instead of making a new one. */
  maxScheduledAgeDays: number;
  /** Allows a destination that does not keep files private (SMB without POSIX extensions). */
  acceptInsecureModes: boolean;
};

/** DEFAULT_ARR_BACKUP_CRON is the server's default schedule of an enabled *arr backup (weekly). */
export const DEFAULT_ARR_BACKUP_CRON = '30 6 * * 0';

export const DEFAULT_ARR_BACKUP: ArrBackupSettings = {
  destinationId: 0,
  cron: '',
  enabled: false,
  maxScheduledAgeDays: 7,
  acceptInsecureModes: false,
};

/** ARR_BACKUP_PRESETS are the offered backup schedules (the default first). */
export const ARR_BACKUP_PRESETS: CronPreset[] = [
  { label: 'Weekly, Sunday 06:30', cron: DEFAULT_ARR_BACKUP_CRON },
  { label: 'Daily at 06:30', cron: '30 6 * * *' },
  { label: 'Monthly, 1st at 06:30', cron: '30 6 1 * *' },
];

/** MANUAL_BACKUP_WARNING is the number of manual backups in the *arr above which the UI warns. */
export const MANUAL_BACKUP_WARNING = 10;

/** arrBackupOf reads an integration's backup settings and backup folder (defaults when missing). */
export function arrBackupOf(it: Integration | null | undefined): { backupFolder: string; backup: ArrBackupSettings } {
  const raw = (it?.settings ?? {}) as { backupFolder?: string; backup?: Partial<ArrBackupSettings> };
  return { backupFolder: raw.backupFolder ?? '', backup: { ...DEFAULT_ARR_BACKUP, ...(raw.backup ?? {}) } };
}

/** startArrBackup queues an arr_backup job (the destination defaults to the integration's). */
export function startArrBackup(id: number, body: { destinationId?: number; dryRun?: boolean } = {}): Promise<Job> {
  return api<Job>(`/integrations/${id}/arr/backup`, { method: 'POST', body });
}

/** listArrSnapshots lists the integration's backup versions at every destination, newest first. */
export async function listArrSnapshots(id: number): Promise<Snapshot[]> {
  return (await api<Snapshot[] | null>(`/integrations/${id}/arr/snapshots`)) ?? [];
}
