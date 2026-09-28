import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { ArrIntegrationInput } from '@/api/arr';
import type { Integration, IntegrationInput } from '@/api/types';
import { targetIndexOf, targetsNeedPassword, targetsOf, targetsProblem } from '@/components/BackupTargetsEditor';
import { validateCron } from '@/lib/cron';
import { callsTo, type Handler } from '@/test/fetch';
import { destination, plexIntegration, rcloneDestination, resticDestination } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { slowPage } from '@/test/slow';

// Backup targets of Plex and *arr backups (docs/design/phase4.md §8.5, §15): up to four
// destinations each with a schedule; a new off-site target needs the password (S29), and the
// server's 400 for an unencrypted off-site target is shown on its row.

slowPage();

const LOGIN = 'my-login-password';
const unas = destination({ capabilities: { ...destination().capabilities!, enforcesModes: true } });
const b2 = resticDestination({ id: 11, name: 'B2', kind: 'b2', encryption: { mode: 'restic', origin: 'generated', kitExportedAt: null, kitConfirmedAt: '2026-09-20T10:00:00Z' } });
const plain = rcloneDestination({ id: 12, name: 'Plain S3', encryption: { mode: 'none', origin: '', kitExportedAt: null, kitConfirmedAt: null } });

function routes(it: Integration, extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/integrations': () => ({ body: [it] }),
    'GET /api/v1/destinations': () => ({ body: [unas, b2, plain] }),
    'GET /api/v1/sources': () => ({ body: [] }),
    'GET /api/v1/notifications': () => ({ body: [] }),
    'GET /api/v1/integrations/7/index': () => ({ body: { status: 'never', refreshedAt: null, attemptedAt: null, error: null, appVersion: '', stats: {}, fresh: false, instanceMatches: true, staleAfterHours: 24 } }),
    ...extra,
  };
}

