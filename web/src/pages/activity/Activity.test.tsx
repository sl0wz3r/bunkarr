import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ItemCount, JobItem } from '@/api/types';
import { type Call, callsTo, type Handler } from '@/test/fetch';
import { destination, GiB, job, MiB, paged } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { LOG_PAGE, MAX_LOG_LINES } from './JobLogs';

const names = {
  'GET /api/v1/destinations': () => ({ body: [destination()] }),
  'GET /api/v1/sources': () => ({ body: [] }),
  'GET /api/v1/integrations': () => ({ body: [] }),
};

describe('Queue', () => {
  it('renders progress, throughput and ETA, and cancels a job', async () => {
    let cancelled = false;
    const { calls, user } = renderApp('/activity/queue', {
      ...names,
      'GET /api/v1/jobs': () => ({ body: paged(cancelled ? [] : [job()], cancelled ? 0 : 1) }),
      'POST /api/v1/jobs/7/cancel': () => {
        cancelled = true;
        return { status: 202, body: job({ status: 'cancelled' }) };
      },
    });

    const bar = await screen.findByRole('progressbar', { name: 'Progress of Sync · UNAS' });
    expect(bar).toHaveAttribute('aria-valuenow', '25');
    expect(screen.getByText('50 of 200 files · 1.0 GiB of 4.0 GiB')).toBeInTheDocument();
    expect(screen.getByText('50.0 MiB/s')).toBeInTheDocument();
    expect(screen.getByText('1h 2m')).toBeInTheDocument();
    expect(screen.getByText('Movies/Heat (1995)/Heat.mkv')).toBeInTheDocument();
    expect(screen.getByText('copying')).toBeInTheDocument();
    expect(calls.some((c) => c.key === 'GET /api/v1/jobs?state=active&page=1&pageSize=100')).toBe(true);

    await user.click(screen.getByRole('button', { name: 'Cancel job #7' }));
    const dialog = await screen.findByRole('dialog', { name: 'Cancel job' });
    expect(callsTo(calls, 'POST /api/v1/jobs/7/cancel')).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Cancel job' }));

    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/jobs/7/cancel')).toHaveLength(1));
    expect(await screen.findByText('Nothing running')).toBeInTheDocument();
  });

  it('shows a queued job as waiting and a scan by its sources', async () => {
    renderApp('/activity/queue', {
      ...names,
      'GET /api/v1/sources': () => ({ body: [{ id: 2, name: 'TV' }] }),
      'GET /api/v1/jobs': () => ({ body: paged([job({ id: 9, type: 'scan', status: 'queued', params: { sourceIds: [2] }, startedAt: null })]) }),
    });
    expect(await screen.findByRole('link', { name: 'Scan · TV' })).toHaveAttribute('href', '/activity/jobs/9');
    expect(screen.getByText(/Waiting for a free worker/)).toBeInTheDocument();
  });
});

describe('Polling', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('refreshes the queue and follows a running job\'s log every 2 s', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const running = job({ id: 15 });
    const { calls } = renderApp('/activity/jobs/15', {
      ...names,
      'GET /api/v1/jobs/15': () => ({ body: running }),
      'GET /api/v1/jobs/15/items/summary': () => ({ body: [] }),
      'GET /api/v1/jobs/15/items': () => ({ body: paged([]) }),
      'GET /api/v1/jobs/15/logs': (_, url) => {
        const after = Number(url.searchParams.get('afterId'));
        const lines = [
          { id: 1, at: new Date().toISOString(), level: 'info', message: 'planning', fields: null },
          { id: 2, at: new Date().toISOString(), level: 'info', message: 'copying 12 files', fields: null },
        ];
        // The second line appears once the first poll has happened.
        const visible = callsTo(calls, 'GET /api/v1/jobs/15/logs').length > 1 ? lines : lines.slice(0, 1);
        return { body: visible.filter((l) => l.id > after) };
      },
    });
    expect(await screen.findByText('planning')).toBeInTheDocument();
    expect(screen.queryByText('copying 12 files')).not.toBeInTheDocument();
    await vi.advanceTimersByTimeAsync(2100);
    expect(await screen.findByText('copying 12 files')).toBeInTheDocument();
    const logCalls = callsTo(calls, 'GET /api/v1/jobs/15/logs');
    expect(logCalls[0].query.get('afterId')).toBe('0');
    expect(logCalls.at(-1)!.query.get('afterId')).toBe('1');
    expect(screen.getAllByText('planning')).toHaveLength(1);
    expect(callsTo(calls, 'GET /api/v1/jobs/15').length).toBeGreaterThan(1);
  });
});

