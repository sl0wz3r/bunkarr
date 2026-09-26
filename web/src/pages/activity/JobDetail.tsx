import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query';
import { ArrowLeft, Play, ShieldAlert, Square } from 'lucide-react';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Link, useNavigate, useParams } from 'react-router';
import { cancelJob, getJob, itemSummary, itemSummaryByTier, listItems } from '@/api/jobs';
import { isReleasePreview, releaseParamsOf, startSync as requestSync, TIERS, type ReleaseParams, type Tier } from '@/api/tiers';
import type { ItemAction, ItemCount, ItemStatus, Job, JobItem, TierItemCount } from '@/api/types';
import { Button } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { DataTable, type Column } from '@/components/DataTable';
import { inputClass } from '@/components/Form';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Page } from '@/components/Page';
import { Pagination } from '@/components/Pagination';
import { ActionBadge, ItemStatusBadge, JobStatusBadge } from '@/components/StatusBadge';
import { DeletedIntegrationsHint } from '@/components/tiers/DeletedIntegrations';
import { ReleaseJobNotice } from '@/components/tiers/Release';
import { TierJobStats } from '@/components/tiers/TierJobStats';
import { TIER_LABELS } from '@/components/tiers/tierText';
import { formatBytes, formatDateTime, formatDuration, formatEta, formatNumber, formatRate, secondsBetween } from '@/lib/format';
import { ACTION_HELP, ACTION_LABELS, ACTION_ORDER, ITEM_STATUS_LABELS, ITEM_STATUS_ORDER, JOB_TYPE_LABELS, TRIGGER_LABELS } from '@/lib/labels';
import { isActive, jobTarget, keys, useDestinations, useNames } from '@/lib/lookups';
import { POLL_MS } from '@/lib/queryClient';
import { cancelText, JobBadges, JobProgressView, JobStats } from './JobParts';
import { JobLogs } from './JobLogs';

export const ITEMS_PAGE_SIZE = 50;

