import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { DestinationInput, DestinationTestResult } from '@/api/types';
import { callsTo } from '@/test/fetch';
import { destination, GiB, job, paged, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { DEFAULT_RETENTION, DEFAULT_SETTINGS, validateDestination } from './DestinationForm';

const probe: DestinationTestResult = {
  ok: true,
  marker: 'foreign',
  writable: true,
  fsType: 'nfs',
  local: true,
  capabilities: { hardlinks: false, caseInsensitive: true, invalidChars: ':?', trailingDotSpace: true, mtimeGranularityNs: 100, fsType: 'nfs', checkedAt: new Date().toISOString() },
  freeBytes: 3 * 1024 * GiB,
  totalBytes: 4 * 1024 * GiB,
  entries: 3,
  message: 'The target is usable.',
  warnings: ['A Bunkarr destination marker already exists (UNAS-old).', 'The target is on the same filesystem as the config directory.'],
};

function base(extra = {}) {
  return {
    'GET /api/v1/sources': () => ({ body: [source(), source({ id: 2, name: 'TV', path: '/media/tv', destFolder: 'tv' })] }),
    'GET /api/v1/integrations': () => ({ body: [] }),
    ...extra,
  };
}

describe('Destination form', () => {
  it('requires a test, shows its warnings and requires the attach and local confirmations', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [] }),
        'POST /api/v1/destinations/test': () => ({ body: probe }),
        'POST /api/v1/destinations': (init?: RequestInit) => ({ status: 201, body: destination(JSON.parse(String(init?.body))) }),
      }),
    );

    await user.click((await screen.findAllByRole('button', { name: 'Add destination' }))[0]);
    const dialog = await screen.findByRole('dialog', { name: 'Add destination' });
    const form = within(dialog);
    await user.type(form.getByLabelText('Name'), 'UNAS');
    await user.type(form.getByLabelText('Target'), '/backup');

    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Test the target first/)).toBeInTheDocument();

    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('A Bunkarr destination marker already exists (UNAS-old).')).toBeInTheDocument();
    // The test answers "Test the target first": the stale message goes away.
    expect(form.queryByText(/Test the target first/)).not.toBeInTheDocument();
    expect(form.getByText('The target is on the same filesystem as the config directory.')).toBeInTheDocument();
    expect(form.getByText('Existing marker')).toBeInTheDocument();
    expect(form.getByText('Local')).toBeInTheDocument();
    expect(form.getByText('Case-insensitive')).toBeInTheDocument();
    expect(form.getByText('3.0 TiB free of 4.0 TiB')).toBeInTheDocument();
    expect(form.getByText(/cannot store hardlinks/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/test')[0].body).toEqual({ target: '/backup' });

    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Confirm that you want to attach/)).toBeInTheDocument();
    await user.click(form.getByRole('checkbox', { name: /Attach to the existing Bunkarr destination/ }));

    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/local filesystem. Confirm/)).toBeInTheDocument();
    await user.click(form.getByRole('checkbox', { name: /back up to this local filesystem/ }));
    expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(0);

    await user.click(form.getByRole('checkbox', { name: /^Movies/ }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput;
    expect(body).toMatchObject({ name: 'UNAS', engine: 'filecopy', target: '/backup', attach: true, allowLocal: true, sourceIds: [1] });
    expect(body.settings).toEqual({ verify: { mode: 'sample', samplePercent: 5 }, hardlinks: 'recreate', adoptExisting: 'size+mtime', mtimeWindowSec: 0, maxChangePercent: 10, maxChangeFiles: 1000 });
    expect(body.retention).toEqual({ deletedDays: 30, plexDbDaily: 14, plexDbWeekly: 8 });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('turns schedule presets, manual and custom cron into the request', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [] }),
        'POST /api/v1/destinations/test': () => ({ body: { ...probe, marker: 'missing', local: false, warnings: [] } }),
        'POST /api/v1/destinations': () => ({ status: 201, body: destination() }),
      }),
    );
    await user.click((await screen.findAllByRole('button', { name: 'Add destination' }))[0]);
    const form = within(await screen.findByRole('dialog', { name: 'Add destination' }));
    await user.type(form.getByLabelText('Name'), 'UNAS');
    await user.type(form.getByLabelText('Target'), '/backup');
    await user.click(form.getByRole('button', { name: 'Test' }));
    await form.findByText('None yet (written on create)');
    expect(form.queryByRole('checkbox', { name: /Attach/ })).not.toBeInTheDocument();

    const sync = form.getByLabelText('Sync');
    expect(sync).toHaveDisplayValue('Nightly at 02:00');
    await user.selectOptions(sync, 'Every 6 hours');
    expect(form.getByText(/Every 6 hours at minute 0/)).toBeInTheDocument();
    await user.selectOptions(form.getByLabelText('Verify'), 'Manual only (disabled)');

    await user.selectOptions(sync, 'Custom (cron)');
    const cron = form.getByLabelText('Sync cron expression');
    await user.clear(cron);
    await user.type(cron, '0 25 * * *');
    expect(form.getByText(/hour must be between 0 and 23/)).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/The sync schedule is invalid/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(0);

    await user.clear(cron);
    await user.type(cron, '15 1 * * 1-5');
    await user.selectOptions(form.getByLabelText('Verify mode'), 'full');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput;
    expect(body.schedule).toEqual({ cron: '15 1 * * 1-5', enabled: true });
    expect(body.verifySchedule).toEqual({ cron: '0 5 * * 0', enabled: false });
    expect(body.settings.verify.mode).toBe('full');
    expect(body).not.toHaveProperty('attach');
    expect(body).not.toHaveProperty('allowLocal');
  });

  it('refuses to create on a target the test rejects', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [] }),
        'POST /api/v1/destinations/test': () => ({
          body: { ...probe, ok: false, marker: 'missing', local: false, writable: false, capabilities: null, message: 'the target does not exist', warnings: [] },
        }),
      }),
    );
    await user.click((await screen.findAllByRole('button', { name: 'Add destination' }))[0]);
    const form = within(await screen.findByRole('dialog', { name: 'Add destination' }));
    await user.type(form.getByLabelText('Name'), 'UNAS');
    await user.type(form.getByLabelText('Target'), '/nope');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('the target does not exist')).toBeInTheDocument();
    expect(form.getByText(/Probed when the destination is created/)).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/The target cannot be used: the target does not exist/)).toBeInTheDocument();
    await user.clear(form.getByLabelText('Target'));
    await user.type(form.getByLabelText('Target'), '/backup');
    expect(form.queryByText('the target does not exist')).not.toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(0);
  });

  it('edits an existing destination with a fixed target', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [destination()] }),
        'PUT /api/v1/destinations/1': () => ({ body: destination() }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit UNAS' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit destination · UNAS' }));
    expect(form.getByLabelText('Target')).toBeDisabled();
    expect(form.getByLabelText('Sync')).toHaveDisplayValue('Nightly at 02:00');
    expect(form.getByLabelText('Verify')).toHaveDisplayValue('Weekly (Sunday 03:00)');
    const keep = form.getByLabelText('Keep deleted files');
    await user.clear(keep);
    await user.type(keep, '60');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/destinations/1')).toHaveLength(1));
    const body = callsTo(calls, 'PUT /api/v1/destinations/1')[0].body as DestinationInput;
    expect(body.retention.deletedDays).toBe(60);
    expect(body.target).toBe('/backup');
  });
});

