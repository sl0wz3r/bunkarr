import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { Snapshot } from '@/api/types';
import { callsTo, type Handler } from '@/test/fetch';
import { GiB, job, rcloneDestination, resticDestination, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { slowPage } from '@/test/slow';
import { formatClock, QUEUED_POLLS } from '@/lib/bandwidth';

// The destination list of restic and rclone destinations (docs/design/phase4.md §15): kind, engine
// and encryption badges, the recovery kit state with its red banner and the actions it disables,
// the waiting state, the repository figures, restic's prune and locks, the delete that would lose
// the secret, the stored test and the media snapshots.

slowPage();

const confirmed = { mode: 'restic' as const, origin: 'generated' as const, kitExportedAt: '2026-09-20T10:00:00Z', kitConfirmedAt: '2026-09-20T10:05:00Z' };

function base(list: unknown[], extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/destinations': () => ({ body: list }),
    'GET /api/v1/sources': () => ({ body: [source(), source({ id: 2, name: 'TV', path: '/media/tv', destFolder: 'tv' })] }),
    'GET /api/v1/integrations': () => ({ body: [] }),
    ...extra,
  };
}

const rowOf = async (name: string) => within((await within(await screen.findByRole('table', { name: 'Destinations' })).findByText(name)).closest('tr')!);

