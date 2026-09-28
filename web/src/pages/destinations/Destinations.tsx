import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Archive, Eraser, Eye, FileText, FlaskConical, HardDrive, KeyRound, Lock, Pencil, Play, Plus, RefreshCw, ShieldCheck, Trash2, Unlock } from 'lucide-react';
import { useEffect, useState, type ReactNode } from 'react';
import { Link, useNavigate } from 'react-router';
import { errorMessage } from '@/api/client';
import { getJob } from '@/api/jobs';
import { deleteDestination, listSnapshots, runRetention, syncDestination, testDestination, testStoredDestination, unlockDestination, verifyDestination } from '@/api/destinations';
import type { Destination, DestinationTestResult, EngineTestResult, Job, Snapshot } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { Checkbox } from '@/components/Form';
import { DataTable, type Column } from '@/components/DataTable';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { EmptyState, Page } from '@/components/Page';
import { Badge, JobStatusBadge } from '@/components/StatusBadge';
import { describeWindow, formatClock, formatKiBps, queuedPollDelay } from '@/lib/bandwidth';
import { describeCron } from '@/lib/cron';
import { ENGINE_BADGES, KIND_BADGES, isEncrypted, isEngine, kitState, repositoryBytes, snapshotCount } from '@/lib/destinationKinds';
import { shortId } from '@/lib/engineStats';
import { formatBytes, formatDateTime, formatNumber, formatRelative } from '@/lib/format';
import { JOB_TYPE_LABELS } from '@/lib/labels';
import { keys, useDestinations, useIntegrations, useSources } from '@/lib/lookups';
import { DestinationForm } from './DestinationForm';
import { ManifestsDialog } from './ManifestsDialog';
import { RecoveryKitDialog } from './RecoveryKit';
import { CapabilityBadges, EngineTestView, TestResultView } from './TestResultView';

type Dialog =
  | { kind: 'edit'; destination: Destination | null; resume?: Destination }
  | { kind: 'delete'; destination: Destination }
  | { kind: 'test'; destination: Destination }
  | { kind: 'snapshots'; destination: Destination }
  | { kind: 'manifests'; destination: Destination }
  | { kind: 'kit'; destination: Destination }
  | { kind: 'unlockAll'; destination: Destination }
  | null;

/**
 * QueuedNotice says a job was queued and, outside its destination's transfer window, when it
 * starts (§9.2). The queue answers before the job is picked up; the job's notBefore is set only
 * once it defers, so a destination with a window has the job re-read (queuedPollDelay) until it
 * runs or defers, and the list is refreshed to show its waiting time.
 */