describe('Job log', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  const line = (id: number) => ({ id, at: new Date().toISOString(), level: 'warn', message: `item ${id} failed`, fields: null });
  const detail = (j: ReturnType<typeof job>, logs: Handler) => ({
    ...names,
    [`GET /api/v1/jobs/${j.id}`]: () => ({ body: j }),
    [`GET /api/v1/jobs/${j.id}/items/summary`]: () => ({ body: [] }),
    [`GET /api/v1/jobs/${j.id}/items`]: () => ({ body: paged([]) }),
    [`GET /api/v1/jobs/${j.id}/logs`]: logs,
  });
  const rendered = () => screen.getByRole('log', { name: 'Job log' }).children.length;

  it('keeps only the newest lines of a live log', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    // A sync where every item fails: the log grows by a full page on every request.
    const running = job({ id: 16 });
    renderApp(
      '/activity/jobs/16',
      detail(running, (_, url) => {
        const after = Number(url.searchParams.get('afterId'));
        const limit = Number(url.searchParams.get('limit'));
        return { body: Array.from({ length: limit }, (_, i) => line(after + i + 1)) };
      }),
    );
    expect(await screen.findByText('item 2000 failed')).toBeInTheDocument();
    expect(rendered()).toBe(MAX_LOG_LINES);
    // The view follows the new lines although the number of lines stays the same.
    const follow = vi.spyOn(Element.prototype, 'scrollTop', 'set');
    await vi.advanceTimersByTimeAsync(2100);
    expect(await screen.findByText('item 4000 failed')).toBeInTheDocument();
    expect(rendered()).toBe(MAX_LOG_LINES);
    expect(follow).toHaveBeenCalled();
    expect(screen.queryByText('item 2000 failed')).not.toBeInTheDocument();
    expect(screen.getByText(/2,000 earlier lines are not shown/)).toBeInTheDocument();
  });

  it('loads earlier lines back on request', async () => {
    const done = job({ id: 17, status: 'failed', finishedAt: new Date().toISOString() });
    const total = 3000;
    const { calls, user } = renderApp(
      '/activity/jobs/17',
      detail(done, (_, url) => {
        const after = Number(url.searchParams.get('afterId'));
        const limit = Number(url.searchParams.get('limit'));
        return { body: Array.from({ length: Math.max(0, Math.min(limit, total - after)) }, (_, i) => line(after + i + 1)) };
      }),
    );
    expect(await screen.findByText('item 2000 failed')).toBeInTheDocument();
    expect(screen.queryByText(/earlier lines/)).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByText('item 3000 failed')).toBeInTheDocument();
    expect(rendered()).toBe(MAX_LOG_LINES);
    expect(screen.queryByText('item 1000 failed')).not.toBeInTheDocument();
    expect(screen.getByText(/1,000 earlier lines are not shown/)).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Show earlier lines' }));
    expect(await screen.findByText('item 501 failed')).toBeInTheDocument();
    expect(screen.queryByText('item 500 failed')).not.toBeInTheDocument();
    expect(screen.getByText(/500 earlier lines are not shown/)).toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/jobs/17/logs').at(-1)!.query.get('afterId')).toBe('500');
    await user.click(screen.getByRole('button', { name: 'Show earlier lines' }));
    expect(await screen.findByText('item 1 failed')).toBeInTheDocument();
    expect(rendered()).toBe(total);
    expect(screen.queryByText(/earlier lines/)).not.toBeInTheDocument();
  });

  // A live log whose server side grows by what grow() adds before each poll.
  const growing = (start: number) => {
    let total = start;
    const logs: Handler = (_, url) => {
      const after = Number(url.searchParams.get('afterId'));
      const limit = Number(url.searchParams.get('limit'));
      return { body: Array.from({ length: Math.max(0, Math.min(limit, total - after)) }, (_, i) => line(after + i + 1)) };
    };
    return { logs, grow: (n: number) => (total += n) };
  };
  const lastLogCall = (calls: Call[], id: number) => {
    const c = callsTo(calls, `GET /api/v1/jobs/${id}/logs`).at(-1)!;
    return { afterId: c.query.get('afterId'), limit: c.query.get('limit') };
  };

  it('brings back at least a page of a slowly growing log per click', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const log = growing(MAX_LOG_LINES);
    const { calls, user } = renderApp('/activity/jobs/21', detail(job({ id: 21 }), log.logs));
    expect(await screen.findByText('item 2000 failed')).toBeInTheDocument();
    // One new line per poll: the dropped lines still come back in one click.
    for (let i = 0; i < 10; i++) {
      log.grow(1);
      await vi.advanceTimersByTimeAsync(2100);
    }
    expect(await screen.findByText('item 2010 failed')).toBeInTheDocument();
    expect(screen.getByText(/10 earlier lines are not shown/)).toBeInTheDocument();
    // 300 lines per poll: 1,210 dropped lines are runs of 500, 500 and 210.
    for (let i = 0; i < 4; i++) {
      log.grow(300);
      await vi.advanceTimersByTimeAsync(2100);
    }
    expect(await screen.findByText('item 3210 failed')).toBeInTheDocument();
    expect(screen.getByText(/1,210 earlier lines are not shown/)).toBeInTheDocument();
    vi.useRealTimers();

    await user.click(screen.getByRole('button', { name: 'Show earlier lines' }));
    expect(await screen.findByText('item 501 failed')).toBeInTheDocument();
    expect(lastLogCall(calls, 21)).toEqual({ afterId: '500', limit: '710' });
    expect(screen.getByText(/500 earlier lines are not shown/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Show earlier lines' }));
    expect(await screen.findByText('item 1 failed')).toBeInTheDocument();
    expect(lastLogCall(calls, 21)).toEqual({ afterId: '0', limit: '500' });
    expect(screen.queryByText(/earlier lines are not shown/)).not.toBeInTheDocument();
  });

  it('keeps the earlier lines it loaded back while the job runs on', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const log = growing(MAX_LOG_LINES);
    const running = job({ id: 22 });
    let status = running;
    const { calls, user } = renderApp('/activity/jobs/22', { ...detail(running, log.logs), 'GET /api/v1/jobs/22': () => ({ body: status }) });
    expect(await screen.findByText('item 2000 failed')).toBeInTheDocument();
    log.grow(LOG_PAGE);
    await vi.advanceTimersByTimeAsync(2100);
    expect(await screen.findByText('item 2500 failed')).toBeInTheDocument();
    expect(screen.getByText(/500 earlier lines are not shown/)).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Show earlier lines' }));
    expect(await screen.findByText('item 1 failed')).toBeInTheDocument();
    // New lines wait: they would push the lines just loaded back out again.
    log.grow(LOG_PAGE);
    await vi.advanceTimersByTimeAsync(4200);
    expect(screen.getByText('item 1 failed')).toBeInTheDocument();
    expect(screen.queryByText('item 2501 failed')).not.toBeInTheDocument();
    expect(screen.queryByText(/earlier lines are not shown/)).not.toBeInTheDocument();
    expect(screen.getByText(/New lines are paused/)).toBeInTheDocument();
    // Also when the job ends meanwhile.
    status = { ...running, status: 'completed', finishedAt: new Date().toISOString() };
    log.grow(10);
    await vi.advanceTimersByTimeAsync(4200);
    expect(screen.queryByText('item 3010 failed')).not.toBeInTheDocument();
    expect(screen.getByText('item 1 failed')).toBeInTheDocument();
    vi.useRealTimers();

    const before = callsTo(calls, 'GET /api/v1/jobs/22/logs').length;
    await user.click(screen.getByRole('button', { name: 'Show new lines' }));
    expect(await screen.findByText('item 3010 failed')).toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/jobs/22/logs')[before].query.get('afterId')).toBe('2500');
    // The window keeps the size the lines loaded back gave it.
    expect(screen.queryByText('item 510 failed')).not.toBeInTheDocument();
    expect(screen.getByText('item 511 failed')).toBeInTheDocument();
    expect(screen.getByText(/510 earlier lines are not shown/)).toBeInTheDocument();
    expect(screen.queryByText(/New lines are paused/)).not.toBeInTheDocument();
    expect(rendered()).toBe(MAX_LOG_LINES + LOG_PAGE);
  });
});