describe('Destination cards', () => {
  it('blocks backups with a red banner until the recovery kit is confirmed, and asks before losing the secret', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base([resticDestination()], {
        'POST /api/v1/destinations/9/unlock': () => ({ status: 204 }),
        'DELETE /api/v1/destinations/9': () => ({ status: 204 }),
        'POST /api/v1/destinations/9/sync': () => ({ status: 202, body: job({ id: 40, dryRun: true, status: 'queued', params: { destinationId: 9 } }) }),
        'GET /api/v1/jobs/40': () => ({ body: job({ id: 40, dryRun: true, status: 'queued', params: { destinationId: 9 } }) }),
        'GET /api/v1/jobs/40/items/summary': () => ({ body: [] }),
        'GET /api/v1/jobs/40/items': () => ({ body: { page: 1, pageSize: 50, totalRecords: 0, records: [] } }),
        'GET /api/v1/jobs/40/logs': () => ({ body: [] }),
      }),
    );
    const banner = await screen.findByText('Recovery kit not confirmed for Offsite');
    expect(banner.closest('[role="alert"]')).not.toBeNull();
    expect(screen.getByText(/No backup runs to Offsite until you download its recovery kit/)).toBeInTheDocument();
    const row = await rowOf('Offsite');
    expect(row.getByText('SFTP')).toBeInTheDocument();
    expect(row.getByText('restic')).toBeInTheDocument();
    expect(row.getByText('Encrypted')).toBeInTheDocument();
    expect(row.getByText('Recovery kit not confirmed: backups do not run')).toBeInTheDocument();
    expect(row.getByText('sftp://bunkarr@backup.example.net:22/srv/bunkarr')).toHaveClass('break-all');
    expect(row.getByText('Retention: Daily at 04:30')).toBeInTheDocument();
    const sync = row.getByRole('button', { name: 'Sync Offsite now' });
    expect(sync).toBeDisabled();
    expect(sync).toHaveAttribute('title', 'Not available: export and confirm the recovery kit first');
    expect(row.getByRole('button', { name: 'Verify Offsite' })).toBeDisabled();
    expect(row.getByRole('button', { name: 'Prune Offsite now' })).toBeDisabled();

    await user.click(row.getByRole('button', { name: 'Remove stale locks of Offsite' }));
    expect(await screen.findByText('Stale locks of Offsite removed.')).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/9/unlock')[0].body).toEqual({ removeAll: false });

    await user.click(row.getByRole('button', { name: 'Remove all locks of Offsite' }));
    const confirm = within(await screen.findByRole('dialog', { name: 'Remove all locks' }));
    expect(confirm.getByText(/can damage the repository/)).toBeInTheDocument();
    await user.click(confirm.getByRole('button', { name: 'Remove all locks' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations/9/unlock')).toHaveLength(2));
    expect(callsTo(calls, 'POST /api/v1/destinations/9/unlock')[1].body).toEqual({ removeAll: true });

    await user.click(screen.getByRole('button', { name: 'Recovery kit' }));
    const kit = within(await screen.findByRole('dialog', { name: 'Recovery kit · Offsite' }));
    expect(kit.getByRole('button', { name: 'Download recovery kit' })).toBeInTheDocument();
    await user.click(kit.getByRole('button', { name: 'Done' }));

    await user.click(row.getByRole('button', { name: 'Delete Offsite' }));
    const del = within(await screen.findByRole('dialog', { name: 'Delete destination' }));
    expect(del.getByText(/also deletes its encryption password/)).toBeInTheDocument();
    await user.click(del.getByRole('button', { name: 'Delete' }));
    expect(await del.findByText(/Tick the box/)).toBeInTheDocument();
    expect(callsTo(calls, 'DELETE /api/v1/destinations/9')).toHaveLength(0);
    await user.click(del.getByRole('checkbox', { name: /delete the encryption password too/ }));
    await user.click(del.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(callsTo(calls, 'DELETE /api/v1/destinations/9')).toHaveLength(1));
    expect(callsTo(calls, 'DELETE /api/v1/destinations/9')[0].query.get('confirmLoseSecret')).toBe('true');

    // A preview (dry run) still runs while the kit blocks real jobs.
    await user.click(row.getByRole('button', { name: 'Preview a sync of Offsite' }));
    expect(await screen.findByRole('heading', { name: 'Sync #40' })).toBeInTheDocument();
  });

  it('points to the check code when a crypt remote with a password2 is confirmed by its password', async () => {
    const crypt = rcloneDestination({
      encryption: { mode: 'crypt', origin: 'user', kitExportedAt: null, kitConfirmedAt: null },
      blockedReason: 'export and confirm the recovery kit first',
    });
    const { calls, user } = renderApp(
      '/destinations',
      base([crypt], {
        'POST /api/v1/destinations/10/recovery-kit/confirm': () => ({
          status: 400,
          body: { message: 'this crypt remote has a password2 as well: confirm with the check code of its recovery kit, which holds both' },
        }),
      }),
    );
    expect(await screen.findByText(/type its encryption password again \(unless it also has a crypt password2\) or download/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Recovery kit' }));
    const kit = within(await screen.findByRole('dialog', { name: 'Recovery kit · Wasabi' }));
    const secret = 'my own rclone crypt password';
    await user.type(kit.getByLabelText('Encryption password'), secret);
    await user.click(kit.getByRole('button', { name: 'Confirm password' }));
    expect(await kit.findByText('This crypt remote has a password2 as well: confirm with the check code of its recovery kit, which holds both.')).toBeInTheDocument();
    expect(kit.queryByText(/That is not the encryption password/)).not.toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/10/recovery-kit/confirm')[0].body).toEqual({ secret });
  });

  it('shows the confirmed kit, the waiting state and the repository figures, and prunes now', async () => {
    const until = new Date(Date.now() + 3 * 3600_000).toISOString();
    const restic = resticDestination({
      encryption: confirmed,
      blockedReason: '',
      waitingUntil: until,
      engineState: {
        destinationId: 9,
        engineVersion: 'restic 0.18.1',
        lastPruneAt: null,
        lastCheckAt: null,
        readSubsetNext: 1,
        lastCleanupAt: null,
        throughputBps: null,
        stats: { snapshotCount: 12, repositoryBytes: 1536 * GiB },
        updatedAt: '2026-09-27T01:00:00Z',
      },
      bandwidth: { uploadKiBps: 2048, downloadKiBps: 0, timetable: [], window: { days: ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'], from: '01:00', to: '07:00', graceMinutes: 15, allowOverrun: false } },
    });
    const { calls, user } = renderApp(
      '/destinations',
      base([restic, rcloneDestination({ encryption: { mode: 'none', origin: '', kitExportedAt: null, kitConfirmedAt: null } })], {
        'POST /api/v1/destinations/9/retention': () => ({ status: 202, body: job({ id: 41, type: 'retention', status: 'queued', params: { destinationId: 9, prune: true }, notBefore: until }) }),
      }),
    );
    const row = await rowOf('Offsite');
    expect(row.getByText(/Recovery kit confirmed on/)).toBeInTheDocument();
    expect(row.getByText(`Waiting for the window until ${formatClock(until)}`)).toBeInTheDocument();
    expect(row.getByText('12 snapshots')).toBeInTheDocument();
    expect(row.getByText('1.5 TiB stored')).toBeInTheDocument();
    expect(row.getByText('Window: Every day 01:00–07:00')).toBeInTheDocument();
    expect(row.getByText('Upload limit: 2.0 MiB/s')).toBeInTheDocument();
    expect(row.getByRole('button', { name: 'Sync Offsite now' })).toBeEnabled();
    expect(screen.queryByText(/Recovery kit not confirmed for/)).not.toBeInTheDocument();

    const wasabi = await rowOf('Wasabi');
    expect(wasabi.getByText('Not encrypted')).toBeInTheDocument();
    expect(wasabi.getByText('rclone')).toBeInTheDocument();
    expect(wasabi.queryByRole('button', { name: /Prune/ })).not.toBeInTheDocument();
    expect(wasabi.queryByRole('button', { name: /Recovery kit/ })).not.toBeInTheDocument();

    await user.click(row.getByRole('button', { name: 'Prune Offsite now' }));
    expect(await screen.findByText(/Retention with a prune of Offsite queued: it waits for the transfer window/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/9/retention')[0].body).toEqual({ prune: true });
  });

  it('says when a manual sync outside the transfer window starts, once the queued job defers', async () => {
    const until = new Date(Date.now() + 6 * 3600_000).toISOString();
    const restic = resticDestination({
      encryption: confirmed,
      blockedReason: '',
      bandwidth: { uploadKiBps: 0, downloadKiBps: 0, timetable: [], window: { days: ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'], from: '01:00', to: '07:00', graceMinutes: 15, allowOverrun: false } },
    });
    let reads = 0;
    const queued = job({ id: 44, type: 'sync', status: 'queued', startedAt: null, params: { destinationId: 9 }, notBefore: null });
    const { calls, user } = renderApp(
      '/destinations',
      base([restic], {
        // The queue answers before the runner defers the job: no notBefore yet.
        'POST /api/v1/destinations/9/sync': () => ({ status: 202, body: queued }),
        'GET /api/v1/jobs/44': () => ({ body: ++reads < 2 ? queued : { ...queued, notBefore: until, deferrals: 1 } }),
      }),
    );
    const row = await rowOf('Offsite');
    await user.click(row.getByRole('button', { name: 'Sync Offsite now' }));
    expect(await screen.findByText(/Sync of Offsite queued\./)).toBeInTheDocument();
    expect(await screen.findByText(`Sync of Offsite queued: it waits for the transfer window and starts at ${formatClock(until)}.`, { exact: false }, { timeout: 4000 })).toBeInTheDocument();
    // The list is read again after the job deferred, for the card's waiting time.
    await waitFor(() => {
      const deferredAt = calls.findIndex((c, i) => c.key === 'GET /api/v1/jobs/44' && calls.slice(0, i).filter((x) => x.key === 'GET /api/v1/jobs/44').length === 1);
      expect(deferredAt).toBeGreaterThan(-1);
      expect(calls.slice(deferredAt + 1).some((c) => c.path === '/api/v1/destinations' && c.method === 'GET')).toBe(true);
    });
  });

  it('still says when a sync starts when a busy queue picks the job up only after the first reads', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const until = new Date(Date.now() + 13 * 3600_000).toISOString();
      const restic = resticDestination({
        encryption: confirmed,
        blockedReason: '',
        bandwidth: { uploadKiBps: 0, downloadKiBps: 0, timetable: [], window: { days: ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'], from: '01:00', to: '07:00', graceMinutes: 15, allowOverrun: false } },
      });
      let reads = 0;
      const queued = job({ id: 45, type: 'sync', status: 'queued', startedAt: null, params: { destinationId: 9 }, notBefore: null });
      const { user } = renderApp(
        '/destinations',
        base([restic], {
          'POST /api/v1/destinations/9/sync': () => ({ status: 202, body: queued }),
          // Both workers are busy: the runner defers the job only after the fast reads are spent.
          'GET /api/v1/jobs/45': () => ({ body: ++reads <= QUEUED_POLLS + 1 ? queued : { ...queued, notBefore: until, deferrals: 1 } }),
        }),
      );
      const row = await rowOf('Offsite');
      await user.click(row.getByRole('button', { name: 'Sync Offsite now' }));
      expect(await screen.findByText(/Sync of Offsite queued\./)).toBeInTheDocument();
      await vi.advanceTimersByTimeAsync((QUEUED_POLLS + 1) * 1000);
      expect(reads).toBeGreaterThanOrEqual(QUEUED_POLLS);
      expect(screen.queryByText(/it waits for the transfer window/)).not.toBeInTheDocument();
      await vi.advanceTimersByTimeAsync(25_000);
      expect(await screen.findByText(`Sync of Offsite queued: it waits for the transfer window and starts at ${formatClock(until)}.`, { exact: false })).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('tests an engine destination with its stored secrets and lists its media snapshots per source', async () => {
    const restic = resticDestination({ encryption: confirmed, blockedReason: '' });
    const snap = (over: Partial<Snapshot>): Snapshot => ({
      id: 0,
      destinationId: 9,
      kind: 'media',
      integrationId: 0,
      jobId: 30,
      path: '',
      createdAt: '2026-09-26T02:10:00Z',
      size: 0,
      method: '',
      integrity: 'ok',
      manifest: null,
      sourceId: 1,
      batch: 1,
      complete: true,
      files: 1200,
      dataAdded: 12 * GiB,
      engineRef: '1a2b3c4d5e6f7a8b',
      ...over,
    });
    const { calls, user } = renderApp(
      '/destinations',
      base([restic], {
        'POST /api/v1/destinations/9/test': () => ({ body: { ok: true, reachable: true, repository: 'exists', id: '4f2a9c', entries: 5, freeBytes: null, engineVersion: 'restic 0.18.1', message: 'The repository matches.' } }),
        'GET /api/v1/destinations/9/snapshots': () => ({
          body: [
            snap({}),
            snap({ sourceId: 2, batch: 2, complete: false, files: 20000, engineRef: '9f8e7d6c5b4a3f2e', dataAdded: 64 * GiB }),
            {
              id: 5,
              destinationId: 9,
              kind: 'plexdb',
              integrationId: 0,
              jobId: 3,
              path: '',
              engineRef: 'aabbccdd11223344',
              createdAt: '2026-09-26T06:00:00Z',
              size: 5 * GiB,
              method: 'backup-api',
              integrity: 'ok',
              manifest: {},
            },
          ],
        }),
      }),
    );
    const row = await rowOf('Offsite');
    await user.click(row.getByRole('button', { name: 'Test Offsite' }));
    const test = within(await screen.findByRole('dialog', { name: 'Test · Offsite' }));
    expect(await test.findByText('Repository OK')).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/9/test')[0].body).toBeUndefined();
    await user.click(test.getByRole('button', { name: 'Done' }));

    await user.click(row.getByRole('button', { name: 'Snapshots on Offsite' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Snapshots · Offsite' }));
    const movies = within(await dialog.findByRole('table', { name: 'Snapshots of Movies' }));
    expect(movies.getByText('1a2b3c4d')).toBeInTheDocument();
    expect(movies.getByRole('cell', { name: 'Complete' })).toBeInTheDocument();
    expect(movies.getByText('1,200')).toBeInTheDocument();
    expect(movies.getByText('12.0 GiB')).toBeInTheDocument();
    const tv = within(dialog.getByRole('table', { name: 'Snapshots of TV' }));
    expect(tv.getByRole('cell', { name: 'Partial' })).toBeInTheDocument();
    expect(tv.getByRole('cell', { name: '2' })).toBeInTheDocument();
    const versions = within(dialog.getByRole('table', { name: 'Snapshots' }));
    expect(versions.getByText('Plex DB')).toBeInTheDocument();
    expect(versions.getByText('snapshot aabbccdd')).toBeInTheDocument();
  });
});