function QueuedNotice({ job, label, name, windowed }: { job: Job; label: string; name: string; windowed: boolean }) {
  const qc = useQueryClient();
  const live = useQuery({
    queryKey: [...keys.jobs, job.id, 'queued'],
    queryFn: () => getJob(job.id),
    enabled: windowed && !job.notBefore && job.status === 'queued',
    refetchInterval: (q) => queuedPollDelay(q.state.data, q.state.dataUpdateCount),
  });
  const notBefore = job.notBefore ?? (live.data?.status === 'queued' ? live.data.notBefore : null);
  const deferred = !!notBefore && !job.notBefore;
  useEffect(() => {
    if (deferred) void qc.invalidateQueries({ queryKey: keys.destinations });
  }, [deferred, qc]);
  return (
    <>
      {label} of {name} queued
      {notBefore ? `: it waits for the transfer window and starts at ${formatClock(notBefore)}` : ''}.{' '}
      <Link className="text-accent hover:underline" to={`/activity/jobs/${job.id}`}>
        View job #{job.id}
      </Link>
    </>
  );
}

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

  async function start(d: Destination, what: 'preview' | 'sync' | 'verify' | 'prune') {
    setNotice(null);
    setBusy(`${what}:${d.id}`);
    try {
      let job: Job;
      if (what === 'verify') job = await verifyDestination(d.id);
      else if (what === 'prune') job = await runRetention(d.id, { prune: true });
      else job = await syncDestination(d.id, { dryRun: what === 'preview', allowChanges: false });
      await qc.invalidateQueries({ queryKey: keys.jobs });
      await qc.invalidateQueries({ queryKey: keys.destinations });
      if (what === 'preview') {
        navigate(`/activity/jobs/${job.id}`);
        return;
      }
      setNotice({
        tone: 'success',
        body: <QueuedNotice job={job} label={what === 'prune' ? 'Retention with a prune' : (JOB_TYPE_LABELS[job.type] ?? 'Job')} name={d.name} windowed={!!d.bandwidth?.window} />,
      });
    } catch (e) {
      setNotice({ tone: 'error', body: `${d.name}: ${errorMessage(e)}` });
    } finally {
      setBusy(null);
    }
  }

  async function unlock(d: Destination) {
    setNotice(null);
    setBusy(`unlock:${d.id}`);
    try {
      await unlockDestination(d.id, false);
      setNotice({ tone: 'success', body: `Stale locks of ${d.name} removed.` });
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
      cell: (d) => <DestinationSummary destination={d} />,
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
          {d.retentionSchedule && (
            <div className="text-ink-muted">Retention: {d.retentionSchedule.enabled ? describeCron(d.retentionSchedule.cron) : 'manual'}</div>
          )}
          {d.bandwidth?.window && <div className="text-ink-muted">Window: {describeWindow(d.bandwidth.window)}</div>}
          {!!d.bandwidth?.uploadKiBps && <div className="text-ink-muted">Upload limit: {formatKiBps(d.bandwidth.uploadKiBps)}</div>}
        </div>
      ),
    },
    {
      // The last sync (design §10): later verify, retention and Plex DB backup jobs must not hide
      // a failed sync or held changes.
      key: 'last',
      header: 'Last sync',
      className: 'whitespace-nowrap',
      cell: (d) => (
        <div>
          {d.lastSync ? (
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
          )}
          <EngineFigures destination={d} />
        </div>
      ),
    },
    {
      key: 'actions',
      header: <span className="sr-only">Actions</span>,
      className: 'whitespace-nowrap text-right',
      cell: (d) => {
        // Only dry runs run while the destination is blocked (S21): the reason is on the button.
        const blocked = d.blockedReason ? `Not available: ${d.blockedReason}` : undefined;
        const restic = d.engine === 'restic';
        return (
          <div className="flex flex-wrap justify-end gap-1">
            {d.pending && kitState(d) !== 'unconfirmed' && (
              <Button small variant="ghost" icon={Play} onClick={() => setDialog({ kind: 'edit', destination: null, resume: d })} aria-label={`Finish creating ${d.name}`}>
                Finish create
              </Button>
            )}
            <Button small variant="ghost" icon={FlaskConical} onClick={() => setDialog({ kind: 'test', destination: d })} aria-label={`Test ${d.name}`}>
              Test
            </Button>
            <Button
              small
              variant="ghost"
              icon={Eye}
              busy={busy === `preview:${d.id}`}
              disabled={d.pending}
              onClick={() => void start(d, 'preview')}
              aria-label={`Preview a sync of ${d.name}`}
            >
              Preview
            </Button>
            <Button
              small
              variant="ghost"
              icon={Play}
              busy={busy === `sync:${d.id}`}
              disabled={!!blocked}
              title={blocked}
              onClick={() => void start(d, 'sync')}
              aria-label={`Sync ${d.name} now`}
            >
              Sync now
            </Button>
            <Button
              small
              variant="ghost"
              icon={ShieldCheck}
              busy={busy === `verify:${d.id}`}
              disabled={!!blocked}
              title={blocked}
              onClick={() => void start(d, 'verify')}
              aria-label={`Verify ${d.name}`}
            >
              Verify
            </Button>
            {restic && (
              <Button
                small
                variant="ghost"
                icon={Eraser}
                busy={busy === `prune:${d.id}`}
                disabled={!!blocked}
                title={blocked ?? 'Run retention now and prune the repository (frees the space of forgotten snapshots)'}
                onClick={() => void start(d, 'prune')}
                aria-label={`Prune ${d.name} now`}
              >
                Prune now
              </Button>
            )}
            {restic && (
              <Button
                small
                variant="ghost"
                icon={Unlock}
                busy={busy === `unlock:${d.id}`}
                disabled={d.pending}
                title="Remove locks left by a restic process that no longer runs"
                onClick={() => void unlock(d)}
                aria-label={`Remove stale locks of ${d.name}`}
              >
                Remove stale locks
              </Button>
            )}
            {restic && <IconButton label={`Remove all locks of ${d.name}`} icon={Lock} onClick={() => setDialog({ kind: 'unlockAll', destination: d })} />}
            {kitState(d) !== 'none' && <IconButton label={`Recovery kit of ${d.name}`} icon={KeyRound} onClick={() => setDialog({ kind: 'kit', destination: d })} />}
            <IconButton label={`Snapshots on ${d.name}`} icon={Archive} onClick={() => setDialog({ kind: 'snapshots', destination: d })} />
            <IconButton label={`Manifests on ${d.name}`} icon={FileText} onClick={() => setDialog({ kind: 'manifests', destination: d })} />
            <IconButton label={`Edit ${d.name}`} icon={Pencil} onClick={() => setDialog({ kind: 'edit', destination: d })} />
            <IconButton label={`Delete ${d.name}`} icon={Trash2} className="hover:text-danger" onClick={() => setDialog({ kind: 'delete', destination: d })} />
          </div>
        );
      },
    },
  ];

  const list = destinations.data;
  // Destinations whose recovery kit custody is not confirmed run no backup (S21): a red banner each.
  const unconfirmed = (list ?? []).filter((d) => kitState(d) === 'unconfirmed');
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
      {unconfirmed.map((d) => (
        <Notice key={d.id} tone="error" title={`Recovery kit not confirmed for ${d.name}`}>
          <p>
            {d.pending
              ? `Creating ${d.name} did not finish: finish it (Finish create, or add a destination at the same location; its encryption password is kept and used), or delete it.`
              : `No backup runs to ${d.name} until you ${d.encryption.origin === 'user' ? 'type its encryption password again or ' : ''}download its recovery kit and type the check code from it. Without the kit, losing this server's /config makes the backup unreadable.`}
          </p>
          <div className="mt-2 flex flex-wrap gap-2">
            {d.pending && (
              <Button small variant="primary" icon={Play} onClick={() => setDialog({ kind: 'edit', destination: null, resume: d })} aria-label={`Finish creating ${d.name}`}>
                Finish create
              </Button>
            )}
            <Button small variant="danger" icon={KeyRound} onClick={() => setDialog({ kind: 'kit', destination: d })}>
              Recovery kit
            </Button>
          </div>
        </Notice>
      ))}
      {list && list.length === 0 ? (
        <EmptyState icon={HardDrive} title="No destinations yet">
          <p>
            A destination is where backups go: a mounted share (for example a UniFi UNAS over NFS or SMB) that receives a verified mirror of your sources, or an
            off-site SFTP server, S3 bucket or Backblaze B2 bucket through restic or rclone, encrypted by default.
          </p>
          <div className="mt-4">
            <Button variant="primary" icon={Plus} onClick={() => setDialog({ kind: 'edit', destination: null })}>
              Add destination
            </Button>
          </div>
        </EmptyState>
      ) : (
        <DataTable columns={columns} rows={list} rowKey={(d) => d.id} loading={destinations.isPending} caption="Destinations" />
      )}

      {dialog?.kind === 'edit' && <DestinationForm destination={dialog.destination} resume={dialog.resume} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'test' && <TestDialog destination={dialog.destination} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'snapshots' && <SnapshotsDialog destination={dialog.destination} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'manifests' && <ManifestsDialog destination={dialog.destination} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'kit' && (
        <RecoveryKitDialog destination={list?.find((d) => d.id === dialog.destination.id) ?? dialog.destination} onClose={() => setDialog(null)} />
      )}
      {dialog?.kind === 'unlockAll' && (
        <ConfirmDialog
          title="Remove all locks"
          confirmLabel="Remove all locks"
          danger
          onClose={() => setDialog(null)}
          onConfirm={async () => {
            await unlockDestination(dialog.destination.id, true);
            setNotice({ tone: 'success', body: `All locks of ${dialog.destination.name} removed.` });
          }}
        >
          <p>
            Remove <strong>every</strong> lock of the restic repository of {dialog.destination.name}, including locks of processes that may still run?
          </p>
          <p className="text-ink-muted">
            Only do this when no other computer uses this repository right now: removing the lock of a running backup or prune elsewhere can damage the
            repository. Bunkarr refuses while a job of this destination runs or waits for its window. &quot;Remove stale locks&quot; is the safe choice.
          </p>
        </ConfirmDialog>
      )}
      {dialog?.kind === 'delete' && <DeleteDialog destination={dialog.destination} onClose={() => setDialog(null)} />}
    </Page>
  );
}