describe('Destination schedules', () => {
  it('drops an invalid custom cron when the schedule is switched to Manual only', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [destination()] }),
        'PUT /api/v1/destinations/1': (init?: RequestInit) => ({ body: destination(JSON.parse(String(init?.body))) }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit UNAS' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit destination · UNAS' }));
    const sync = form.getByLabelText('Sync');
    await user.selectOptions(sync, 'Custom (cron)');
    const cron = form.getByLabelText('Sync cron expression');
    await user.clear(cron);
    await user.type(cron, '0 2 * * 8');
    expect(form.getByText(/day of week must be between 0 and 6/)).toBeInTheDocument();
    await user.selectOptions(sync, 'Manual only (disabled)');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/destinations/1')).toHaveLength(1));
    // The server refuses any invalid cron, even on a disabled schedule.
    expect((callsTo(calls, 'PUT /api/v1/destinations/1')[0].body as DestinationInput).schedule).toEqual({ cron: '', enabled: false });
  });
});

describe('Destinations list', () => {
  it('shows the last sync and capabilities, previews, syncs and tests', async () => {
    const preview = job({ id: 30, dryRun: true, status: 'queued' });
    const last = job({ id: 29, status: 'completed_with_warnings', finishedAt: new Date().toISOString(), summary: '2 held' });
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [destination({ lastJob: last, lastSync: last })] }),
        'POST /api/v1/destinations/1/sync': (init?: RequestInit) => {
          const b = JSON.parse(String(init?.body)) as { dryRun: boolean };
          return { status: 202, body: b.dryRun ? preview : job({ id: 31, status: 'queued' }) };
        },
        'POST /api/v1/destinations/1/test': () => ({
          body: { ...probe, marker: 'ok', local: false, capabilities: { ...probe.capabilities, hardlinks: true, unstableInodes: true, probeVersion: 1 }, warnings: null },
        }),
        'GET /api/v1/jobs/30': () => ({ body: preview }),
        'GET /api/v1/jobs/30/items/summary': () => ({ body: [] }),
        'GET /api/v1/jobs/30/items': () => ({ body: paged([]) }),
        'GET /api/v1/jobs/30/logs': () => ({ body: [] }),
      }),
    );

    expect(await screen.findByRole('link', { name: /Warnings/ })).toHaveAttribute('href', '/activity/jobs/29');
    expect(screen.getByText('Hardlinks')).toBeInTheDocument();
    expect(screen.getByText('Sync: Daily at 02:00')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Sync UNAS now' }));
    expect(await screen.findByRole('link', { name: 'View job #31' })).toHaveAttribute('href', '/activity/jobs/31');
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')[0].body).toEqual({ dryRun: false, allowChanges: false });

    await user.click(screen.getByRole('button', { name: 'Test UNAS' }));
    const test = within(await screen.findByRole('dialog', { name: 'Test · UNAS' }));
    expect(await test.findByText('Marker OK')).toBeInTheDocument();
    // A share whose inode numbers change per lookup (SMB with noserverino) is compared by content.
    expect(test.getByText('Inode numbers not stable (content compared)')).toBeInTheDocument();
    expect(screen.queryAllByText('Inode numbers not stable (content compared)')).toHaveLength(1);
    await user.click(test.getByRole('button', { name: 'Done' }));

    await user.click(screen.getByRole('button', { name: 'Preview a sync of UNAS' }));
    expect(await screen.findByRole('heading', { name: 'Sync #30' })).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')[1].body).toEqual({ dryRun: true, allowChanges: false });
  });
});

