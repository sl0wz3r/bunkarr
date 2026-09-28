// Stats of restic and rclone jobs (docs/design/phase4.md §11.4): labels and formats for the keys
// the engines add to the Phase 1–3 stats, and the per-snapshot rows of a restic sync.

import { formatNumber } from './format';
import { formatKiBps } from './bandwidth';

/** ENGINE_STAT_LABELS name the stats keys restic and rclone jobs add. */
export const ENGINE_STAT_LABELS: Record<string, string> = {
  engine: 'Engine',
  batches: 'Batches',
  bytesUploaded: 'Uploaded',
  bytesRead: 'Read from sources',
  unchanged: 'Nothing changed',
  deferrals: 'Waited for the window',
  limitKiBps: 'Upload limit',
  sampleFiles: 'Sample files',
  sampleBytes: 'Sample size',
  versionsChecked: 'Config versions checked',
  snapshotsForgotten: 'Snapshots forgotten',
  snapshotsKept: 'Snapshots kept',
  forgetRequests: 'Forget requests',
  pruned: 'Pruned',
  pruneDurationMs: 'Prune took',
  cleanup: 'Unfinished uploads cleaned',
};

/**
 * ENGINE_DRY_RUN_STAT_LABELS name the engine keys a dry run fills with what would happen (a
 * retention preview's would-forget and would-keep counts), so they never read as done.
 */
const ENGINE_DRY_RUN_STAT_LABELS: Record<string, string> = {
  snapshotsForgotten: 'Snapshots to forget',
  snapshotsKept: 'Snapshots to keep',
};

/**
 * engineStatHidden reports an engine stats key with no meaning in this job's stats: `unchanged` is
 * set only by a real restic sync (internal/enginerun restic_sync.go result), so a preview's or an
 * rclone sync's would always read "Nothing changed: No", even when nothing is to be copied.
 */
export function engineStatHidden(key: string, stats: Record<string, unknown> | null, dryRun = false): boolean {
  const engine = stats?.engine;
  return key === 'unchanged' && typeof engine === 'string' && (dryRun || engine !== 'restic');
}

/** engineStatLabel names an engine stats key (in a dry run: what would happen), or undefined for the Phase 1–3 keys. */
export function engineStatLabel(key: string, dryRun = false): string | undefined {
  return (dryRun ? ENGINE_DRY_RUN_STAT_LABELS[key] : undefined) ?? ENGINE_STAT_LABELS[key];
}

/** formatEngineStat formats the engine keys whose unit the generic formatter cannot tell; undefined for the others. */
export function formatEngineStat(key: string, v: unknown): string | undefined {
  if (key === 'limitKiBps' && typeof v === 'number') return formatKiBps(v);
  if (key === 'deferrals' && typeof v === 'number') return v === 1 ? 'once' : `${formatNumber(v)} times`;
  return undefined;
}

/** SnapshotStat is one snapshot of a restic sync (stats.snapshots). */
export interface SnapshotStat {
  sourceId: number;
  snapshotId: string;
  batch: number;
  filesNew: number;
  filesChanged: number;
  filesUnmodified: number;
  dataAdded: number;
}

/** snapshotStats reads a restic sync's per-snapshot rows (none for other jobs). */
export function snapshotStats(stats: Record<string, unknown> | null | undefined): SnapshotStat[] {
  const list = stats?.snapshots;
  if (!Array.isArray(list)) return [];
  return list.filter((r): r is SnapshotStat => typeof r === 'object' && r !== null && typeof (r as SnapshotStat).snapshotId === 'string');
}

/** CheckStat is a restic verify's repository check (stats.check). */
export interface CheckStat {
  numErrors: number;
  readSubset: string;
}

export function checkStat(stats: Record<string, unknown> | null | undefined): CheckStat | null {
  const c = stats?.check;
  if (typeof c !== 'object' || c === null || typeof (c as CheckStat).numErrors !== 'number') return null;
  return c as CheckStat;
}

/** describeCheck is "Structure checked, no errors" / "Read data subset 2/20: 3 errors". */
export function describeCheck(c: CheckStat): string {
  const what = c.readSubset === 'all' ? 'All data read' : c.readSubset ? `Data subset ${c.readSubset} read` : 'Structure checked';
  return `${what}: ${c.numErrors === 0 ? 'no errors' : `${formatNumber(c.numErrors)} ${c.numErrors === 1 ? 'error' : 'errors'}`}`;
}

/** shortId is the first 8 characters of a restic snapshot id. */
export function shortId(id: string): string {
  return id.slice(0, 8);
}