/** Activity → job detail (/activity/jobs/:id): summary, stats, planned items (the dry-run preview), held changes, logs. */
export function JobDetail() {
  const jobId = Number(useParams().id);
  const validId = Number.isInteger(jobId) && jobId > 0;
  const names = useNames();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [confirm, setConfirm] = useState<'cancel' | 'apply' | 'run' | null>(null);
  const [filter, setFilter] = useState<ItemFilter>({ action: '', status: '', tier: '', page: 1 });

  const job = useQuery({
    queryKey: ['job', jobId],
    queryFn: () => getJob(jobId),
    enabled: validId,
    refetchInterval: (q) => (q.state.data && isActive(q.state.data) ? POLL_MS : false),
  });
  const active = job.data ? isActive(job.data) : false;
  const summary = useQuery({
    queryKey: ['job', jobId, 'summary'],
    queryFn: () => itemSummary(jobId),
    enabled: !!job.data,
    refetchInterval: active ? POLL_MS * 2 : false,
  });
  // A sync that evaluated tier rules records each item's tier: its items can be grouped and
  // filtered by tier (phase2-3.md §16).
  const tiered = !!job.data && job.data.type === 'sync' && typeof job.data.stats?.tiers === 'object' && job.data.stats?.tiers !== null;
  const tierSummary = useQuery({
    queryKey: ['job', jobId, 'summary', 'tier'],
    queryFn: () => itemSummaryByTier(jobId),
    enabled: tiered,
    refetchInterval: active ? POLL_MS * 2 : false,
  });
  const items = useQuery({
    queryKey: ['job', jobId, 'items', filter.action, filter.status, filter.tier, filter.page],
    queryFn: () => listItems(jobId, { action: filter.action, status: filter.status, tier: filter.tier, page: filter.page, pageSize: ITEMS_PAGE_SIZE }),
    enabled: !!job.data,
    placeholderData: keepPreviousData,
    refetchInterval: active ? POLL_MS * 2 : false,
  });

  // When the job finishes, refresh the item summary and table once more.
  const wasActive = useRef(active);
  useEffect(() => {
    if (wasActive.current && !active) {
      void qc.invalidateQueries({ queryKey: ['job', jobId] });
    }
    wasActive.current = active;
  }, [active, jobId, qc]);

  const j = job.data;
  const counts = summary.data ?? [];
  const held = counts.filter((c) => c.status === 'held');
  const heldFiles = held.reduce((n, c) => n + c.files, 0);
  const destinationId = j?.params?.destinationId;
  const canSync = !!j && j.type === 'sync' && !!destinationId;
  // A release preview's held changes are applied only by its "Apply release" (whose confirmation
  // says what a release frees); a real release's held changes keep that release (§16).
  const releasePreview = !!j && isReleasePreview(j);
  const release: ReleaseParams = j ? releaseParamsOf(j) : {};
  const guard = useDestinations().data?.find((d) => d.id === destinationId)?.settings;
  const limits = guard
    ? `more than ${formatNumber(guard.maxChangePercent)}% of a source's files (and at least 20 files), or more than ${formatNumber(guard.maxChangeFiles)} files`
    : "more than the destination's limits allow (a share of a source's files and at least 20 files, or a fixed number of files)";

  async function startSync(allowChanges: boolean) {
    if (!destinationId) return;
    // Applying what a release held keeps its release (phase2-3.md §16): the params are copied.
    const next = await requestSync(destinationId, { dryRun: false, allowChanges, ...(allowChanges ? release : {}) });
    await qc.invalidateQueries({ queryKey: keys.jobs });
    navigate(`/activity/jobs/${next.id}`);
  }

  const title = j ? `${JOB_TYPE_LABELS[j.type] ?? j.type} #${j.id}` : validId ? `Job #${jobId}` : 'Job';
  return (
    <Page
      title={title}
      actions={
        <>
          <Link to={active ? '/activity/queue' : '/activity/history'} className="inline-flex items-center gap-2 rounded px-3 py-1.5 text-sm hover:bg-panel-2">
            <ArrowLeft className="h-4 w-4" aria-hidden="true" /> {active ? 'Queue' : 'History'}
          </Link>
          {j && canSync && j.dryRun && !active && !isReleasePreview(j) && (
            <Button variant="ghost" icon={Play} onClick={() => setConfirm('run')}>
              Run this sync
            </Button>
          )}
          {j && active && (
            <Button variant="ghost" icon={Square} onClick={() => setConfirm('cancel')}>
              Cancel job
            </Button>
          )}
        </>
      }
    >
      <ErrorNotice error={job.error} />
      {!validId && <Notice tone="error">There is no job with this id.</Notice>}
      {!j ? (
        validId && !job.error && <p className="text-ink-muted">Loading…</p>
      ) : (
        <>
          <JobHeader job={j} target={jobTarget(j, names)} />
          {j.dryRun && (
            <Notice tone="info" title="Preview (dry run)">
              {previewText(j)}
            </Notice>
          )}
          <ReleaseJobNotice job={j} />
          {heldFiles > 0 && (
            <Notice
              tone="warning"
              title={`${formatNumber(heldFiles)} ${heldFiles === 1 ? 'change' : 'changes'} ${j.dryRun ? 'would be' : heldFiles === 1 ? 'was' : 'were'} held by the mass-change guard`}
            >
              <p>
                {held.map((c) => `${formatNumber(c.files)} ${ACTION_LABELS[c.action].toLowerCase()}`).join(', ')}. A sync holds its retain and update items when
                they are {limits}, and always holds an update that would empty a file or shrink it to less than half its size (truncation or ransomware).
                {j.dryRun ? ' A real sync would move nothing for these files.' : ' Nothing was moved for these files.'}
              </p>
              <p className="mt-1">
                {releasePreview
                  ? 'Check them below. "Apply release" above also applies them; nothing else on this page does.'
                  : 'Check them below. If they are expected (for example you reorganised or upgraded the library), apply them.'}
              </p>
              <div className="mt-2 flex flex-wrap gap-2">
                <Button small onClick={() => setFilter({ action: '', status: 'held', tier: '', page: 1 })}>
                  Show held items
                </Button>
                {canSync && !releasePreview && (
                  <Button small variant="primary" icon={ShieldAlert} disabled={active} title={active ? 'Available when this job has finished' : undefined} onClick={() => setConfirm('apply')}>
                    Apply held changes
                  </Button>
                )}
              </div>
              {j.type === 'sync' && <DeletedIntegrationsHint />}
            </Notice>
          )}
          {active && (
            <section aria-label="Progress" className="mb-5 rounded border border-line bg-panel p-4">
              <JobProgressView job={j} label={title} />
              {j.status === 'running' && (
                <div className="mt-2 flex flex-wrap gap-x-6 gap-y-1 text-sm">
                  <span>
                    <span className="text-ink-muted">Phase:</span> <span className="capitalize">{j.progress?.phase || '—'}</span>
                  </span>
                  <span>
                    <span className="text-ink-muted">Speed:</span> {formatRate(j.progress?.bytesPerSec)}
                  </span>
                  <span>
                    <span className="text-ink-muted">ETA:</span> {formatEta(j.progress?.etaSeconds)}
                  </span>
                </div>
              )}
            </section>
          )}
          <JobStats stats={j.stats} dryRun={j.dryRun} />
          <TierJobStats stats={j.stats} />

          <h2 className="mb-2 text-lg">Items</h2>
          <ErrorNotice error={summary.error} />
          {summary.isSuccess && counts.length === 0 && !filter.action && !filter.status && !filter.tier ? (
            <p className="mb-6 text-sm text-ink-muted">{noItemsText(j)}</p>
          ) : (
            <>
              <ItemSummary counts={counts} dryRun={j.dryRun} onPick={(action, status) => setFilter({ action, status, tier: '', page: 1 })} />
              {tiered && <TierItemSummary counts={tierSummary.data ?? []} onPick={(tier) => setFilter({ action: '', status: '', tier, page: 1 })} />}
              <ItemFilters filter={filter} counts={counts} dryRun={j.dryRun} tiers={tiered} onChange={setFilter} />
              <ErrorNotice error={items.error} />
              <ItemTable items={items.data?.records} loading={items.isPending} dryRun={j.dryRun} />
              {items.data && items.data.totalRecords > 0 && (
                <Pagination page={filter.page} pageSize={ITEMS_PAGE_SIZE} total={items.data.totalRecords} onPage={(page) => setFilter({ ...filter, page })} />
              )}
              <details className="mb-6 mt-3 text-xs text-ink-muted">
                <summary className="cursor-pointer">What the actions mean</summary>
                <dl className="mt-2 grid gap-x-4 gap-y-1 sm:grid-cols-[6rem_1fr]">
                  {ACTION_ORDER.map((a) => (
                    <div key={a} className="contents">
                      <dt className="font-medium text-ink">{ACTION_LABELS[a]}</dt>
                      <dd>{ACTION_HELP[a]}</dd>
                    </div>
                  ))}
                </dl>
              </details>
            </>
          )}

          <h2 className="mb-2 mt-6 text-lg">Log</h2>
          <JobLogs jobId={jobId} live={active} />
        </>
      )}

      {j && confirm === 'cancel' && (
        <ConfirmDialog
          title="Cancel job"
          confirmLabel="Cancel job"
          cancelLabel={j.status === 'queued' ? 'Keep it queued' : 'Keep running'}
          danger
          onClose={() => setConfirm(null)}
          onConfirm={async () => {
            await cancelJob(j.id);
            await qc.invalidateQueries({ queryKey: ['job', jobId] });
          }}
        >
          <p>{cancelText(j)}</p>
        </ConfirmDialog>
      )}
      {j && confirm === 'apply' && (
        <ConfirmDialog title="Apply held changes" confirmLabel="Start sync" onClose={() => setConfirm(null)} onConfirm={() => startSync(true)}>
          <p>
            Start a sync of <strong>{jobTarget(j, names)}</strong> that also runs the changes the mass-change guard would hold (retained files move into
            retention, updated files keep their old version in retention).
          </p>
          {release.releaseDemoted && (
            <p>
              It also continues the release of preview #{release.releaseOf} (rule revision {release.releaseRevision}): the kept files that preview listed
              move into the destination&apos;s retention folder and are deleted after the retention period, while the rules are still at that revision.
            </p>
          )}
          <p className="text-ink-muted">The sync plans again from the current state of the sources, so it applies what is held now.</p>
        </ConfirmDialog>
      )}
      {j && confirm === 'run' && (
        <ConfirmDialog title="Run this sync" confirmLabel="Start sync" onClose={() => setConfirm(null)} onConfirm={() => startSync(false)}>
          <p>
            Start a real sync of <strong>{jobTarget(j, names)}</strong>. It plans again from the current state of the sources; changes the guard holds stay held.
          </p>
        </ConfirmDialog>
      )}
    </Page>
  );
}

