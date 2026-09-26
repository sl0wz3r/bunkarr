import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { ArrIntegrationInput, ArrTestInput } from '@/api/arr';
import type { Integration, Snapshot } from '@/api/types';
import { callsTo, type Handler } from '@/test/fetch';
import { destination } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { arrBackupSettings, arrBackupValue, validateArrBackup } from './ArrBackup';

function radarr(backup: Record<string, unknown> = {}, over: Partial<Integration> = {}): Integration {
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
    ...over,
  };
}

const smb = destination({
  id: 2,
  name: 'SMB share',
  capabilities: { ...destination().capabilities!, enforcesModes: false },
});

function routes(it: Integration, extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/integrations': () => ({ body: [it] }),
    'GET /api/v1/notifications': () => ({ body: [] }),
    'GET /api/v1/sources': () => ({ body: [] }),
    'GET /api/v1/destinations': () => ({ body: [destination({ capabilities: { ...destination().capabilities!, enforcesModes: true } }), smb] }),
    'GET /api/v1/integrations/7/index': () => ({ body: { status: 'never', refreshedAt: null, attemptedAt: null, error: null, appVersion: '', stats: {}, fresh: false, instanceMatches: true, staleAfterHours: 24 } }),
    ...extra,
  };
}

const snapshot: Snapshot = {
  id: 3,
  destinationId: 1,
  kind: 'arr',
  integrationId: 7,
  jobId: 12,
  path: '.bunkarr/arr/radarr-7/20260925T063000Z',
  createdAt: '2026-09-25T06:30:00Z',
  size: 26388,
  method: 'arr_api_folder',
  integrity: 'ok',
  manifest: { backup: { name: 'radarr_backup_v6.4.4.10685_2026.09.21_06.00.00.zip', type: 'scheduled' }, sensitive: true },
};

