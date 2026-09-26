import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Archive, Eye, FileText, FlaskConical, HardDrive, Pencil, Play, Plus, RefreshCw, ShieldCheck, Trash2 } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { Link, useNavigate } from 'react-router';
import { errorMessage } from '@/api/client';
import { deleteDestination, listSnapshots, syncDestination, testDestination, verifyDestination } from '@/api/destinations';
import type { Destination, DestinationTestResult, Job } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { DataTable, type Column } from '@/components/DataTable';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { EmptyState, Page } from '@/components/Page';
import { Badge, JobStatusBadge } from '@/components/StatusBadge';
import { describeCron } from '@/lib/cron';
import { formatBytes, formatDateTime, formatRelative } from '@/lib/format';
import { JOB_TYPE_LABELS } from '@/lib/labels';
import { keys, useDestinations, useIntegrations, useSources } from '@/lib/lookups';
import { DestinationForm } from './DestinationForm';
import { ManifestsDialog } from './ManifestsDialog';
import { CapabilityBadges, TestResultView } from './TestResultView';

type Dialog =
  | { kind: 'edit'; destination: Destination | null }
  | { kind: 'delete'; destination: Destination }
  | { kind: 'test'; destination: Destination }
  | { kind: 'snapshots'; destination: Destination }
  | { kind: 'manifests'; destination: Destination }
  | null;