describe('validateDestination', () => {
  const valid: DestinationInput = {
    name: 'UNAS',
    engine: 'filecopy',
    target: '/backup',
    enabled: true,
    sourceIds: [],
    schedule: { cron: '0 2 * * *', enabled: true },
    verifySchedule: { cron: '0 5 * * 0', enabled: true },
    settings: DEFAULT_SETTINGS,
    retention: DEFAULT_RETENTION,
  };

  // The server's ranges (internal/destinations): a 0 would silently become the default there.
  it.each([
    ['deletedDays 0', { deletedDays: 0 }, /1–3650 days/],
    ['deletedDays 3651', { deletedDays: 3651 }, /1–3650 days/],
    ['plexDbDaily 366', { plexDbDaily: 366 }, /1–365 daily/],
    ['plexDbWeekly 0', { plexDbWeekly: 0 }, /1–520 weekly/],
    ['plexDbWeekly 521', { plexDbWeekly: 521 }, /1–520 weekly/],
  ])('rejects %s', (_, patch, message) => {
    expect(validateDestination({ ...valid, retention: { ...DEFAULT_RETENTION, ...patch } })).toMatch(message);
  });

  // As the server (internal/api validateSchedule): any non-empty cron must be valid, even on a
  // schedule that is off; an enabled one needs a cron.
  it('checks the cron of a disabled schedule too', () => {
    expect(validateDestination({ ...valid, schedule: { cron: '0 2 * * 8', enabled: false } })).toMatch(/sync schedule is invalid/);
    expect(validateDestination({ ...valid, verifySchedule: { cron: '@every 1h', enabled: false } })).toMatch(/verify schedule is invalid/);
    expect(validateDestination({ ...valid, schedule: { cron: '', enabled: true } })).toMatch(/sync schedule is invalid/);
    expect(validateDestination({ ...valid, schedule: { cron: '', enabled: false } })).toBeNull();
  });

  it('accepts the defaults and the range limits', () => {
    expect(validateDestination(valid)).toBeNull();
    expect(validateDestination({ ...valid, retention: { deletedDays: 3650, plexDbDaily: 365, plexDbWeekly: 520 } })).toBeNull();
  });
});