/** DestinationSummary is the card part of a row: name, kind, engine, encryption, kit, location, waiting state. */
function DestinationSummary({ destination: d }: { destination: Destination }) {
  const engine = isEngine(d);
  const kit = kitState(d);
  return (
    <div className="min-w-[12rem]">
      <span className="font-medium">{d.name}</span>
      {!d.enabled && (
        <span className="ml-2">
          <Badge>Disabled</Badge>
        </span>
      )}
      <div className="break-all font-mono text-xs text-ink-muted">{d.target}</div>
      <div className="mt-1 flex flex-wrap gap-1">
        {(engine || d.kind !== 'local') && <Badge tone="info">{KIND_BADGES[d.kind] ?? d.kind}</Badge>}
        {engine && (
          <Badge tone="info" title={d.engineVersion || undefined}>
            {ENGINE_BADGES[d.engine] ?? d.engine}
          </Badge>
        )}
        {engine || d.kind !== 'local' ? (
          isEncrypted(d) ? (
            <Badge tone="ok" title={d.encryption.mode === 'crypt' ? 'rclone crypt' : 'restic repository encryption'}>
              Encrypted
            </Badge>
          ) : (
            <Badge tone="danger" title="The storage provider can read every file">
              Not encrypted
            </Badge>
          )
        ) : (
          <Badge title="Plain files on your share">Not encrypted</Badge>
        )}
        {d.pending && <Badge tone="danger">Create did not finish</Badge>}
        {!engine && d.fsType && <Badge>{d.fsType}</Badge>}
        {!engine && <CapabilityBadges caps={d.capabilities} />}
      </div>
      {kit === 'confirmed' && d.encryption.kitConfirmedAt && (
        <div className="mt-1 text-xs text-accent">Recovery kit confirmed on {formatDateTime(d.encryption.kitConfirmedAt)}</div>
      )}
      {kit === 'unconfirmed' && <div className="mt-1 text-xs font-medium text-danger">Recovery kit not confirmed: backups do not run</div>}
      {d.blockedReason && kit !== 'unconfirmed' && <div className="mt-1 text-xs text-warn">{d.blockedReason}</div>}
      {d.waitingUntil && <div className="mt-1 text-xs text-info">Waiting for the window until {formatClock(d.waitingUntil)}</div>}
    </div>
  );
}