describe('Plex backup targets', () => {
  it('adds an off-site target with the password, and shows the server’s refusal on its row', async () => {
    let puts = 0;
    const { calls, user } = renderApp(
      '/settings/plex',
      routes(plexIntegration(), {
        'PUT /api/v1/integrations/3': () =>
          ++puts === 1
            ? { status: 400, body: { message: "this backup holds Plex's credentials; use an encrypted destination (backup.targets[1], destination 12)" } }
            : { body: plexIntegration() },
      }),
    );
    await user.click(within(await screen.findByRole('article', { name: 'Plex' })).getByRole('button', { name: 'Edit Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    expect(form.getByLabelText('Destination')).toHaveDisplayValue('UNAS');
    expect(form.queryByLabelText('Your Bunkarr password')).not.toBeInTheDocument();

    await user.click(form.getByRole('button', { name: 'Add a backup target' }));
    const row = within(form.getByRole('group', { name: 'Backup target 2' }));
    // The list keeps its items: the group is inside the list item.
    expect(within(form.getByRole('list', { name: 'Backup targets' })).getAllByRole('listitem')).toHaveLength(1);
    await user.selectOptions(row.getByLabelText('Destination of target 2'), 'Plain S3');
    expect(row.getByText(/not encrypted: the backup holds Plex's credentials/)).toBeInTheDocument();
    // A target off this server needs the password (S29).
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Enter your Bunkarr password to confirm the backup target/)).toBeInTheDocument();
    await user.type(form.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await row.findByRole('alert')).toHaveTextContent('use an encrypted destination (backup.targets[1], destination 12)');

    await user.selectOptions(row.getByLabelText('Destination of target 2'), 'B2');
    await user.selectOptions(row.getByLabelText('Schedule of target 2'), 'Weekly (Sunday 06:00)');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(2));
    const body = callsTo(calls, 'PUT /api/v1/integrations/3')[1].body as IntegrationInput;
    expect(body.currentPassword).toBe(LOGIN);
    expect(body.settings.backup).toEqual({
      destinationId: 1,
      cron: '0 6 * * *',
      enabled: true,
      targets: [
        { destinationId: 1, cron: '0 6 * * *', enabled: true, acceptInsecureModes: false },
        { destinationId: 11, cron: '0 6 * * 0', enabled: true, acceptInsecureModes: false },
      ],
    });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('sends the targets list when a target is removed, and an empty one for None', async () => {
    const it = plexIntegration({
      settings: {
        dataPath: '/plex',
        pathMappings: [],
        backup: {
          destinationId: 1,
          cron: '0 6 * * *',
          enabled: true,
          targets: [
            { destinationId: 1, cron: '0 6 * * *', enabled: true, acceptInsecureModes: false },
            { destinationId: 11, cron: '0 6 * * 0', enabled: true, acceptInsecureModes: false },
          ],
        },
      },
    });
    const { calls, user } = renderApp('/settings/plex', routes(it, { 'PUT /api/v1/integrations/3': () => ({ body: it }) }));
    await user.click(within(await screen.findByRole('article', { name: 'Plex' })).getByRole('button', { name: 'Edit Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    await user.click(form.getByRole('button', { name: 'Remove backup target 2' }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(1));
    expect((callsTo(calls, 'PUT /api/v1/integrations/3')[0].body as IntegrationInput).settings.backup).toEqual({
      destinationId: 1,
      cron: '0 6 * * *',
      enabled: true,
      targets: [{ destinationId: 1, cron: '0 6 * * *', enabled: true, acceptInsecureModes: false }],
    });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

    // None: no Plex DB backup goes anywhere (the stored B2 target must not become the first).
    await user.click(within(await screen.findByRole('article', { name: 'Plex' })).getByRole('button', { name: 'Edit Plex' }));
    const again = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    await user.selectOptions(again.getByLabelText('Destination'), 'None (no database backup)');
    await user.click(again.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(2));
    expect((callsTo(calls, 'PUT /api/v1/integrations/3')[1].body as IntegrationInput).settings.backup).toMatchObject({ destinationId: 0, enabled: false, targets: [] });
  });

  it('lists every target on the card and backs up to each', async () => {
    const it = plexIntegration({
      settings: {
        dataPath: '/plex',
        pathMappings: [],
        backup: {
          destinationId: 1,
          cron: '0 6 * * *',
          enabled: true,
          targets: [
            { destinationId: 1, cron: '0 6 * * *', enabled: true, acceptInsecureModes: false },
            { destinationId: 11, cron: '0 6 * * 0', enabled: false, acceptInsecureModes: false },
          ],
        },
      },
    });
    const { calls, user } = renderApp(
      '/settings/plex',
      routes(it, {
        'POST /api/v1/integrations/3/plex/backup': () => ({ status: 202, body: { id: 60, type: 'plexdb_backup', status: 'queued', params: {} } }),
        'GET /api/v1/jobs/60': () => ({ body: { id: 60, type: 'plexdb_backup', status: 'queued', params: {}, progress: {}, stats: null, queuedAt: new Date().toISOString() } }),
        'GET /api/v1/jobs/60/items/summary': () => ({ body: [] }),
        'GET /api/v1/jobs/60/items': () => ({ body: { page: 1, pageSize: 50, totalRecords: 0, records: [] } }),
        'GET /api/v1/jobs/60/logs': () => ({ body: [] }),
      }),
    );
    const card = within(await screen.findByRole('article', { name: 'Plex' }));
    expect(await card.findByText(/to UNAS, Daily at 06:00/)).toBeInTheDocument();
    expect(card.getByText('to B2, manual only')).toBeInTheDocument();
    await user.click(card.getByRole('button', { name: 'Back up to B2' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/integrations/3/plex/backup')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/integrations/3/plex/backup')[0].body).toEqual({ destinationId: 11, dryRun: false });
  });
});

describe('*arr backup targets', () => {
  function radarr(backup: Record<string, unknown>): Integration {
    return {
      id: 7,
      type: 'radarr',
      name: 'Radarr',
      url: 'http://radarr:7878',
      enabled: true,
      hasApiKey: true,
      settings: {
        pathMappings: [{ arr: '/movies', local: '/media/movies' }],
        backupFolder: '/arr/radarr-backups',
        backup: { destinationId: 1, cron: '30 6 * * 0', enabled: true, maxScheduledAgeDays: 7, acceptInsecureModes: false, ...backup },
        refresh: { cron: '15 */6 * * *', enabled: true, staleAfterHours: 24 },
      } as unknown as Integration['settings'],
      createdAt: '2026-09-01T10:00:00Z',
      updatedAt: '2026-09-01T10:00:00Z',
    };
  }

  it('keeps stored targets without asking for the password, and removes one', async () => {
    const stored = radarr({
      targets: [
        { destinationId: 1, cron: '30 6 * * 0', enabled: true, acceptInsecureModes: false },
        { destinationId: 11, cron: '30 6 1 * *', enabled: true, acceptInsecureModes: false },
      ],
    });
    const { calls, user } = renderApp('/settings/connect', routes(stored, { 'PUT /api/v1/integrations/7': () => ({ body: stored }) }));
    const card = within(await screen.findByRole('article', { name: 'Radarr' }));
    expect(await card.findByText('and to B2, 30 6 1 * *')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    expect(within(form.getByRole('group', { name: 'Backup target 2' })).getByLabelText('Destination of target 2')).toHaveDisplayValue('B2');
    expect(form.queryByLabelText('Your Bunkarr password')).not.toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/7')).toHaveLength(1));
    const first = callsTo(calls, 'PUT /api/v1/integrations/7')[0].body as ArrIntegrationInput & { currentPassword?: string };
    expect((first.settings.backup as { targets: unknown[] }).targets).toHaveLength(2);
    expect(first).not.toHaveProperty('currentPassword');

    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const again = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await user.click(again.getByRole('button', { name: 'Remove backup target 2' }));
    await user.click(again.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/7')).toHaveLength(2));
    const second = callsTo(calls, 'PUT /api/v1/integrations/7')[1].body as ArrIntegrationInput;
    // One target left: the targets list says so (the single form alone would keep the stored
    // target 2 on the server, which merges it into the stored targets[1:]).
    expect(second.settings.backup).toEqual({
      destinationId: 1,
      cron: '30 6 * * 0',
      enabled: true,
      maxScheduledAgeDays: 7,
      acceptInsecureModes: false,
      targets: [{ destinationId: 1, cron: '30 6 * * 0', enabled: true, acceptInsecureModes: false }],
    });

    // "None" turns every target off: an empty list, not the single form without a destination
    // (which the server would merge into the stored target 2, now the first).
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const third = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await user.selectOptions(third.getByLabelText('Destination'), 'None (no backup)');
    await user.click(third.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/7')).toHaveLength(3));
    expect((callsTo(calls, 'PUT /api/v1/integrations/7')[2].body as ArrIntegrationInput).settings.backup).toMatchObject({ destinationId: 0, targets: [] });
  });
});

describe('target helpers', () => {
  const t = (destinationId: number, over = {}) => ({ destinationId, cron: '0 6 * * *', enabled: true, acceptInsecureModes: false, ...over });

  it('reads either form', () => {
    expect(targetsOf({ destinationId: 1, cron: '0 6 * * *', enabled: true })).toEqual([t(1)]);
    expect(targetsOf({ destinationId: 0, cron: '', enabled: false })).toEqual([]);
    expect(targetsOf({ destinationId: 1, cron: '', enabled: false, targets: [t(1), t(2)] })).toHaveLength(2);
  });

  it('checks targets as the server does', () => {
    expect(targetsProblem([t(1), t(2)], validateCron)).toBeNull();
    expect(targetsProblem([t(1), t(1)], validateCron)).toMatch(/a target already/);
    expect(targetsProblem([t(1), t(2), t(3), t(4), t(5)], validateCron)).toMatch(/At most 4/);
    expect(targetsProblem([t(1), t(2, { cron: 'nope' })], validateCron)).toMatch(/Target 2: the schedule is invalid/);
    expect(targetsProblem([t(0)], validateCron)).toMatch(/choose a destination/);
  });

  it('asks for the password for a new off-site target or new insecure modes only', () => {
    const list = [unas, b2];
    expect(targetsNeedPassword([t(1)], [], list)).toBe(false);
    expect(targetsNeedPassword([t(1), t(11)], [t(1)], list)).toBe(true);
    expect(targetsNeedPassword([t(1), t(11)], [t(1), t(11)], list)).toBe(false);
    expect(targetsNeedPassword([t(1, { acceptInsecureModes: true })], [t(1)], list)).toBe(true);
    expect(targetsNeedPassword([t(1, { acceptInsecureModes: true })], [t(1, { acceptInsecureModes: true })], list)).toBe(false);
    expect(targetIndexOf('… (backup.targets[2], destination 5)')).toBe(2);
    expect(targetIndexOf('something else')).toBe(-1);
  });
});