/** Destinations: where backups go (mounted shares), their schedules and the actions that run jobs. */
export function Destinations() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const destinations = useDestinations();
  const sources = useSources();
  const [dialog, setDialog] = useState<Dialog>(null);
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const sourceName = (id: number) => sources.data?.find((s) => s.id === id)?.name ?? `#${id}`;

  async function start(d: Destination, what: 'preview' | 'sync' | 'verify') {
    setNotice(null);
    setBusy(`${what}:${d.id}`);
    try {
      let job: Job;
      if (what === 'verify') job = await verifyDestination(d.id);
      else job = await syncDestination(d.id, { dryRun: what === 'preview', allowChanges: false });
      await qc.invalidateQueries({ queryKey: keys.jobs });
      if (what === 'preview') {
        navigate(`/activity/jobs/${job.id}`);
        return;
      }
      setNotice({
        tone: 'success',
        body: (
          <>
            {JOB_TYPE_LABELS[job.type] ?? 'Job'} of {d.name} queued.{' '}
            <Link className="text-accent hover:underline" to={`/activity/jobs/${job.id}`}>
              View job #{job.id}
            </Link>
          </>
        ),
      });
    } catch (e) {
      setNotice({ tone: 'error', body: `${d.name}: ${errorMessage(e)}` });
    } finally {
      setBusy(null);
    }
  }

  const columns: Column<Destination>[] = [
    {
      key: 'name',
      header: 'Destination',
      cell: (d) => (
        <div className="min-w-[10rem]">
          <span className="font-medium">{d.name}</span>
          {!d.enabled && (
            <span className="ml-2">
              <Badge>Disabled</Badge>
            </span>
          )}
          <div className="break-all font-mono text-xs text-ink-muted">{d.target}</div>
          <div className="mt-1 flex flex-wrap gap-1">
            {d.fsType && <Badge>{d.fsType}</Badge>}
            <CapabilityBadges caps={d.capabilities} />
          </div>
        </div>
      ),
    },
    {
      key: 'sources',
      header: 'Sources',
      cell: (d) => (d.sourceIds?.length ? <span className="text-sm">{d.sourceIds.map(sourceName).join(', ')}</span> : <span className="text-warn">None</span>),
    },
    {
      key: 'schedule',
      header: 'Schedule',
      cell: (d) => (
        <div className="text-xs">
          <div>Sync: {d.schedule?.enabled ? describeCron(d.schedule.cron) : 'manual'}</div>
          <div className="text-ink-muted">Verify: {d.verifySchedule?.enabled ? describeCron(d.verifySchedule.cron) : 'manual'}</div>
        </div>
      ),
    },
    {
      // The last sync (design §10): later verify, retention and Plex DB backup jobs must not hide
      // a failed sync or held changes.
      key: 'last',
      header: 'Last sync',
      className: 'whitespace-nowrap',
      cell: (d) =>
        d.lastSync ? (
          <Link to={`/activity/jobs/${d.lastSync.id}`} className="block hover:underline" title={d.lastSync.summary || undefined}>
            <span className="flex items-center gap-1">
              <JobStatusBadge status={d.lastSync.status} />
            </span>
            <span className="text-xs text-ink-muted" title={formatDateTime(d.lastSync.finishedAt ?? d.lastSync.queuedAt)}>
              {formatRelative(d.lastSync.finishedAt ?? d.lastSync.startedAt ?? d.lastSync.queuedAt)}
            </span>
          </Link>
        ) : (
          <span className="text-xs text-ink-muted">Never synced</span>
        ),
    },
    {
      key: 'actions',
      header: <span className="sr-only">Actions</span>,
      className: 'whitespace-nowrap text-right',
      cell: (d) => (
        <div className="flex flex-wrap justify-end gap-1">
          <Button small variant="ghost" icon={FlaskConical} onClick={() => setDialog({ kind: 'test', destination: d })} aria-label={`Test ${d.name}`}>
            Test
          </Button>
          <Button small variant="ghost" icon={Eye} busy={busy === `preview:${d.id}`} onClick={() => void start(d, 'preview')} aria-label={`Preview a sync of ${d.name}`}>
            Preview
          </Button>
          <Button small variant="ghost" icon={Play} busy={busy === `sync:${d.id}`} onClick={() => void start(d, 'sync')} aria-label={`Sync ${d.name} now`}>
            Sync now
          </Button>
          <Button small variant="ghost" icon={ShieldCheck} busy={busy === `verify:${d.id}`} onClick={() => void start(d, 'verify')} aria-label={`Verify ${d.name}`}>
            Verify
          </Button>
          <IconButton label={`Snapshots on ${d.name}`} icon={Archive} onClick={() => setDialog({ kind: 'snapshots', destination: d })} />
          <IconButton label={`Manifests on ${d.name}`} icon={FileText} onClick={() => setDialog({ kind: 'manifests', destination: d })} />
          <IconButton label={`Edit ${d.name}`} icon={Pencil} onClick={() => setDialog({ kind: 'edit', destination: d })} />
          <IconButton label={`Delete ${d.name}`} icon={Trash2} className="hover:text-danger" onClick={() => setDialog({ kind: 'delete', destination: d })} />
        </div>
      ),
    },
  ];

  const list = destinations.data;
  return (
    <Page
      title="Destinations"
      actions={
        <>
          <Button variant="ghost" icon={Plus} onClick={() => setDialog({ kind: 'edit', destination: null })}>
            Add destination
          </Button>
          <Button variant="ghost" icon={RefreshCw} onClick={() => void destinations.refetch()}>
            Refresh
          </Button>
        </>
      }
    >
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      <ErrorNotice error={destinations.error} />
      {list && list.length === 0 ? (
        <EmptyState icon={HardDrive} title="No destinations yet">
          <p>A destination is a mounted share (for example a UniFi UNAS over NFS or SMB) that receives a verified mirror of your sources, with deleted and replaced files kept in retention.</p>
          <div className="mt-4">
            <Button variant="primary" icon={Plus} onClick={() => setDialog({ kind: 'edit', destination: null })}>
              Add destination
            </Button>
          </div>
        </EmptyState>
      ) : (
        <DataTable columns={columns} rows={list} rowKey={(d) => d.id} loading={destinations.isPending} caption="Destinations" />
      )}

      {dialog?.kind === 'edit' && <DestinationForm destination={dialog.destination} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'test' && <TestDialog destination={dialog.destination} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'snapshots' && <SnapshotsDialog destination={dialog.destination} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'manifests' && <ManifestsDialog destination={dialog.destination} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'delete' && (
        <ConfirmDialog
          title="Delete destination"
          confirmLabel="Delete"
          danger
          onClose={() => setDialog(null)}
          onConfirm={async () => {
            await deleteDestination(dialog.destination.id);
            await qc.invalidateQueries({ queryKey: keys.destinations });
            await qc.invalidateQueries({ queryKey: keys.schedules });
          }}
        >
          <p>
            Delete <strong>{dialog.destination.name}</strong> from Bunkarr?
          </p>
          <p className="text-ink-muted">
            Its schedules and records are removed. The backup data at <code>{dialog.destination.target}</code> is not touched; you can attach to it again later.
          </p>
        </ConfirmDialog>
      )}
    </Page>
  );
}

