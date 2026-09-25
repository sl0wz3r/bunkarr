// Human labels and explanations for job types, statuses, triggers and item actions. The action
// texts follow docs/design/phase1.md §4.2 so the dry-run preview explains what a sync would do.

import type { ItemAction, ItemStatus, JobStatus, JobTrigger, JobType } from '@/api/types';

export const JOB_TYPE_LABELS: Record<JobType, string> = {
  scan: 'Scan',
  sync: 'Sync',
  plexdb_backup: 'Plex DB backup',
  retention: 'Retention',
  verify: 'Verify',
};

export const JOB_STATUS_LABELS: Record<JobStatus, string> = {
  queued: 'Queued',
  running: 'Running',
  completed: 'Completed',
  completed_with_warnings: 'Warnings',
  failed: 'Failed',
  cancelled: 'Cancelled',
};

export const TRIGGER_LABELS: Record<JobTrigger, string> = {
  schedule: 'Scheduled',
  manual: 'Manual',
  webhook: 'Webhook',
  resume: 'Resumed',
  startup: 'Startup',
};

export const ACTION_LABELS: Record<ItemAction, string> = {
  copy: 'Copy',
  update: 'Update',
  move: 'Move',
  adopt: 'Adopt',
  link: 'Link',
  promote: 'Promote',
  retain: 'Retain',
  expire: 'Expire',
  verify: 'Verify',
  backup: 'Backup',
  skip: 'Skip',
};

/** ACTION_HELP explains each action in one sentence (the dry-run legend). */
export const ACTION_HELP: Record<ItemAction, string> = {
  copy: 'New file: copied to the destination (an unmanaged file already at that path is adopted if it matches, otherwise moved into retention first).',
  update: 'Changed file: the new version is copied and verified first, then the old version moves into retention.',
  move: 'Renamed or moved at the source with the same content: renamed at the destination instead of copied again.',
  adopt: 'Already at the destination with matching size and modification time (for example from rsync): recorded, not copied.',
  link: 'Another name of a hardlinked file: hardlinked at the destination when it supports hardlinks, otherwise only recorded (the content is stored once).',
  promote: 'A hardlinked file whose first name is going away: its content is kept under a surviving name before the old name is retained.',
  retain: 'Gone from the source: moved into the destination\'s retention folder and deleted only after the retention period. Deletes are never mirrored directly.',
  expire: 'Retention period over: the retained copy is deleted.',
  verify: 'Re-read at the destination and compared with the recorded size and hash.',
  backup: 'A Plex database or preferences file copied into a new backup version.',
  skip: 'Nothing to do; listed for the preview (for example a name the destination cannot store).',
};

export const ITEM_STATUS_LABELS: Record<ItemStatus, string> = {
  pending: 'Pending',
  done: 'Done',
  failed: 'Failed',
  skipped: 'Skipped',
  held: 'Held',
};

/** ITEM_STATUS_ORDER is the column order of the item summary. */
export const ITEM_STATUS_ORDER: ItemStatus[] = ['pending', 'done', 'held', 'failed', 'skipped'];

/** ACTION_ORDER is the execution order of a sync (design §4.1 step 5), then the rest. */
export const ACTION_ORDER: ItemAction[] = ['promote', 'move', 'copy', 'update', 'adopt', 'link', 'retain', 'expire', 'verify', 'backup', 'skip'];

const STAT_LABELS: Record<string, string> = {
  // sync
  filesPlanned: 'Files planned',
  filesCopied: 'Copied',
  filesUpdated: 'Updated',
  filesMoved: 'Moved',
  filesAdopted: 'Adopted',
  filesLinked: 'Linked',
  filesPromoted: 'Promoted',
  filesRetained: 'Retained',
  filesDisplaced: 'Displaced',
  filesHeld: 'Held',
  filesFailed: 'Failed',
  filesSkipped: 'Skipped',
  bytesPlanned: 'Planned (unique)',
  bytesCopied: 'Copied (unique)',
  durationMs: 'Duration',
  // scan, Plex DB backup
  files: 'Files',
  bytes: 'Size',
  sourcesScanned: 'Sources scanned',
  sourcesFailed: 'Sources failed',
  groups: 'Hardlink groups',
  groupedFiles: 'Hardlinked files',
  // verify
  filesChecked: 'Checked (size)',
  filesVerified: 'Verified (hash)',
  filesMissing: 'Missing',
  hashesRecorded: 'Hashes recorded',
  bytesVerified: 'Re-read',
  // retention
  jobsQueued: 'Jobs queued',
  jobsPruned: 'Old jobs pruned',
  filesExpired: 'Expired',
  filesKept: 'Kept',
  bytesExpired: 'Freed',
  // Plex DB backup
  snapshotId: 'Snapshot',
  metadataItems: 'Metadata items',
  mediaParts: 'Media parts',
  recovered: 'Recovered versions',
  pruned: 'Old versions pruned',
};

/**
 * DRY_RUN_STAT_LABELS name a dry run's counts, which say what a sync would do (design S9):
 * "To copy" instead of "Copied".
 */
const DRY_RUN_STAT_LABELS: Record<string, string> = {
  filesCopied: 'To copy',
  filesUpdated: 'To update',
  filesMoved: 'To move',
  filesAdopted: 'To adopt',
  filesLinked: 'To link',
  filesPromoted: 'To promote',
  filesRetained: 'To retain',
  filesDisplaced: 'To displace',
  filesHeld: 'Would be held',
  filesFailed: 'Would fail',
  filesExpired: 'To expire',
  filesVerified: 'To verify',
  files: 'Files to back up',
  bytes: 'Size to back up',
};

/** DRY_RUN_HIDDEN_STATS are counters a dry run never moves (always 0). */
export const DRY_RUN_HIDDEN_STATS = new Set(['bytesCopied', 'bytesExpired', 'bytesVerified', 'hashesRecorded', 'pruned']);

/** statLabel names a job stats key (in a dry run: what would happen); unknown keys are split from camelCase. */
export function statLabel(key: string, dryRun = false): string {
  if (dryRun && DRY_RUN_STAT_LABELS[key]) {
    return DRY_RUN_STAT_LABELS[key];
  }
  if (STAT_LABELS[key]) {
    return STAT_LABELS[key];
  }
  const words = key.replace(/([a-z0-9])([A-Z])/g, '$1 $2').toLowerCase();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/** SOURCE_COLUMN_LABELS head the per-source table of sync and scan stats. */
const SOURCE_COLUMN_LABELS: Record<string, string> = {
  files: 'Files',
  bytes: 'Size',
  added: 'Added',
  changed: 'Changed',
  deleted: 'Deleted',
  skipped: 'Skipped',
  excluded: 'Excluded',
  groups: 'Hardlink groups',
  groupedFiles: 'Hardlinked files',
  changes: 'Retain + update',
  held: 'Held',
  warningCount: 'Warnings',
  durationMs: 'Duration',
};

/** sourceColumnLabel heads a column of the per-source stats table. */
export function sourceColumnLabel(key: string): string {
  return SOURCE_COLUMN_LABELS[key] ?? statLabel(key);
}