describe('Destination dialogs', () => {
  afterEach(() => {
    delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
  });

  it('scrolls a repeated form error into view on every Save', async () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    const { user } = renderApp('/destinations', base({ 'GET /api/v1/destinations': () => ({ body: [] }) }));
    await user.click((await screen.findAllByRole('button', { name: 'Add destination' }))[0]);
    const form = within(await screen.findByRole('dialog', { name: 'Add destination' }));
    // Calls on the notice that shows the message (its text is the message).
    const revealed = () => scroll.mock.contexts.filter((el) => (el as Element).textContent === 'Enter a name.').length;
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('Enter a name.')).toBeInTheDocument();
    expect(revealed()).toBe(1);
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(revealed()).toBe(2));
  });

  it('keeps Tab inside the folder browser opened from the form', async () => {
    const { user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [] }),
        'GET /api/v1/filesystem': () => ({
          body: { path: '/', parent: '', directories: [{ name: 'backup', path: '/backup' }], truncated: false },
        }),
      }),
    );
    await user.click((await screen.findAllByRole('button', { name: 'Add destination' }))[0]);
    const form = within(await screen.findByRole('dialog', { name: 'Add destination' }));
    await user.click(form.getByRole('button', { name: 'Browse for Target' }));
    const browser = await screen.findByRole('dialog', { name: 'Choose target' });
    await within(browser).findByText('backup');
    // Tab from any item of the folder browser, and Shift+Tab, stay in it: the form behind it
    // does not take focus.
    for (const item of within(browser).getAllByRole('button')) {
      item.focus();
      await user.tab();
      expect(browser).toContainElement(document.activeElement as HTMLElement);
      item.focus();
      await user.keyboard('{Shift>}{Tab}{/Shift}');
      expect(browser).toContainElement(document.activeElement as HTMLElement);
    }
  });

  it('names a snapshot whose Plex server was deleted', async () => {
    const { user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [destination()] }),
        'GET /api/v1/destinations/1/snapshots': () => ({
          body: [
            {
              id: 1,
              destinationId: 1,
              integrationId: 0,
              jobId: 0,
              path: '.bunkarr/plex/plex/20260925T060000Z',
              createdAt: new Date().toISOString(),
              size: 5 * GiB,
              method: 'backup-api',
              integrity: 'ok',
              manifest: {},
            },
          ],
        }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Plex DB snapshots on UNAS' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Plex DB snapshots · UNAS' }));
    expect(await dialog.findByText('Deleted server')).toBeInTheDocument();
    expect(dialog.queryByText('#0')).not.toBeInTheDocument();
  });
});

describe('Destinations list', () => {
  it('shows the last sync, not a later retention or verify job', async () => {
    const hoursAgo = (h: number) => new Date(Date.now() - h * 3600_000).toISOString();
    const failedSync = job({ id: 31, status: 'failed', finishedAt: hoursAgo(4), summary: 'source not mounted' });
    const retention = job({ id: 32, type: 'retention', status: 'completed', finishedAt: hoursAgo(1) });
    renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({
          body: [
            destination({ lastJob: retention, lastSync: failedSync }),
            destination({ id: 2, name: 'Offsite', target: '/offsite', lastJob: retention, lastSync: null }),
          ],
        }),
      }),
    );
    const table = await screen.findByRole('table', { name: 'Destinations' });
    expect(within(table).getByRole('columnheader', { name: 'Last sync' })).toBeInTheDocument();
    const unas = within((await within(table).findByText('UNAS')).closest('tr')!);
    expect(unas.getByRole('link', { name: /Failed/ })).toHaveAttribute('href', '/activity/jobs/31');
    expect(unas.queryByText(/Retention/)).not.toBeInTheDocument();
    // A destination with retention jobs but no sync has never synced.
    const offsite = within(within(table).getByText('Offsite').closest('tr')!);
    expect(offsite.getByText('Never synced')).toBeInTheDocument();
    expect(offsite.queryByText(/Retention/)).not.toBeInTheDocument();
  });
});