function TestDialog({ destination, onClose }: { destination: Destination; onClose: () => void }) {
  const qc = useQueryClient();
  const test = useQuery<DestinationTestResult>({
    queryKey: ['destination', destination.id, 'test'],
    queryFn: async () => {
      const r = await testDestination(destination.id);
      await qc.invalidateQueries({ queryKey: keys.destinations });
      return r;
    },
    gcTime: 0,
    retry: false,
  });
  return (
    <Modal
      title={`Test · ${destination.name}`}
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button icon={RefreshCw} busy={test.isFetching} onClick={() => void test.refetch()}>
            Test again
          </Button>
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        </>
      }
    >
      <ErrorNotice error={test.error} />
      {test.isPending ? <p className="text-sm text-ink-muted">Probing {destination.target}…</p> : test.data && <TestResultView result={test.data} />}
    </Modal>
  );
}

function SnapshotsDialog({ destination, onClose }: { destination: Destination; onClose: () => void }) {
  const snapshots = useQuery({ queryKey: ['destination', destination.id, 'snapshots'], queryFn: () => listSnapshots(destination.id) });
  const integrations = useIntegrations();
  // integrationId is 0 once the Plex server or *arr was deleted from Bunkarr (the backup stays).
  const serverName = (id: number) => (id > 0 ? (integrations.data?.find((i) => i.id === id)?.name ?? `integration #${id}`) : 'Deleted server');
  const columns: Column<NonNullable<typeof snapshots.data>[number]>[] = [
    { key: 'created', header: 'Created', className: 'whitespace-nowrap', cell: (s) => <span title={formatRelative(s.createdAt)}>{formatDateTime(s.createdAt)}</span> },
    { key: 'kind', header: 'Kind', cell: (s) => (s.kind === 'arr' ? '*arr backup' : 'Plex DB') },
    { key: 'server', header: 'Backed up', cell: (s) => serverName(s.integrationId) },
    { key: 'size', header: 'Size', className: 'whitespace-nowrap text-right', cell: (s) => formatBytes(s.size) },
    {
      key: 'integrity',
      header: 'Integrity',
      cell: (s) => (s.integrity === 'ok' ? <Badge tone="ok">OK</Badge> : <Badge tone="danger">Failed</Badge>),
    },
    {
      key: 'path',
      header: 'Path',
      cell: (s) => (
        <div>
          <code className="break-all text-xs">{s.path}</code>
          <div className="text-xs text-ink-muted">{s.method}</div>
          {s.manifest && Object.keys(s.manifest).length > 0 && (
            <details className="text-xs text-ink-muted">
              <summary className="cursor-pointer">Manifest</summary>
              <pre className="mt-1 max-h-60 overflow-auto whitespace-pre-wrap break-all">{JSON.stringify(s.manifest, null, 2)}</pre>
            </details>
          )}
        </div>
      ),
    },
  ];
  return (
    <Modal
      title={`Snapshots · ${destination.name}`}
      size="xl"
      onClose={onClose}
      footer={
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <p className="mb-3 text-xs text-ink-muted">
        Verified copies of the Plex database, blobs database and Preferences.xml (under <code>.bunkarr/plex/</code>), and of the *arr apps&apos; backup zips
        (under <code>.bunkarr/arr/</code>). Preferences.xml contains the server&apos;s Plex token and an *arr zip its API key and passwords: protect the share
        accordingly. Neither is offered for download.
      </p>
      <ErrorNotice error={snapshots.error} />
      <DataTable columns={columns} rows={snapshots.data} rowKey={(s) => s.id} loading={snapshots.isPending} empty="No Plex database or *arr backups on this destination yet." caption="Snapshots" />
    </Modal>
  );
}