describe('*arr backups on Settings → Connect', () => {
  it('shows the backup on the card, backs up now and lists the versions without a download', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes(radarr(), {
        'POST /api/v1/integrations/7/arr/backup': () => ({ status: 202, body: { id: 91, type: 'arr_backup', status: 'queued' } }),
        'GET /api/v1/integrations/7/arr/snapshots': () => ({ body: [snapshot] }),
      }),
    );
    const card = within(await screen.findByRole('article', { name: 'Radarr' }));
    expect(await card.findByText(/to UNAS, .*from \/arr\/radarr-backups/)).toBeInTheDocument();

    await user.click(card.getByRole('button', { name: 'Back up now' }));
    expect(await card.findByText(/Backup queued/)).toBeInTheDocument();
    expect(card.getByRole('link', { name: 'Open the job' })).toHaveAttribute('href', '/activity/jobs/91');
    expect(callsTo(calls, 'POST /api/v1/integrations/7/arr/backup')).toHaveLength(1);

    await user.click(card.getByRole('button', { name: 'Backups' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Radarr backups · Radarr' }));
    expect(await dialog.findByText('radarr_backup_v6.4.4.10685_2026.09.21_06.00.00.zip')).toBeInTheDocument();
    expect(dialog.getByText(/its scheduled backup · from the Backups folder/)).toBeInTheDocument();
    expect(dialog.getByText('UNAS')).toBeInTheDocument();
    expect(dialog.queryByRole('link', { name: /download/i })).not.toBeInTheDocument();
    expect(dialog.queryByRole('button', { name: /download/i })).not.toBeInTheDocument();
  });

  it('cannot back up without a destination', async () => {
    renderApp('/settings/connect', routes(radarr({ destinationId: 0, enabled: false, cron: '' })));
    const card = within(await screen.findByRole('article', { name: 'Radarr' }));
    expect(await card.findByText('not set up')).toBeInTheDocument();
    expect(card.getByRole('button', { name: 'Back up now' })).toBeDisabled();
  });

  it('edits the backup settings, tests the folder and asks to accept insecure modes on an SMB destination', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes(radarr({ destinationId: 0, enabled: false, cron: '' }, { settings: { ...radarr().settings, backupFolder: '' } as unknown as Integration['settings'] }), {
        'POST /api/v1/integrations/test': () => ({
          body: {
            ok: true,
            message: 'Connected to Radarr 6.4.4.10685.',
            rootFolders: [],
            recycleBin: null,
            backup: { folder: 'ok', http: 'login-required' },
            manualBackups: { count: 12, bytes: 12 * 26388 },
          },
        }),
        'PUT /api/v1/integrations/7': () => ({ body: radarr() }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await user.type(form.getByLabelText('Backups folder'), '/arr/radarr-backups');
    await user.click(form.getByRole('button', { name: 'Test' }));
    const access = within(await form.findByRole('list', { name: 'Backup access' }));
    expect(access.getByText('readable')).toBeInTheDocument();
    expect(access.getByText(/needs a login/)).toBeInTheDocument();
    expect(form.getByText(/12 in Radarr/)).toHaveClass('text-warn');
    expect((callsTo(calls, 'POST /api/v1/integrations/test')[0].body as ArrTestInput).settings).toMatchObject({ backupFolder: '/arr/radarr-backups' });

    await user.selectOptions(form.getByLabelText('Destination'), 'SMB share');
    expect(await form.findByText(/does not keep files private/)).toBeInTheDocument();
    // Saving without the confirmation is left to the server (400 naming the flag); with it:
    await user.click(form.getByRole('checkbox', { name: /Accept insecure file modes/ }));
    const age = form.getByLabelText('Reuse scheduled backups');
    await user.clear(age);
    await user.type(age, '3');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/7')).toHaveLength(1));
    const body = callsTo(calls, 'PUT /api/v1/integrations/7')[0].body as ArrIntegrationInput;
    expect(body.settings.backupFolder).toBe('/arr/radarr-backups');
    expect(body.settings.backup).toEqual({ destinationId: 2, cron: '30 6 * * 0', enabled: true, maxScheduledAgeDays: 3, acceptInsecureModes: true });
  });

  it('refuses an out-of-range reuse age before anything is sent', async () => {
    const { calls, user } = renderApp('/settings/connect', routes(radarr()));
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    const age = await form.findByLabelText('Reuse scheduled backups');
    await user.clear(age);
    await user.type(age, '91');
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('"Reuse scheduled backups" must be 1 to 90 days.')).toBeInTheDocument();
    expect(callsTo(calls, 'PUT /api/v1/integrations/7')).toHaveLength(0);
  });
});

describe('arrBackupSettings', () => {
  it('round-trips the stored settings and clears the schedule without a destination', () => {
    const v = arrBackupValue(radarr({ cron: '0 3 * * *' }));
    expect(arrBackupSettings(v)).toEqual({
      backupFolder: '/arr/radarr-backups',
      backup: { destinationId: 1, cron: '0 3 * * *', enabled: true, maxScheduledAgeDays: 7, acceptInsecureModes: false },
    });
    expect(arrBackupSettings({ ...v, destinationId: 0 }).backup).toEqual({ destinationId: 0, cron: '', enabled: false, maxScheduledAgeDays: 7, acceptInsecureModes: false });
    // A new integration offers the weekly default once a destination is chosen.
    expect(arrBackupValue(null).schedule).toEqual({ cron: '30 6 * * 0', enabled: true });
  });

  it('validates the fields', () => {
    const v = arrBackupValue(radarr());
    expect(validateArrBackup(v)).toBeNull();
    expect(validateArrBackup({ ...v, backupFolder: 'relative/path' })).toMatch(/absolute path/);
    expect(validateArrBackup({ ...v, maxScheduledAgeDays: 0 })).toMatch(/1 to 90/);
    expect(validateArrBackup({ ...v, schedule: { cron: 'nonsense', enabled: true } })).toMatch(/schedule is invalid/);
    expect(validateArrBackup({ ...v, destinationId: 0, schedule: { cron: 'nonsense', enabled: true } })).toBeNull();
  });
});