describe('History', () => {
  const finished = [
    job({ id: 1, type: 'scan', status: 'completed', params: { sourceIds: [1] }, finishedAt: new Date().toISOString(), summary: '1,200 files' }),
    job({ id: 2, status: 'failed', finishedAt: new Date().toISOString(), error: 'destination not mounted?' }),
  ];

  it('filters by type and status and pages', async () => {
    const { calls, user } = renderApp('/activity/history', {
      ...names,
      'GET /api/v1/jobs': (_, url) => {
        const type = url.searchParams.get('type');
        const status = url.searchParams.get('status');
        const records = finished.filter((j) => (!type || j.type === type) && (!status || j.status === status));
        return { body: paged(records, type ? records.length : 60, Number(url.searchParams.get('page') ?? 1)) };
      },
    });

    expect(await screen.findByText('destination not mounted?')).toBeInTheDocument();
    expect(screen.getByText('1,200 files')).toBeInTheDocument();
    expect(calls.some((c) => c.path === '/api/v1/jobs' && c.query.get('state') === 'finished' && c.query.get('pageSize') === '25')).toBe(true);

    await user.click(screen.getByRole('button', { name: 'Next page' }));
    await waitFor(() => expect(calls.some((c) => c.path === '/api/v1/jobs' && c.query.get('page') === '2')).toBe(true));

    await user.selectOptions(screen.getByLabelText('Type'), 'sync');
    await waitFor(() => expect(screen.queryByText('1,200 files')).not.toBeInTheDocument());
    const last = callsTo(calls, 'GET /api/v1/jobs').at(-1)!;
    expect(last.query.get('type')).toBe('sync');
    expect(last.query.get('page')).toBe('1');
    expect(screen.getByText('destination not mounted?')).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('Status'), 'completed');
    expect(await screen.findByText('No jobs match these filters.')).toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/jobs').at(-1)!.query.get('status')).toBe('completed');

    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    expect(await screen.findByText('1,200 files')).toBeInTheDocument();
  });
});

