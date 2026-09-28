import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { formatClock } from '@/lib/bandwidth';
import { GiB, job, MiB, paged, resticDestination, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { deferralText, engineProgressParts, isWaiting, splitStats, waitingText } from './JobParts';

// Engine jobs in Activity (docs/design/phase4.md §9.2, §10.4, §11.4, §15): the waiting state of a
// deferred job, the batch counter, the limit in force and the window's end, and engine stats.

const names = {
  'GET /api/v1/destinations': () => ({ body: [resticDestination({ name: 'B2' })] }),
  'GET /api/v1/sources': () => ({ body: [source(), source({ id: 2, name: 'TV' })] }),
  'GET /api/v1/integrations': () => ({ body: [] }),
};

describe('Queue', () => {
  it('shows a deferred job as waiting for its window, and an engine job’s batch, limit and window end', async () => {
    const resumes = new Date(Date.now() + 5 * 3600_000).toISOString();
    const ends = new Date(Date.now() + 2 * 3600_000).toISOString();
    const waiting = job({ id: 21, status: 'queued', startedAt: null, params: { destinationId: 9 }, notBefore: resumes, deferrals: 2, progress: { ...job().progress, filesDone: 0 } });
    const running = job({
      id: 22,
      type: 'sync',
      params: { destinationId: 9 },
      progress: { ...job().progress, phase: 'backing-up', batch: 2, batches: 5, limitBytesPerSec: 2 * MiB, windowEndsAt: ends },
    });
    renderApp('/activity/queue', { ...names, 'GET /api/v1/jobs': () => ({ body: paged([waiting, running]) }) });
    const table = await screen.findByRole('table', { name: 'Active jobs' });
    const links = await within(table).findAllByRole('link', { name: 'Sync · B2' });
    const w = within(links[0].closest('tr')!);
    expect(w.getByText(`Waiting for the transfer window, resumes at ${formatClock(resumes)} (deferred 2 times)`)).toBeInTheDocument();
    expect(w.getByText('Waiting for window')).toBeInTheDocument();
    expect(w.getByText(`until ${formatClock(resumes)}`)).toBeInTheDocument();
    const r = within(links[1].closest('tr')!);
    expect(r.getByText(`Batch 2 of 5 · Limit 2.0 MiB/s · Window ends ${formatClock(ends)}`)).toBeInTheDocument();
    expect(r.queryByText('Waiting for window')).not.toBeInTheDocument();
  });
});

describe('Job page', () => {
  it('shows a restic sync’s snapshots, uploads, limit and waits', async () => {
    const done = job({
      id: 30,
      status: 'completed',
      params: { destinationId: 9 },
      finishedAt: new Date().toISOString(),
      deferrals: 1,
      summary: 'Backed up 1,234 files (12.3 GiB new, 2 batches) to B2; snapshot 1a2b3c4d',
      stats: {
        dryRun: false,
        filesPlanned: 1234,
        filesCopied: 1234,
        engine: 'restic',
        batches: 2,
        bytesUploaded: 12 * GiB,
        bytesRead: 40 * GiB,
        deferrals: 1,
        limitKiBps: 2048,
        unchanged: false,
        snapshots: [
          { sourceId: 1, snapshotId: '1a2b3c4d5e6f', batch: 1, filesNew: 1000, filesChanged: 0, filesUnmodified: 0, dataAdded: 10 * GiB },
          { sourceId: 2, snapshotId: '9f8e7d6c5b4a', batch: 2, filesNew: 234, filesChanged: 3, filesUnmodified: 7, dataAdded: 2 * GiB },
        ],
        sources: [],
      },
    });
    renderApp('/activity/jobs/30', {
      ...names,
      'GET /api/v1/jobs/30': () => ({ body: done }),
      'GET /api/v1/jobs/30/items/summary': () => ({ body: [] }),
      'GET /api/v1/jobs/30/items': () => ({ body: paged([]) }),
      'GET /api/v1/jobs/30/logs': () => ({ body: [] }),
    });
    const grid = within(await screen.findByRole('group', { name: 'Statistics' }));
    expect(grid.getByText('Uploaded')).toBeInTheDocument();
    expect(grid.getByText('12.0 GiB')).toBeInTheDocument();
    expect(grid.getByText('Upload limit')).toBeInTheDocument();
    expect(grid.getByText('2.0 MiB/s')).toBeInTheDocument();
    expect(grid.getByText('Waited for the window')).toBeInTheDocument();
    expect(grid.getByText('once')).toBeInTheDocument();
    const snaps = within(screen.getByRole('table', { name: 'Snapshots made' }));
    expect(snaps.getByText('1a2b3c4d')).toBeInTheDocument();
    expect(snaps.getByRole('rowheader', { name: 'TV' })).toBeInTheDocument();
    expect(screen.getByText('Waited for the transfer window: deferred once')).toBeInTheDocument();
  });

  it('shows a restic verify’s repository check and a deferred job’s start', async () => {
    const verify = job({ id: 31, type: 'verify', status: 'completed_with_warnings', params: { destinationId: 9, readData: true }, stats: { engine: 'restic', check: { numErrors: 2, readSubset: 'all' }, sampleFiles: 10, sampleBytes: GiB } });
    renderApp('/activity/jobs/31', {
      ...names,
      'GET /api/v1/jobs/31': () => ({ body: verify }),
      'GET /api/v1/jobs/31/items/summary': () => ({ body: [] }),
      'GET /api/v1/jobs/31/items': () => ({ body: paged([]) }),
      'GET /api/v1/jobs/31/logs': () => ({ body: [] }),
    });
    expect(await screen.findByText('Repository check: All data read: 2 errors')).toHaveAttribute('role', 'alert');
    expect(screen.getByText('Read all data')).toBeInTheDocument();
  });
});

describe('Retention preview', () => {
  it('labels a restic forget preview’s counts as what would happen and hides what it never does', async () => {
    const preview = job({
      id: 32,
      type: 'retention',
      status: 'completed',
      dryRun: true,
      params: { destinationId: 9 },
      summary: 'Retention preview of B2; would forget 28 snapshots',
      stats: { dryRun: true, engine: 'restic', snapshotsForgotten: 28, snapshotsKept: 12, forgetRequests: 0, pruned: false, pruneDurationMs: 0, cleanup: false, bytesUploaded: 0, batches: 0 },
    });
    renderApp('/activity/jobs/32', {
      ...names,
      'GET /api/v1/jobs/32': () => ({ body: preview }),
      'GET /api/v1/jobs/32/items/summary': () => ({ body: [] }),
      'GET /api/v1/jobs/32/items': () => ({ body: paged([]) }),
      'GET /api/v1/jobs/32/logs': () => ({ body: [] }),
    });
    const grid = within(await screen.findByRole('group', { name: 'Statistics' }));
    expect(grid.getByText('Snapshots to forget')).toBeInTheDocument();
    expect(grid.getByText('Snapshots to keep')).toBeInTheDocument();
    expect(grid.queryByText('Snapshots forgotten')).not.toBeInTheDocument();
    expect(grid.queryByText('Snapshots kept')).not.toBeInTheDocument();
    for (const never of ['Forget requests', 'Pruned', 'Prune took', 'Unfinished uploads cleaned', 'Uploaded', 'Batches']) {
      expect(grid.queryByText(never)).not.toBeInTheDocument();
    }
  });
});

describe('Sync preview', () => {
  it('hides what a restic sync preview never settles: nothing changed and waits for the window', async () => {
    const preview = job({
      id: 33,
      status: 'completed',
      dryRun: true,
      params: { destinationId: 9 },
      summary: 'Dry run: nothing to back up to B2',
      stats: { dryRun: true, engine: 'restic', filesPlanned: 0, filesCopied: 0, unchanged: false, deferrals: 0, limitKiBps: 0, snapshots: [], sources: [] },
    });
    renderApp('/activity/jobs/33', {
      ...names,
      'GET /api/v1/jobs/33': () => ({ body: preview }),
      'GET /api/v1/jobs/33/items/summary': () => ({ body: [] }),
      'GET /api/v1/jobs/33/items': () => ({ body: paged([]) }),
      'GET /api/v1/jobs/33/logs': () => ({ body: [] }),
    });
    const grid = within(await screen.findByRole('group', { name: 'Statistics' }));
    expect(grid.getByText('To copy')).toBeInTheDocument();
    expect(grid.queryByText('Nothing changed')).not.toBeInTheDocument();
    expect(grid.queryByText('Waited for the window')).not.toBeInTheDocument();
  });

  it('keeps unchanged only where a job settles it', () => {
    const keys = (stats: Record<string, unknown>, dryRun = false) => splitStats(stats, dryRun).figures.map(([k]) => k);
    // A real restic sync settles it; an rclone sync never sets it.
    expect(keys({ engine: 'restic', unchanged: true })).toContain('unchanged');
    expect(keys({ engine: 'rclone', unchanged: false })).not.toContain('unchanged');
    expect(keys({ engine: 'rclone', unchanged: false, deferrals: 0 }, true)).toEqual(['engine']);
    // The *arr backup and manifest previews do check the destination (arrbackup runner dryRun).
    expect(keys({ dryRun: true, unchanged: true, backupName: 'nzbdrone_backup.zip' }, true)).toEqual(['unchanged', 'backupName']);
  });
});

describe('waiting helpers', () => {
  it('tells a deferred job from a queued one', () => {
    const now = Date.parse('2026-09-27T20:00:00Z');
    expect(isWaiting({ status: 'queued', notBefore: '2026-09-28T01:00:00Z' }, now)).toBe(true);
    expect(isWaiting({ status: 'queued', notBefore: '2026-09-27T19:00:00Z' }, now)).toBe(false);
    expect(isWaiting({ status: 'running', notBefore: '2026-09-28T01:00:00Z' }, now)).toBe(false);
    expect(isWaiting({ status: 'queued', notBefore: null }, now)).toBe(false);
    expect(deferralText(0)).toBe('');
    expect(deferralText(14)).toBe('deferred 14 times');
    expect(waitingText({ notBefore: null, deferrals: 0 })).toBe('Waiting for the transfer window, resumes at —');
    expect(engineProgressParts({ filesTotal: 0, filesDone: 0, bytesTotal: 0, bytesDone: 0, bytesPerSec: 0, etaSeconds: 0 })).toEqual([]);
  });
});