/** EngineFigures are a restic or rclone destination's snapshot count and repository size (engine_state). */
function EngineFigures({ destination: d }: { destination: Destination }) {
  if (!isEngine(d)) return null;
  const count = snapshotCount(d.engineState);
  const size = repositoryBytes(d.engineState);
  if (count == null && size == null) return null;
  return (
    <div className="mt-1 text-xs text-ink-muted">
      {count != null && <div>{d.engine === 'restic' ? `${formatNumber(count)} ${count === 1 ? 'snapshot' : 'snapshots'}` : `${formatNumber(count)} versions`}</div>}
      {size != null && <div title="Last known size of the repository">{formatBytes(size)} stored</div>}
    </div>
  );
}

/**
 * DeleteDialog forgets a destination. When its recovery kit custody was never confirmed, the
 * encryption secret is deleted with it (S21): the user must say they understand (confirmLoseSecret).
 */
function DeleteDialog({ destination, onClose }: { destination: Destination; onClose: () => void }) {
  const qc = useQueryClient();
  const loseSecret = kitState(destination) === 'unconfirmed';
  const [understood, setUnderstood] = useState(false);
  return (
    <ConfirmDialog
      title="Delete destination"
      confirmLabel="Delete"
      danger
      onClose={onClose}
      onConfirm={async () => {
        if (loseSecret && !understood) {
          throw new Error('Tick the box: the encryption password is deleted with the destination.');
        }
        await deleteDestination(destination.id, { confirmLoseSecret: loseSecret });
        await qc.invalidateQueries({ queryKey: keys.destinations });
        await qc.invalidateQueries({ queryKey: keys.schedules });
      }}
    >
      <p>
        Delete <strong>{destination.name}</strong> from Bunkarr?
      </p>
      <p className="text-ink-muted">
        Its schedules and records are removed. The backup data at <code className="break-all">{destination.target}</code> is not touched; you can attach to it
        again later.
      </p>
      {loseSecret && (
        <div className="rounded border border-danger/50 bg-danger/10 p-2">
          <p className="mb-2 text-danger">
            The recovery kit of {destination.name} was never confirmed. Deleting it also deletes its encryption password: without a kit you saved, the backup
            there can never be read again.
          </p>
          <Checkbox label="I understand: delete the encryption password too" checked={understood} onChange={setUnderstood} />
        </div>
      )}
    </ConfirmDialog>
  );
}

