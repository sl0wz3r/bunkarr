// Pieces shared by the queue, history and job detail pages.

import { Link } from 'react-router';
import type { Job, JobProgress } from '@/api/types';
import { ProgressBar } from '@/components/ProgressBar';
import { Badge } from '@/components/StatusBadge';
import { Stat, StatGrid } from '@/components/Page';
import { formatBytes, formatDuration, formatDurationMs, formatNumber, formatPercent } from '@/lib/format';
import { DRY_RUN_HIDDEN_STATS, sourceColumnLabel, statLabel, TRIGGER_LABELS } from '@/lib/labels';
import { jobTitle, type Names } from '@/lib/lookups';

/** progressFraction is bytes done/total, else files done/total, else null (not known yet). */
export function progressFraction(p: JobProgress | null | undefined): number | null {
  if (!p) {
    return null;
  }
  if (p.bytesTotal > 0) {
    return p.bytesDone / p.bytesTotal;
  }
  if (p.filesTotal > 0) {
    return p.filesDone / p.filesTotal;
  }
  return null;
}

/** progressText is "1,234 of 5,678 files · 1.2 GiB of 3.4 GiB" (parts that are known). */
export function progressText(p: JobProgress | null | undefined): string {
  if (!p) {
    return '';
  }
  const parts: string[] = [];
  if (p.filesTotal > 0 || p.filesDone > 0) {
    parts.push(p.filesTotal > 0 ? `${formatNumber(p.filesDone)} of ${formatNumber(p.filesTotal)} files` : `${formatNumber(p.filesDone)} files`);
  }
  if (p.bytesTotal > 0 || p.bytesDone > 0) {
    parts.push(p.bytesTotal > 0 ? `${formatBytes(p.bytesDone)} of ${formatBytes(p.bytesTotal)}` : formatBytes(p.bytesDone));
  }
  return parts.join(' · ');
}

/** JobProgressView is the progress bar with its figures, phase and current file. */
export function JobProgressView({ job, label }: { job: Job; label: string }) {
  const p = job.progress;
  const fraction = job.status === 'running' ? progressFraction(p) : job.status === 'queued' ? 0 : null;
  const text = progressText(p);
  return (
    <div className="min-w-[12rem]">
      <div className="flex items-center gap-2">
        <ProgressBar value={fraction} label={`Progress of ${label}`} />
        {fraction != null && <span className="w-12 shrink-0 text-right text-xs text-ink-muted">{formatPercent(fraction)}</span>}
      </div>
      <div className="mt-1 text-xs text-ink-muted">
        {job.status === 'queued' ? 'Waiting for a free worker or for another job on the same target' : text || (p?.phase ? `${p.phase}…` : 'Starting…')}
      </div>
      {p?.currentFile && (
        <div className="mt-0.5 truncate font-mono text-xs text-ink-muted" title={p.currentFile}>
          {p.currentFile}
        </div>
      )}
    </div>
  );
}

/** cancelText explains what cancelling a job does: a queued job is only taken off the queue. */
export function cancelText(job: Pick<Job, 'status'>): string {
  return job.status === 'queued'
    ? 'The job has not started yet: it is taken off the queue and nothing is changed.'
    : 'The job stops promptly and removes its temporary files. Files already copied stay and are recorded, so the next run continues from there.';
}

/** JobBadges marks previews, held-change runs and retries. */
export function JobBadges({ job }: { job: Job }) {
  return (
    <>
      {job.dryRun && (
        <Badge tone="info" title="Dry run: nothing is changed at the destination">
          Preview
        </Badge>
      )}
      {job.params?.allowChanges && (
        <Badge tone="warn" title="Runs changes the mass-change guard held">
          Applies held changes
        </Badge>
      )}
      {job.attempt > 1 && <Badge title="Resumed after a restart">Attempt {job.attempt}</Badge>}
    </>
  );
}

/** JobCell links to the job and shows its id, trigger and badges. */
export function JobCell({ job, names }: { job: Job; names: Names }) {
  return (
    <div className="min-w-[10rem]">
      <Link to={`/activity/jobs/${job.id}`} className="font-medium text-ink hover:text-accent hover:underline">
        {jobTitle(job, names)}
      </Link>
      <div className="mt-1 flex flex-wrap items-center gap-1 text-xs text-ink-muted">
        <span>
          #{job.id} · {TRIGGER_LABELS[job.trigger] ?? job.trigger}
        </span>
        <JobBadges job={job} />
      </div>
    </div>
  );
}

/** formatStat formats a stats value by its key: bytes*, *Bytes → size, *Ms → duration. */
export function formatStat(key: string, v: unknown): string {
  if (typeof v === 'number') {
    if (/^bytes|Bytes$/.test(key)) return formatBytes(v);
    if (/Ms$/.test(key)) return formatDurationMs(v);
    if (/Seconds$|Sec$/.test(key)) return formatDuration(v);
    return formatNumber(v);
  }
  if (typeof v === 'boolean') return v ? 'Yes' : 'No';
  if (typeof v === 'string') return v;
  return JSON.stringify(v);
}

type Row = Record<string, unknown>;

