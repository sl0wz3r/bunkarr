import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ListOrdered, RefreshCw, Square } from 'lucide-react';
import { useState } from 'react';
import { cancelJob, listJobs } from '@/api/jobs';
import type { Job } from '@/api/types';
import { Button } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { DataTable, type Column } from '@/components/DataTable';
import { ErrorNotice } from '@/components/Notice';
import { EmptyState, Page } from '@/components/Page';
import { JobStatusBadge } from '@/components/StatusBadge';
import { formatEta, formatRate } from '@/lib/format';
import { jobTitle, keys, useNames } from '@/lib/lookups';
import { POLL_MS } from '@/lib/queryClient';
import { cancelText, JobCell, JobProgressView } from './JobParts';

/** Activity → Queue: queued and running jobs with live progress (polls every 2 s). */
export function Queue() {
  const names = useNames();
  const qc = useQueryClient();
  const [cancelling, setCancelling] = useState<Job | null>(null);
  const jobs = useQuery({
    queryKey: [...keys.jobs, 'active'],
    queryFn: () => listJobs({ state: 'active', page: 1, pageSize: 100 }),
    refetchInterval: POLL_MS,
  });

  const columns: Column<Job>[] = [
    { key: 'job', header: 'Job', cell: (j) => <JobCell job={j} names={names} /> },
    {
      key: 'status',
      header: 'Status',
      cell: (j) => (
        <div>
          <JobStatusBadge status={j.status} />
          {j.status === 'running' && j.progress?.phase && <div className="mt-1 text-xs capitalize text-ink-muted">{j.progress.phase}</div>}
        </div>
      ),
    },
    { key: 'progress', header: 'Progress', className: 'w-[40%]', cell: (j) => <JobProgressView job={j} label={jobTitle(j, names)} /> },
    { key: 'speed', header: 'Speed', className: 'whitespace-nowrap', cell: (j) => (j.status === 'running' ? formatRate(j.progress?.bytesPerSec) : '—') },
    { key: 'eta', header: 'ETA', className: 'whitespace-nowrap', cell: (j) => (j.status === 'running' ? formatEta(j.progress?.etaSeconds) : '—') },
    {
      key: 'actions',
      header: <span className="sr-only">Actions</span>,
      className: 'text-right',
      cell: (j) => (
        <Button small variant="ghost" icon={Square} aria-label={`Cancel job #${j.id}`} onClick={() => setCancelling(j)}>
          Cancel
        </Button>
      ),
    },
  ];

  const list = jobs.data?.records;
  return (
    <Page
      title="Queue"
      actions={
        <Button variant="ghost" icon={RefreshCw} onClick={() => void jobs.refetch()}>
          Refresh
        </Button>
      }
    >
      <ErrorNotice error={jobs.error} />
      {list && list.length === 0 ? (
        <EmptyState icon={ListOrdered} title="Nothing running">
          Scans, syncs, verifications and Plex database backups appear here while they are queued or running, with their progress and throughput.
        </EmptyState>
      ) : (
        <DataTable columns={columns} rows={list} rowKey={(j) => j.id} loading={jobs.isPending} caption="Active jobs" />
      )}
      {cancelling && (
        <ConfirmDialog
          title="Cancel job"
          confirmLabel="Cancel job"
          cancelLabel={cancelling.status === 'queued' ? 'Keep it queued' : 'Keep running'}
          danger
          onClose={() => setCancelling(null)}
          onConfirm={async () => {
            await cancelJob(cancelling.id);
            await qc.invalidateQueries({ queryKey: keys.jobs });
          }}
        >
          <p>
            Stop <strong>{jobTitle(cancelling, names)}</strong> (#{cancelling.id})?
          </p>
          <p className="text-ink-muted">{cancelText(cancelling)}</p>
        </ConfirmDialog>
      )}
    </Page>
  );
}