describe('Job detail', () => {
  const summary: ItemCount[] = [
    { action: 'copy', status: 'done', files: 8, bytes: 8 * GiB },
    { action: 'retain', status: 'held', files: 30, bytes: 30 * GiB },
    { action: 'update', status: 'failed', files: 1, bytes: 5 * MiB },
  ];
  const items: JobItem[] = [
    { id: 1, jobId: 12, relPath: 'movies/A (2001)/A.mkv', action: 'copy', status: 'done', bytes: GiB },
    { id: 2, jobId: 12, relPath: 'movies/B (2002)/B.mkv', action: 'retain', status: 'held', bytes: GiB },
    { id: 3, jobId: 12, relPath: 'movies/C (2003)/C.mkv', action: 'update', status: 'failed', bytes: 5 * MiB, error: 'changed during copy' },
  ];
  const finishedJob = job({
    id: 12,
    status: 'completed_with_warnings',
    finishedAt: new Date().toISOString(),
    warnings: 31,
    summary: '8 copied, 30 held, 1 failed',
    stats: { filesPlanned: 39, filesCopied: 8, filesHeld: 30, filesFailed: 1, bytesPlanned: 39 * GiB, bytesCopied: 8 * GiB, durationMs: 65_000 },
  });

  function routes(overrides = {}) {
    return {
      ...names,
      'GET /api/v1/jobs/12': () => ({ body: finishedJob }),
      'GET /api/v1/jobs/12/items/summary': () => ({ body: summary }),
      'GET /api/v1/jobs/12/items': (_: unknown, url: URL) => {
        const action = url.searchParams.get('action');
        const status = url.searchParams.get('status');
        const records = items.filter((i) => (!action || i.action === action) && (!status || i.status === status));
        return { body: paged(records, records.length, 1, 50) };
      },
      'GET /api/v1/jobs/12/logs': () => ({ body: [{ id: 1, at: new Date().toISOString(), level: 'warn', message: 'mass-change guard held 30 changes', fields: { source: 'Movies' } }] }),
      ...overrides,
    };
  }

  it('shows stats, the item summary, filters items and logs', async () => {
    const { calls, user } = renderApp('/activity/jobs/12', routes());
    expect(await screen.findByRole('heading', { name: 'Sync #12' })).toBeInTheDocument();
    expect(screen.getByText('8 copied, 30 held, 1 failed')).toBeInTheDocument();
    const stats = screen.getByRole('group', { name: 'Statistics' });
    expect(within(stats).getByText('39.0 GiB')).toBeInTheDocument();
    expect(within(stats).getByText('1m 5s')).toBeInTheDocument();

    expect(await screen.findByText('movies/C (2003)/C.mkv')).toBeInTheDocument();
    expect(screen.getByText('changed during copy')).toBeInTheDocument();
    expect(await screen.findByText('mass-change guard held 30 changes')).toBeInTheDocument();
    expect(screen.getByText('source=Movies')).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('Action'), 'update');
    await waitFor(() => expect(screen.queryByText('movies/A (2001)/A.mkv')).not.toBeInTheDocument());
    expect(callsTo(calls, 'GET /api/v1/jobs/12/items').at(-1)!.query.get('action')).toBe('update');
    expect(screen.getByText('movies/C (2003)/C.mkv')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    await user.click(await screen.findByRole('button', { name: 'Show copy items: done' }));
    await waitFor(() => expect(screen.queryByText('movies/C (2003)/C.mkv')).not.toBeInTheDocument());
    const last = callsTo(calls, 'GET /api/v1/jobs/12/items').at(-1)!;
    expect(last.query.get('action')).toBe('copy');
    expect(last.query.get('status')).toBe('done');
  });

  it('applies held changes with a sync that allows changes', async () => {
    const next = job({ id: 13, status: 'queued', params: { destinationId: 1, allowChanges: true } });
    const { calls, user } = renderApp(
      '/activity/jobs/12',
      routes({
        'POST /api/v1/destinations/1/sync': () => ({ status: 202, body: next }),
        'GET /api/v1/jobs/13': () => ({ body: next }),
        'GET /api/v1/jobs/13/items/summary': () => ({ body: [] }),
        'GET /api/v1/jobs/13/items': () => ({ body: paged([]) }),
        'GET /api/v1/jobs/13/logs': () => ({ body: [] }),
      }),
    );

    expect(await screen.findByText('30 changes were held by the mass-change guard')).toBeInTheDocument();
    expect(screen.getByText(/more than 10% of a source's files/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Show held items' }));
    await waitFor(() => expect(screen.queryByText('movies/A (2001)/A.mkv')).not.toBeInTheDocument());
    expect(screen.getByText('movies/B (2002)/B.mkv')).toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/jobs/12/items').at(-1)!.query.get('status')).toBe('held');

    await user.click(screen.getByRole('button', { name: 'Apply held changes' }));
    const dialog = await screen.findByRole('dialog', { name: 'Apply held changes' });
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Start sync' }));

    expect(await screen.findByRole('heading', { name: 'Sync #13' })).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')[0].body).toEqual({ dryRun: false, allowChanges: true });
    expect(screen.getByText('Applies held changes')).toBeInTheDocument();
  });

  it('presents a dry run as a preview of planned items', async () => {
    const preview = job({ id: 12, dryRun: true, status: 'completed', finishedAt: new Date().toISOString() });
    const { calls, user } = renderApp(
      '/activity/jobs/12',
      routes({
        'GET /api/v1/jobs/12': () => ({ body: preview }),
        'GET /api/v1/jobs/12/items/summary': () => ({ body: [{ action: 'copy', status: 'pending', files: 1, bytes: GiB }] }),
        'GET /api/v1/jobs/12/items': () => ({ body: paged([{ ...items[0], status: 'pending' }]) }),
        'POST /api/v1/destinations/1/sync': () => ({ status: 202, body: job({ id: 14, status: 'queued' }) }),
        'GET /api/v1/jobs/14': () => ({ body: job({ id: 14, status: 'queued' }) }),
      }),
    );
    expect(await screen.findByText('Preview (dry run)')).toBeInTheDocument();
    expect(screen.getAllByText('Planned').length).toBeGreaterThan(0);
    await user.click(screen.getByRole('button', { name: 'Run this sync' }));
    await user.click(within(await screen.findByRole('dialog', { name: 'Run this sync' })).getByRole('button', { name: 'Start sync' }));
    expect(await screen.findByRole('heading', { name: 'Sync #14' })).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')[0].body).toEqual({ dryRun: false, allowChanges: false });
  });
});

