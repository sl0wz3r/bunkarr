import type { ReactNode } from 'react';
import type { ItemAction, ItemStatus, JobStatus, MarkerStatus, ScanStatus } from '@/api/types';
import { ACTION_HELP, ACTION_LABELS, ITEM_STATUS_LABELS, JOB_STATUS_LABELS } from '@/lib/labels';

export type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'muted';

const TONES: Record<Tone, string> = {
  ok: 'border-accent/40 bg-accent/15 text-accent',
  warn: 'border-warn/50 bg-warn/15 text-warn',
  danger: 'border-danger/50 bg-danger/15 text-danger',
  info: 'border-info/50 bg-info/15 text-info',
  muted: 'border-line bg-panel-2 text-ink-muted',
};

/** Badge is a small colored label. */
export function Badge({ tone = 'muted', children, title }: { tone?: Tone; children: ReactNode; title?: string }) {
  return (
    <span title={title} className={`inline-flex items-center whitespace-nowrap rounded border px-1.5 py-0.5 text-xs font-medium leading-none ${TONES[tone]}`}>
      {children}
    </span>
  );
}

const JOB_TONES: Record<JobStatus, Tone> = {
  queued: 'muted',
  running: 'info',
  completed: 'ok',
  completed_with_warnings: 'warn',
  failed: 'danger',
  cancelled: 'muted',
};

export function JobStatusBadge({ status }: { status: JobStatus }) {
  return <Badge tone={JOB_TONES[status] ?? 'muted'}>{JOB_STATUS_LABELS[status] ?? status}</Badge>;
}

const ITEM_TONES: Record<ItemStatus, Tone> = {
  pending: 'muted',
  done: 'ok',
  failed: 'danger',
  skipped: 'muted',
  held: 'warn',
};

/** ItemStatusBadge shows an item's status; in a dry run "pending" reads "Planned". */
export function ItemStatusBadge({ status, dryRun }: { status: ItemStatus; dryRun?: boolean }) {
  const label = dryRun && status === 'pending' ? 'Planned' : (ITEM_STATUS_LABELS[status] ?? status);
  const title = status === 'held' ? 'Held by the mass-change guard: nothing was moved. "Apply held changes" runs it.' : undefined;
  return (
    <Badge tone={ITEM_TONES[status] ?? 'muted'} title={title}>
      {label}
    </Badge>
  );
}

const ACTION_TONES: Partial<Record<ItemAction, Tone>> = {
  copy: 'info',
  update: 'info',
  move: 'info',
  retain: 'warn',
  expire: 'warn',
  promote: 'warn',
};

export function ActionBadge({ action }: { action: ItemAction }) {
  return (
    <Badge tone={ACTION_TONES[action] ?? 'muted'} title={ACTION_HELP[action]}>
      {ACTION_LABELS[action] ?? action}
    </Badge>
  );
}

/** ScanStatusBadge shows a source's last scan status ('' or null: never scanned). */
export function ScanStatusBadge({ status }: { status: ScanStatus | '' | null }) {
  if (!status) {
    return <Badge>Never scanned</Badge>;
  }
  const tone: Tone = status === 'ok' ? 'ok' : status === 'warnings' ? 'warn' : 'danger';
  const label = status === 'ok' ? 'OK' : status === 'warnings' ? 'Warnings' : 'Failed';
  return <Badge tone={tone}>{label}</Badge>;
}

const MARKER_TEXT: Record<MarkerStatus, { tone: Tone; label: string; title: string }> = {
  ok: { tone: 'ok', label: 'Marker OK', title: 'The destination marker matches this destination.' },
  missing: { tone: 'warn', label: 'No marker', title: 'No .bunkarr/destination.json at the target.' },
  mismatch: { tone: 'danger', label: 'Marker mismatch', title: 'The marker belongs to a different destination.' },
  foreign: { tone: 'warn', label: 'Existing marker', title: 'The target already holds a Bunkarr destination marker.' },
};

export function MarkerBadge({ marker }: { marker: MarkerStatus }) {
  const m = MARKER_TEXT[marker] ?? { tone: 'muted' as Tone, label: marker, title: '' };
  return (
    <Badge tone={m.tone} title={m.title}>
      {m.label}
    </Badge>
  );
}
