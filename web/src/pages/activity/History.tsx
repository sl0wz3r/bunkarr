import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { History as HistoryIcon, RefreshCw } from 'lucide-react';
import { useSearchParams } from 'react-router';
import { listJobs } from '@/api/jobs';
import type { Job, JobStatus, JobType } from '@/api/types';
import { Button } from '@/components/Button';
import { DataTable, type Column } from '@/components/DataTable';
import { inputClass } from '@/components/Form';
import { ErrorNotice } from '@/components/Notice';
import { EmptyState, Page } from '@/components/Page';
import { Pagination } from '@/components/Pagination';
import { JobStatusBadge } from '@/components/StatusBadge';
import { formatDateTime, formatDuration, formatRelative, secondsBetween } from '@/lib/format';
import { JOB_STATUS_LABELS, JOB_TYPE_LABELS } from '@/lib/labels';
import { keys, useNames } from '@/lib/lookups';
import { JobCell } from './JobParts';

export const HISTORY_PAGE_SIZE = 25;

const FINAL_STATUSES: JobStatus[] = ['completed', 'completed_with_warnings', 'failed', 'cancelled'];

/** Activity → History: finished jobs, filterable by type and status, paged (state in the URL). */
export function History() {
  const names = useNames();
  const [params, setParams] = useSearchParams();
  const type = (params.get('type') ?? '') as JobType | '';
  const status = (params.get('status') ?? '') as JobStatus | '';
  const page = Math.max(1, Number(params.get('page')) || 1);

  const jobs = useQuery({
    queryKey: [...keys.jobs, 'finished', type, status, page],
    queryFn: () => listJobs({ state: 'finished', type, status, page, pageSize: HISTORY_PAGE_SIZE }),
    placeholderData: keepPreviousData,
  });

  function update(next: Record<string, string>) {
    const p = new URLSearchParams(params);
    for (const [k, v] of Object.entries(next)) {
      if (v) p.set(k, v);
      else p.delete(k);
    }
    setParams(p);
  }

  const columns: Column<Job>[] = [
    { key: 'job', header: 'Job', cell: (j) => <JobCell job={j} names={names} /> },
    { key: 'status', header: 'Status', cell: (j) => <JobStatusBadge status={j.status} /> },
    {
      key: 'started',
      header: 'Started',
      className: 'whitespace-nowrap',
      cell: (j) => <span title={formatDateTime(j.startedAt ?? j.queuedAt)}>{formatRelative(j.startedAt ?? j.queuedAt)}</span>,
    },
    { key: 'duration', header: 'Duration', className: 'whitespace-nowrap', cell: (j) => formatDuration(secondsBetween(j.startedAt, j.finishedAt)) },
    {
      key: 'summary',
      header: 'Summary',
      className: 'w-[40%]',
      cell: (j) => (
        <div className="text-xs">
          {j.summary && <div>{j.summary}</div>}
          {j.error && <div className="text-danger">{j.error}</div>}
          {j.warnings > 0 && <div className="text-warn">{j.warnings === 1 ? '1 warning' : `${j.warnings} warnings`}</div>}
        </div>
      ),
    },
  ];

  const filtered = type !== '' || status !== '';
  const data = jobs.data;
  return (
    <Page
      title="History"
      actions={
        <Button variant="ghost" icon={RefreshCw} onClick={() => void jobs.refetch()}>
          Refresh
        </Button>
      }
    >
      <div className="mb-4 flex flex-wrap items-end gap-3">
        <label className="text-sm">
          <span className="mb-1 block text-ink-muted">Type</span>
          <select className={`${inputClass} w-44`} value={type} onChange={(e) => update({ type: e.target.value, page: '' })}>
            <option value="">All types</option>
            {Object.entries(JOB_TYPE_LABELS).map(([v, l]) => (
              <option key={v} value={v}>
                {l}
              </option>
            ))}
          </select>
        </label>
        <label className="text-sm">
          <span className="mb-1 block text-ink-muted">Status</span>
          <select className={`${inputClass} w-44`} value={status} onChange={(e) => update({ status: e.target.value, page: '' })}>
            <option value="">All statuses</option>
            {FINAL_STATUSES.map((s) => (
              <option key={s} value={s}>
                {JOB_STATUS_LABELS[s]}
              </option>
            ))}
          </select>
        </label>
        {filtered && (
          <Button variant="ghost" onClick={() => update({ type: '', status: '', page: '' })}>
            Clear filters
          </Button>
        )}
      </div>
      <ErrorNotice error={jobs.error} />
      {data && data.totalRecords === 0 && !filtered ? (
        <EmptyState icon={HistoryIcon} title="No history yet">
          Finished scans, syncs, verifications and Plex database backups are listed here with their results and logs.
        </EmptyState>
      ) : (
        <>
          <DataTable columns={columns} rows={data?.records} rowKey={(j) => j.id} loading={jobs.isPending} empty="No jobs match these filters." caption="Finished jobs" />
          {data && data.totalRecords > 0 && (
            <Pagination page={page} pageSize={HISTORY_PAGE_SIZE} total={data.totalRecords} onPage={(p) => update({ page: p > 1 ? String(p) : '' })} />
          )}
        </>
      )}
    </Page>
  );
}