/** previewText says what a dry run of the job's type did not change and what its items are. */
export function previewText(job: Pick<Job, 'type' | 'params'>): string {
  switch (job.type) {
    case 'sync':
      return 'Nothing was changed at the destination. The items below are what a sync would do right now: review them, then run the sync for real. Changes the mass-change guard would hold are listed as held.';
    case 'retention':
      return job.params?.destinationId
        ? 'Nothing was deleted. The items below are the retained files a retention run would delete now: their retention period is over.'
        : 'Nothing was deleted and no job history was pruned. This preview queued a preview for each enabled destination (linked in the log below): their items are the retained files a retention run would delete now.';
    case 'verify':
      return 'No file was read or marked. The items below are the files a verify would check right now.';
    default:
      return 'Nothing was written to the destination. The items below are what this job would do right now.';
  }
}

/** noItemsText explains an empty item list: scans and some jobs never plan file items. */
export function noItemsText(job: Pick<Job, 'type' | 'status' | 'params'>): string {
  if (isActive(job)) {
    return 'No items yet: the job is still scanning or planning.';
  }
  if (job.type === 'scan') {
    return 'A scan updates the catalog and plans no file items; the figures above and the log show what it found.';
  }
  if (job.type === 'retention' && !job.params?.destinationId && job.status === 'completed') {
    return 'This job plans no file items: it queues a retention job for each enabled destination (linked in the log below), whose items are the files.';
  }
  if (job.status === 'failed' || job.status === 'cancelled') {
    return 'No items: the job stopped before it planned any file work. The log shows why.';
  }
  return 'No items: there was no file work to do.';
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-ink-muted">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </div>
  );
}