function isRow(v: unknown): v is Row {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** isCountMap reports whether v is an object of counts, e.g. a scan's skipped {symlink: 3}. */
function isCountMap(v: unknown): v is Record<string, number> {
  return isRow(v) && Object.values(v).every((x) => typeof x === 'number');
}

function sumCounts(m: Record<string, number>): number {
  return Object.values(m).reduce((n, x) => n + x, 0);
}

/**
 * splitStats separates a stats object into the figures shown as a grid (numbers, strings,
 * booleans), per-source rows (the sync's and scan's `sources` list) and nothing else: dryRun is
 * shown by the page itself, and nested values the UI has no view for are left out.
 */
export function splitStats(stats: Record<string, unknown> | null, dryRun = false): { figures: [string, unknown][]; sources: Row[] } {
  const figures: [string, unknown][] = [];
  let sources: Row[] = [];
  for (const [k, v] of Object.entries(stats ?? {})) {
    if (k === 'sources' && Array.isArray(v)) {
      sources = v.filter(isRow);
    } else if (k === 'dryRun' || v === null || v === undefined || (dryRun && DRY_RUN_HIDDEN_STATS.has(k))) {
      continue;
    } else if (typeof v === 'number' || typeof v === 'string' || typeof v === 'boolean') {
      figures.push([k, v]);
    } else if (isCountMap(v)) {
      figures.push([k, sumCounts(v)]);
    }
  }
  return { figures, sources };
}

/** SOURCE_SKIP are per-source fields that are not table columns (ids, names, timestamps). */
const SOURCE_SKIP = new Set(['sourceId', 'name', 'sourceName', 'startedAt', 'fsType', 'fuse']);

function sourceName(r: Row): string {
  const n = r.name ?? r.sourceName;
  return typeof n === 'string' && n ? n : `Source #${String(r.sourceId ?? '?')}`;
}

/** SourceBreakdown is the per-source table of a sync's or scan's stats, with their warnings. */
export function SourceBreakdown({ rows }: { rows: Row[] }) {
  if (rows.length === 0) {
    return null;
  }
  const columns: string[] = [];
  for (const r of rows) {
    for (const [k, v] of Object.entries(r)) {
      if (!SOURCE_SKIP.has(k) && !columns.includes(k) && (typeof v === 'number' || isCountMap(v))) {
        columns.push(k);
      }
    }
  }
  const notes: string[] = [];
  for (const r of rows) {
    for (const key of ['unreadableDirs', 'warnings']) {
      const list = r[key];
      if (Array.isArray(list)) {
        for (const w of list) {
          if (typeof w === 'string' && w) notes.push(`${sourceName(r)}: ${key === 'unreadableDirs' ? `cannot read ${w}` : w}`);
        }
      }
    }
  }
  const cell = (k: string, v: unknown) => {
    if (isCountMap(v)) {
      const parts = Object.entries(v).map(([reason, n]) => `${reason} ${formatNumber(n)}`);
      return <span title={parts.join(', ') || undefined}>{formatNumber(sumCounts(v))}</span>;
    }
    return typeof v === 'number' ? formatStat(k, v) : '—';
  };
  return (
    <div className="mb-5">
      <div className="relative overflow-x-auto rounded border border-line bg-panel">
        <table className="w-full text-sm">
          <caption className="sr-only">By source</caption>
          <thead>
            <tr className="border-b border-line text-left text-ink-muted">
              <th scope="col" className="px-3 py-2 font-medium">
                Source
              </th>
              {columns.map((k) => (
                <th key={k} scope="col" className="whitespace-nowrap px-3 py-2 text-right font-medium">
                  {sourceColumnLabel(k)}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={String(r.sourceId ?? i)} className="border-b border-line/60 last:border-b-0">
                <th scope="row" className="px-3 py-2 text-left font-normal">
                  {sourceName(r)}
                </th>
                {columns.map((k) => (
                  <td key={k} className={`whitespace-nowrap px-3 py-2 text-right ${r[k] === 0 ? 'text-ink-muted' : ''}`}>
                    {cell(k, r[k])}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {notes.length > 0 && (
        <ul className="mt-2 list-disc space-y-1 pl-5 text-xs text-warn">
          {notes.map((n, i) => (
            <li key={i} className="break-words">
              {n}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** JobStats renders a job's stats object as a grid of figures and, for syncs and scans, a per-source table. */
export function JobStats({ stats, dryRun = false }: { stats: Record<string, unknown> | null; dryRun?: boolean }) {
  const { figures, sources } = splitStats(stats, dryRun);
  if (figures.length === 0 && sources.length === 0) {
    return null;
  }
  return (
    <>
      {figures.length > 0 && (
        <StatGrid label="Statistics">
          {figures.map(([k, v]) => (
            <Stat
              key={k}
              label={statLabel(k, dryRun)}
              value={typeof v === 'string' && v.includes('/') ? <span className="break-all font-mono text-xs">{v}</span> : formatStat(k, v)}
              muted={v === 0}
            />
          ))}
        </StatGrid>
      )}
      <SourceBreakdown rows={sources} />
    </>
  );
}