function TestDialog({ destination, onClose }: { destination: Destination; onClose: () => void }) {
  const qc = useQueryClient();
  const engine = isEngine(destination);
  const test = useQuery<{ filecopy?: DestinationTestResult; engine?: EngineTestResult }>({
    queryKey: ['destination', destination.id, 'test'],
    queryFn: async () => {
      // A restic or rclone destination is tested with its stored location and secrets only.
      const r = engine ? { engine: await testStoredDestination(destination.id) } : { filecopy: await testDestination(destination.id) };
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
      {test.isPending ? (
        <p className="text-sm text-ink-muted">Probing {destination.target}…</p>
      ) : (
        <>
          {test.data?.filecopy && <TestResultView result={test.data.filecopy} />}
          {test.data?.engine && <EngineTestView result={test.data.engine} engine={destination.engine as 'restic' | 'rclone'} />}
        </>
      )}
    </Modal>
  );
}

function SnapshotsDialog({ destination, onClose }: { destination: Destination; onClose: () => void }) {
  const snapshots = useQuery({ queryKey: ['destination', destination.id, 'snapshots'], queryFn: () => listSnapshots(destination.id) });
  const integrations = useIntegrations();
  const sources = useSources();
  // integrationId is 0 once the Plex server or *arr was deleted from Bunkarr (the backup stays).
  const serverName = (id: number) => (id > 0 ? (integrations.data?.find((i) => i.id === id)?.name ?? `integration #${id}`) : 'Deleted server');
  const sourceName = (id: number | undefined) => (id ? (sources.data?.find((x) => x.id === id)?.name ?? `Source #${id}`) : 'Unknown source');
  const all = snapshots.data;
  const media = all?.filter((s) => s.kind === 'media') ?? [];
  const versions = all?.filter((s) => s.kind !== 'media');
  const engine = isEngine(destination);
  const columns: Column<Snapshot>[] = [
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
          <code className="break-all text-xs">{s.path || (s.engineRef ? `snapshot ${shortId(s.engineRef)}` : '')}</code>
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
  // Media snapshots of a restic destination, one table per source (phase4.md §15).
  const bySource = new Map<number, Snapshot[]>();
  for (const s of media) {
    const id = s.sourceId ?? 0;
    bySource.set(id, [...(bySource.get(id) ?? []), s]);
  }
  const mediaColumns: Column<Snapshot>[] = [
    { key: 'time', header: 'Time', className: 'whitespace-nowrap', cell: (s) => <span title={formatRelative(s.createdAt)}>{formatDateTime(s.createdAt)}</span> },
    { key: 'batch', header: 'Batch', className: 'text-right', cell: (s) => (s.batch ? formatNumber(s.batch) : '—') },
    {
      key: 'complete',
      header: 'Complete',
      cell: (s) =>
        s.complete ? (
          <Badge tone="ok">Complete</Badge>
        ) : (
          <Badge tone="warn" title="A batch of a sync that holds part of the source; the complete snapshot follows when the sync finishes">
            Partial
          </Badge>
        ),
    },
    { key: 'files', header: 'Files', className: 'whitespace-nowrap text-right', cell: (s) => formatNumber(s.files) },
    { key: 'added', header: 'Data added', className: 'whitespace-nowrap text-right', cell: (s) => formatBytes(s.dataAdded ?? 0) },
    { key: 'id', header: 'Snapshot', cell: (s) => <code className="text-xs">{s.engineRef ? shortId(s.engineRef) : '—'}</code> },
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
      <ErrorNotice error={snapshots.error} />
      {destination.engine === 'restic' && (
        <section aria-label="Media snapshots" className="mb-6">
          <h3 className="mb-1 text-sm font-semibold">Media snapshots</h3>
          <p className="mb-3 text-xs text-ink-muted">
            Each sync adds snapshots per source (a long first backup one per batch). Retention forgets the ones no longer needed; a snapshot that holds the last
            version of a deleted or replaced file is kept.
          </p>
          {snapshots.isPending ? (
            <p className="text-sm text-ink-muted">Loading…</p>
          ) : bySource.size === 0 ? (
            <p className="text-sm text-ink-muted">No media snapshots yet: they appear after the first sync.</p>
          ) : (
            [...bySource.entries()].map(([id, rows]) => (
              <div key={id} className="mb-4">
                <h4 className="mb-1 text-sm font-medium">{sourceName(id)}</h4>
                <DataTable columns={mediaColumns} rows={rows} rowKey={(s) => s.engineRef ?? `${s.createdAt}-${s.batch}`} caption={`Snapshots of ${sourceName(id)}`} />
              </div>
            ))
          )}
        </section>
      )}
      {engine && <h3 className="mb-1 text-sm font-semibold">Plex DB and *arr versions</h3>}
      <p className="mb-3 text-xs text-ink-muted">
        Verified copies of the Plex database, blobs database and Preferences.xml (under <code>.bunkarr/plex/</code>), and of the *arr apps&apos; backup zips
        (under <code>.bunkarr/arr/</code>). Preferences.xml contains the server&apos;s Plex token and an *arr zip its API key and passwords: protect the
        {engine ? ' recovery kit' : ' share'} accordingly. Neither is offered for download.
      </p>
      <DataTable columns={columns} rows={versions} rowKey={(s) => s.id} loading={snapshots.isPending} empty="No Plex database or *arr backups on this destination yet." caption="Snapshots" />
    </Modal>
  );
}