// Shapes recorded from the real server (internal/syncer SyncStats, internal/catalog ScanRunner
// stats, internal/syncer Detail): nested values must not leak into the page as raw JSON.
describe('Job detail against real API shapes', () => {
  const syncStats = {
    dryRun: true,
    filesPlanned: 10,
    filesCopied: 9,
    filesUpdated: 0,
    filesLinked: 1,
    filesHeld: 1,
    bytesPlanned: 12 * MiB,
    bytesCopied: 0,
    durationMs: 2,
    sources: [
      { sourceId: 1, name: 'Movies', files: 4, added: 4, changed: 0, deleted: 0, skipped: 1, changes: 0, held: 0 },
      { sourceId: 2, name: 'TV', files: 6, added: 6, changed: 0, deleted: 0, skipped: 0, changes: 2, held: 1 },
    ],
  };

  function routesFor(j: ReturnType<typeof job>, summary: ItemCount[], items: JobItem[] = []) {
    return {
      ...names,
      [`GET /api/v1/jobs/${j.id}`]: () => ({ body: j }),
      [`GET /api/v1/jobs/${j.id}/items/summary`]: () => ({ body: summary }),
      [`GET /api/v1/jobs/${j.id}/items`]: () => ({ body: paged(items, items.length, 1, 50) }),
      [`GET /api/v1/jobs/${j.id}/logs`]: () => ({ body: [] }),
    };
  }

  it('shows a dry run\'s stats as what would happen, with a per-source table', async () => {
    const preview = job({ id: 21, dryRun: true, status: 'completed', finishedAt: new Date().toISOString(), stats: syncStats });
    renderApp('/activity/jobs/21', routesFor(preview, [{ action: 'copy', status: 'pending', files: 9, bytes: 12 * MiB }]));
    const stats = await screen.findByRole('group', { name: 'Statistics' });
    expect(within(stats).getByText('To copy')).toBeInTheDocument();
    expect(within(stats).getByText('Would be held')).toBeInTheDocument();
    expect(within(stats).queryByText('Copied')).not.toBeInTheDocument();
    // bytesCopied is always 0 in a dry run; dryRun itself is the page's Preview notice.
    expect(within(stats).queryByText('Copied (unique)')).not.toBeInTheDocument();
    expect(within(stats).queryByText('Dry run')).not.toBeInTheDocument();
    expect(within(stats).queryByText('Sources')).not.toBeInTheDocument();
    expect(screen.queryByText(/"sourceId"/)).not.toBeInTheDocument();

    const table = screen.getByRole('table', { name: 'By source' });
    const tv = within(table).getByRole('row', { name: /^TV/ });
    expect(within(table).getByRole('columnheader', { name: 'Retain + update' })).toBeInTheDocument();
    expect(within(tv).getAllByRole('cell').map((c) => c.textContent)).toEqual(['6', '6', '0', '0', '0', '2', '1']);
  });

  it('shows a scan without an empty item table, with skipped reasons and warnings per source', async () => {
    const scan = job({
      id: 22,
      type: 'scan',
      status: 'completed_with_warnings',
      params: { sourceIds: [1] },
      finishedAt: new Date().toISOString(),
      stats: {
        sourcesScanned: 1,
        files: 4,
        bytes: 11 * MiB,
        skipped: 1,
        groups: 1,
        durationMs: 0,
        sources: [
          {
            sourceId: 1,
            sourceName: 'Movies',
            startedAt: new Date().toISOString(),
            fsType: 'ext4',
            fuse: false,
            files: 4,
            bytes: 11 * MiB,
            skipped: { symlink: 1 },
            unreadableDirs: ['Extras'],
            warnings: ['Extras: permission denied'],
            warningCount: 1,
          },
        ],
      },
    });
    renderApp('/activity/jobs/22', routesFor(scan, []));
    expect(await screen.findByText(/A scan updates the catalog and plans no file items/)).toBeInTheDocument();
    expect(screen.queryByLabelText('Action')).not.toBeInTheDocument();
    expect(screen.queryByText('No items match.')).not.toBeInTheDocument();
    const stats = screen.getByRole('group', { name: 'Statistics' });
    expect(within(stats).getByText('Hardlink groups')).toBeInTheDocument();
    const table = screen.getByRole('table', { name: 'By source' });
    const row = within(table).getByRole('row', { name: /^Movies/ });
    expect(within(row).getByTitle('symlink 1')).toHaveTextContent('1');
    expect(within(row).getByText('11.0 MiB')).toBeInTheDocument();
    expect(screen.getByText('Movies: cannot read Extras')).toBeInTheDocument();
    expect(screen.getByText('Movies: Extras: permission denied')).toBeInTheDocument();
    expect(screen.queryByText(/startedAt/)).not.toBeInTheDocument();
  });

  it('hides an empty per-source list (a resumed sync) and explains a failed job without items', async () => {
    const failed = job({ id: 23, status: 'failed', finishedAt: new Date().toISOString(), error: 'destination not mounted?', stats: { filesPlanned: 0, sources: [] } });
    renderApp('/activity/jobs/23', routesFor(failed, []));
    expect(await screen.findByText(/the job stopped before it planned any file work/)).toBeInTheDocument();
    expect(screen.queryByRole('table', { name: 'By source' })).not.toBeInTheDocument();
    expect(screen.queryByText('[]')).not.toBeInTheDocument();
  });

  it('shows the planner\'s detail and a held item\'s reason as a warning', async () => {
    const done = job({ id: 24, status: 'completed_with_warnings', finishedAt: new Date().toISOString() });
    const items: JobItem[] = [
      { id: 1, jobId: 24, relPath: 'tv/S01E01 - Pilot.mkv', action: 'move', status: 'done', bytes: 200_000, detail: { reason: 'renamed', from: 'tv/S01E01.mkv', temp: 'x' } },
      { id: 2, jobId: 24, relPath: 'movies/A/hardlink.mkv', action: 'link', status: 'done', bytes: 0, detail: { reason: 'hardlink', primary: 'A/A.mkv' } },
      { id: 3, jobId: 24, relPath: 'tv/S01E03.mkv', action: 'update', status: 'held', bytes: 100, error: 'held: the new version is 100 bytes', detail: { reason: 'changed' } },
      { id: 4, jobId: 24, relPath: 'tv/S01E04.mkv', action: 'copy', status: 'failed', bytes: 5, error: 'changed during copy' },
    ];
    renderApp(
      '/activity/jobs/24',
      routesFor(
        done,
        [
          { action: 'move', status: 'done', files: 1, bytes: 200_000 },
          { action: 'update', status: 'held', files: 1, bytes: 100 },
        ],
        items,
      ),
    );
    expect(await screen.findByText('reason: renamed · from: tv/S01E01.mkv')).toBeInTheDocument();
    expect(screen.getByText('reason: hardlink · hardlink of: A/A.mkv')).toBeInTheDocument();
    expect(screen.getByText('held: the new version is 100 bytes')).toHaveClass('text-warn');
    expect(screen.getByText('changed during copy')).toHaveClass('text-danger');
  });
});

describe('Cancelling', () => {
  it('says a queued job is only taken off the queue', async () => {
    const { user } = renderApp('/activity/queue', {
      ...names,
      'GET /api/v1/jobs': () => ({ body: paged([job({ id: 30, type: 'verify', status: 'queued', startedAt: null })]) }),
    });
    await user.click(await screen.findByRole('button', { name: 'Cancel job #30' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Cancel job' }));
    expect(dialog.getByText(/taken off the queue and nothing is changed/)).toBeInTheDocument();
    expect(dialog.getByRole('button', { name: 'Keep it queued' })).toBeInTheDocument();
  });
});