function JobHeader({ job, target }: { job: Job; target: string }) {
  const duration = secondsBetween(job.startedAt, job.finishedAt);
  return (
    <section aria-label="Summary" className="mb-5 rounded border border-line bg-panel p-4">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <JobStatusBadge status={job.status} />
        <JobBadges job={job} />
        <span className="font-medium">{target}</span>
      </div>
      {job.summary && <p className="mb-2">{job.summary}</p>}
      {job.error && (
        <p role="alert" className="mb-2 text-danger">
          {job.error}
        </p>
      )}
      <dl className="grid grid-cols-[7rem_1fr] gap-x-4 gap-y-1 text-sm sm:grid-cols-[7rem_1fr_7rem_1fr]">
        <Field label="Trigger">{TRIGGER_LABELS[job.trigger] ?? job.trigger}</Field>
        <Field label="Queued">{formatDateTime(job.queuedAt)}</Field>
        <Field label="Started">{formatDateTime(job.startedAt)}</Field>
        <Field label="Finished">{formatDateTime(job.finishedAt)}</Field>
        <Field label="Duration">{formatDuration(duration)}</Field>
        <Field label="Warnings">{formatNumber(job.warnings)}</Field>
      </dl>
    </section>
  );
}

function ItemSummary({ counts, dryRun, onPick }: { counts: ItemCount[]; dryRun: boolean; onPick: (action: ItemAction, status: ItemStatus) => void }) {
  if (counts.length === 0) {
    return null;
  }
  const actions = ACTION_ORDER.filter((a) => counts.some((c) => c.action === a));
  for (const c of counts) if (!actions.includes(c.action)) actions.push(c.action);
  const statuses = ITEM_STATUS_ORDER.filter((s) => counts.some((c) => c.status === s));
  const cell = (a: ItemAction, s: ItemStatus) => counts.find((c) => c.action === a && c.status === s);
  const statusLabel = (s: ItemStatus) => (dryRun && s === 'pending' ? 'Planned' : ITEM_STATUS_LABELS[s]);

  return (
    <div className="relative mb-4 overflow-x-auto rounded border border-line bg-panel">
      <table className="w-full text-sm">
        <caption className="sr-only">Items by action and status</caption>
        <thead>
          <tr className="border-b border-line text-left text-ink-muted">
            <th scope="col" className="px-3 py-2 font-medium">
              Action
            </th>
            {statuses.map((s) => (
              <th key={s} scope="col" className="px-3 py-2 text-right font-medium">
                {statusLabel(s)}
              </th>
            ))}
            <th scope="col" className="px-3 py-2 text-right font-medium">
              Total
            </th>
          </tr>
        </thead>
        <tbody>
          {actions.map((a) => {
            const row = counts.filter((c) => c.action === a);
            const files = row.reduce((n, c) => n + c.files, 0);
            const bytes = row.reduce((n, c) => n + c.bytes, 0);
            return (
              <tr key={a} className="border-b border-line/60 last:border-b-0">
                <th scope="row" className="px-3 py-2 text-left font-normal">
                  <ActionBadge action={a} />
                </th>
                {statuses.map((s) => {
                  const c = cell(a, s);
                  return (
                    <td key={s} className="px-3 py-1.5 text-right">
                      {c ? (
                        <button
                          type="button"
                          className="rounded px-1 hover:bg-panel-2 hover:text-accent"
                          aria-label={`Show ${ACTION_LABELS[a].toLowerCase()} items: ${statusLabel(s).toLowerCase()}`}
                          onClick={() => onPick(a, s)}
                        >
                          {formatNumber(c.files)}
                          <span className="block text-xs text-ink-muted">{formatBytes(c.bytes)}</span>
                        </button>
                      ) : (
                        <span className="text-ink-muted">—</span>
                      )}
                    </td>
                  );
                })}
                <td className="px-3 py-1.5 text-right font-medium">
                  {formatNumber(files)}
                  <span className="block text-xs font-normal text-ink-muted">{formatBytes(bytes)}</span>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

type ItemFilter = { action: ItemAction | ''; status: ItemStatus | ''; tier: Tier | ''; page: number };

/**
 * tierGroups sums a job's items by the tier each records (GET /jobs/{id}/items/summary?by=tier),
 * in tier order; rows without a tier are left out.
 */
export function tierGroups(counts: TierItemCount[]): { tier: Tier; files: number; bytes: number }[] {
  return TIERS.map((tier) => {
    const rows = counts.filter((c) => c.tier === tier);
    return { tier, files: rows.reduce((n, c) => n + c.files, 0), bytes: rows.reduce((n, c) => n + c.bytes, 0) };
  }).filter((g) => g.files > 0);
}

/** TierItemSummary groups a tiered sync's items by tier; picking a tier filters the items. */
function TierItemSummary({ counts, onPick }: { counts: TierItemCount[]; onPick: (tier: Tier) => void }) {
  const groups = tierGroups(counts);
  if (groups.length === 0) {
    return null;
  }
  return (
    <div role="group" aria-label="Items by tier" className="mb-3 flex flex-wrap items-center gap-2 text-sm">
      <span className="text-ink-muted">By tier:</span>
      {groups.map((g) => (
        <button
          key={g.tier}
          type="button"
          className="rounded border border-line px-2 py-1 hover:bg-panel-2 hover:text-accent"
          aria-label={`Show ${TIER_LABELS[g.tier].toLowerCase()} items`}
          onClick={() => onPick(g.tier)}
        >
          {TIER_LABELS[g.tier]}: {formatNumber(g.files)} <span className="text-xs text-ink-muted">({formatBytes(g.bytes)})</span>
        </button>
      ))}
    </div>
  );
}

function ItemFilters({
  filter,
  counts,
  dryRun,
  tiers,
  onChange,
}: {
  filter: ItemFilter;
  counts: ItemCount[];
  dryRun: boolean;
  tiers: boolean;
  onChange: (f: ItemFilter) => void;
}) {
  const actions = ACTION_ORDER.filter((a) => counts.length === 0 || counts.some((c) => c.action === a) || a === filter.action);
  const statuses = ITEM_STATUS_ORDER.filter((s) => counts.length === 0 || counts.some((c) => c.status === s) || s === filter.status);
  return (
    <div className="mb-3 flex flex-wrap items-end gap-3">
      <label className="text-sm">
        <span className="mb-1 block text-ink-muted">Action</span>
        <select className={`${inputClass} w-40`} value={filter.action} onChange={(e) => onChange({ ...filter, action: e.target.value as ItemAction | '', page: 1 })}>
          <option value="">All actions</option>
          {actions.map((a) => (
            <option key={a} value={a}>
              {ACTION_LABELS[a]}
            </option>
          ))}
        </select>
      </label>
      <label className="text-sm">
        <span className="mb-1 block text-ink-muted">Status</span>
        <select className={`${inputClass} w-40`} value={filter.status} onChange={(e) => onChange({ ...filter, status: e.target.value as ItemStatus | '', page: 1 })}>
          <option value="">All statuses</option>
          {statuses.map((s) => (
            <option key={s} value={s}>
              {dryRun && s === 'pending' ? 'Planned' : ITEM_STATUS_LABELS[s]}
            </option>
          ))}
        </select>
      </label>
      {(tiers || filter.tier) && (
        <label className="text-sm">
          <span className="mb-1 block text-ink-muted">Tier</span>
          <select className={`${inputClass} w-40`} value={filter.tier} onChange={(e) => onChange({ ...filter, tier: e.target.value as Tier | '', page: 1 })}>
            <option value="">All tiers</option>
            {TIERS.map((t) => (
              <option key={t} value={t}>
                {TIER_LABELS[t]}
              </option>
            ))}
          </select>
        </label>
      )}
      {(filter.action || filter.status || filter.tier) && (
        <Button variant="ghost" onClick={() => onChange({ action: '', status: '', tier: '', page: 1 })}>
          Clear filters
        </Button>
      )}
    </div>
  );
}

/**
 * NOTE_LABELS pick the item detail fields shown inline (internal/syncer Detail): why the planner
 * chose the action, what execution did instead, and where files came from or went.
 */
const NOTE_LABELS: [key: string, label: string][] = [
  ['reason', 'reason'],
  ['outcome', 'outcome'],
  ['from', 'from'],
  ['primary', 'hardlink of'],
  ['displaced', 'unmanaged file moved to'],
  ['retained', 'old version kept at'],
  ['check', 'check'],
  ['note', 'tier'],
];

/** itemNotes lists an item's notable detail fields as "label: value". */
export function itemNotes(detail: Record<string, unknown> | null | undefined): string[] {
  if (!detail || typeof detail !== 'object') {
    return [];
  }
  return NOTE_LABELS.filter(([k]) => typeof detail[k] === 'string' && detail[k]).map(([k, label]) => `${label}: ${String(detail[k])}`);
}

function ItemNote({ item }: { item: JobItem }) {
  const detail = item.detail && typeof item.detail === 'object' ? item.detail : null;
  const notes = itemNotes(detail);
  const hasDetail = detail && Object.keys(detail).length > 0;
  return (
    <div className="text-xs">
      {/* A held item's "error" is the guard's reason, not a failure. */}
      {item.error && <div className={item.status === 'held' ? 'text-warn' : 'text-danger'}>{item.error}</div>}
      {notes.length > 0 && <div className="break-all text-ink-muted">{notes.join(' · ')}</div>}
      {hasDetail && (
        <details className="text-ink-muted">
          <summary className="cursor-pointer">Detail</summary>
          <pre className="mt-1 whitespace-pre-wrap break-all">{JSON.stringify(detail, null, 2)}</pre>
        </details>
      )}
    </div>
  );
}

function ItemTable({ items, loading, dryRun }: { items: JobItem[] | undefined; loading: boolean; dryRun: boolean }) {
  const columns: Column<JobItem>[] = [
    { key: 'action', header: 'Action', cell: (i) => <ActionBadge action={i.action} /> },
    { key: 'status', header: 'Status', cell: (i) => <ItemStatusBadge status={i.status} dryRun={dryRun} /> },
    { key: 'path', header: 'Path', className: 'w-[50%]', cell: (i) => <span className="break-all font-mono text-xs">{i.relPath}</span> },
    { key: 'size', header: 'Size', className: 'whitespace-nowrap text-right', cell: (i) => formatBytes(i.bytes) },
    { key: 'note', header: 'Notes', cell: (i) => <ItemNote item={i} /> },
  ];
  return (
    <DataTable
      columns={columns}
      rows={items}
      rowKey={(i) => i.id}
      loading={loading}
      caption="Job items"
      empty="No items match."
      rowClassName={(i) => (i.status === 'held' ? 'bg-warn/5' : i.status === 'failed' ? 'bg-danger/5' : '')}
    />
  );
}
