import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { Schedule } from '@/api/types';
import { callsTo } from '@/test/fetch';
import { destination, job, paged } from '@/test/fixtures';
import { renderApp } from '@/test/render';

const inHours = (h: number) => new Date(Date.now() + h * 3600_000).toISOString();

const schedules: Schedule[] = [
  {
    id: 1,
    jobType: 'retention',
    params: {},
    description: 'Expire retained files and old job history',
    cron: '30 4 * * *',
    enabled: true,
    lastRunAt: null,
    nextRunAt: inHours(3),
    blockedReason: '',
  },
  { id: 2, jobType: 'sync', params: { destinationId: 1 }, description: '', cron: '0 2 * * *', enabled: false, lastRunAt: inHours(-20), nextRunAt: null, blockedReason: '' },
];

function routes(extra = {}) {
  return {
    'GET /api/v1/schedules': () => ({ body: schedules }),
    'GET /api/v1/destinations': () => ({ body: [destination()] }),
    'GET /api/v1/sources': () => ({ body: [] }),
    'GET /api/v1/integrations': () => ({ body: [] }),
    ...extra,
  };
}

describe('System → Tasks', () => {
  it('lists schedules and runs one now', async () => {
    const { calls, user } = renderApp(
      '/system/tasks',
      routes({ 'POST /api/v1/schedules/1/run': () => ({ status: 202, body: job({ id: 21, type: 'retention', status: 'queued', params: {} }) }) }),
    );
    expect(await screen.findByText('Expire retained files and old job history')).toBeInTheDocument();
    expect(screen.getByText('Daily at 04:30')).toBeInTheDocument();
    expect(screen.getByText('in 3 hours')).toBeInTheDocument();
    expect(screen.getByText('Never')).toBeInTheDocument();
    expect(await screen.findByText('Sync · UNAS')).toBeInTheDocument();
    expect(screen.getByText('Disabled')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Run Expire retained files and old job history now' }));
    expect(await screen.findByRole('link', { name: 'View job #21' })).toHaveAttribute('href', '/activity/jobs/21');
    expect(callsTo(calls, 'POST /api/v1/schedules/1/run')).toHaveLength(1);
    expect(callsTo(calls, 'POST /api/v1/schedules/1/run')[0].body).toBeUndefined();
  });

  it('previews retention: a dry run whose destination jobs list what would expire', async () => {
    const done = { status: 'completed' as const, finishedAt: new Date().toISOString() };
    // The retention schedule's dry run queues a dry run per destination (its log names them).
    const global = job({ ...done, id: 22, type: 'retention', dryRun: true, params: {}, stats: { dryRun: true, jobsQueued: 1, jobsPruned: 0 } });
    const perDestination = job({ ...done, id: 23, type: 'retention', dryRun: true, params: { destinationId: 1 }, stats: { dryRun: true, filesPlanned: 1 } });
    const expire = { id: 1, jobId: 23, relPath: 'movies/Gone (2020)/Gone.mkv', action: 'expire', status: 'pending', bytes: 1000 };
    const { calls, user } = renderApp(
      '/system/tasks',
      routes({
        'POST /api/v1/schedules/1/run': () => ({ status: 202, body: global }),
        'GET /api/v1/jobs/22': () => ({ body: global }),
        'GET /api/v1/jobs/22/items/summary': () => ({ body: [] }),
        'GET /api/v1/jobs/22/items': () => ({ body: paged([]) }),
        'GET /api/v1/jobs/22/logs': () => ({
          body: [{ id: 1, at: new Date().toISOString(), level: 'info', message: 'retention queued', fields: { destination: 'UNAS', jobId: 23 } }],
        }),
        'GET /api/v1/jobs/23': () => ({ body: perDestination }),
        'GET /api/v1/jobs/23/items/summary': () => ({ body: [{ action: 'expire', status: 'pending', files: 1, bytes: 1000 }] }),
        'GET /api/v1/jobs/23/items': () => ({ body: paged([expire]) }),
        'GET /api/v1/jobs/23/logs': () => ({ body: [] }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Preview Expire retained files and old job history' }));
    // The job opens.
    expect(await screen.findByRole('heading', { name: 'Retention #22' })).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/schedules/1/run')[0].body).toEqual({ dryRun: true });
    expect(screen.getByText(/Nothing was deleted/)).toBeInTheDocument();
    expect(screen.queryByText(/what a sync would do/)).not.toBeInTheDocument();
    // Its log links the destination's preview, whose items are the files a run would delete.
    await user.click(await screen.findByRole('link', { name: 'jobId=23' }));
    expect(await screen.findByRole('heading', { name: 'Retention #23' })).toBeInTheDocument();
    expect(await screen.findByText('movies/Gone (2020)/Gone.mkv')).toBeInTheDocument();
    expect(screen.getByText(/retained files a retention run would delete now/)).toBeInTheDocument();
  });

  it('enables a schedule and edits its cron', async () => {
    const { calls, user } = renderApp('/system/tasks', routes({ 'PUT /api/v1/schedules/2': () => ({ body: schedules[1] }) }));
    const toggle = await screen.findByRole('switch', { name: 'Enable Sync · UNAS' });
    expect(toggle).toHaveAttribute('aria-checked', 'false');
    await user.click(toggle);
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/schedules/2')).toHaveLength(1));
    expect(callsTo(calls, 'PUT /api/v1/schedules/2')[0].body).toEqual({ cron: '0 2 * * *', enabled: true });

    await user.click(screen.getByRole('button', { name: 'Edit schedule of Sync · UNAS' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Schedule · Sync · UNAS' }));
    await user.selectOptions(dialog.getByLabelText('Runs'), 'Every 6 hours');
    await user.click(dialog.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/schedules/2')).toHaveLength(2));
    expect(callsTo(calls, 'PUT /api/v1/schedules/2')[1].body).toEqual({ cron: '0 */6 * * *', enabled: false });
  });

  it('says why a schedule will not run instead of showing a next run', async () => {
    const blocked: Schedule = {
      id: 3,
      jobType: 'verify',
      params: { destinationId: 1 },
      description: 'Verify UNAS',
      cron: '0 5 * * 0',
      enabled: true,
      lastRunAt: null,
      nextRunAt: null,
      blockedReason: 'destination "UNAS" is disabled',
    };
    const { calls } = renderApp('/system/tasks', routes({ 'GET /api/v1/schedules': () => ({ body: [schedules[0], blocked] }) }));
    const row = within((await screen.findByText('Verify UNAS')).closest('tr')!);
    expect(row.getByText('Not running')).toBeInTheDocument();
    expect(row.getByText('destination "UNAS" is disabled')).toBeInTheDocument();
    expect(row.getByRole('switch', { name: 'Enable Verify UNAS' })).toHaveAttribute('aria-checked', 'true');
    const run = row.getByRole('button', { name: 'Run Verify UNAS now' });
    expect(run).toBeDisabled();
    expect(run).toHaveAttribute('title', 'Cannot run: destination "UNAS" is disabled');
    expect(row.getByRole('button', { name: 'Preview Verify UNAS' })).toBeDisabled();
    expect(callsTo(calls, 'POST /api/v1/schedules/3/run')).toHaveLength(0);
    // The runnable schedule still shows its next run.
    expect(screen.getByText('in 3 hours')).toBeInTheDocument();
  });
});
