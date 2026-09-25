import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { IntegrationInput, Notification, NotificationInput } from '@/api/types';
import { callsTo } from '@/test/fetch';
import { destination, job, plexIntegration } from '@/test/fixtures';
import { renderApp } from '@/test/render';

const phone: Notification = {
  id: 4,
  name: 'Phone',
  kind: 'apprise',
  enabled: true,
  apiUrl: 'http://apprise:8000',
  configKey: '',
  hasUrls: true,
  onFailure: true,
  onWarning: true,
  onSuccess: false,
  createdAt: '2026-09-01T10:00:00Z',
  updatedAt: '2026-09-01T10:00:00Z',
};

describe('Secret fields after a URL change', () => {
  it('asks for the Apprise URLs again when the API URL changes', async () => {
    const { user } = renderApp('/settings/connect', {
      'GET /api/v1/notifications': () => ({ body: [phone] }),
    });
    await user.click(await screen.findByRole('button', { name: 'Edit Phone' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit notification · Phone' }));
    const urls = form.getByLabelText(/^Apprise URLs/);
    expect(urls).toHaveAttribute('placeholder', 'Stored. Leave empty to keep it; type to replace it.');
    const api = form.getByLabelText(/^Apprise API URL/);
    await user.clear(api);
    await user.type(api, 'http://other-apprise:8000');
    expect(urls).toHaveAttribute('placeholder', expect.stringMatching(/^Enter it again: the URL changed/));
  });
});

describe('Settings → Connect', () => {
  it('never shows stored URLs and only sends new ones', async () => {
    const { calls, user } = renderApp('/settings/connect', {
      'GET /api/v1/notifications': () => ({ body: [phone] }),
      'PUT /api/v1/notifications/4': () => ({ body: phone }),
      'POST /api/v1/notifications/test': () => ({ body: { ok: true, message: 'Sent.' } }),
    });

    expect(await screen.findByText('URLs stored')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Edit Phone' }));
    let form = within(await screen.findByRole('dialog', { name: 'Edit notification · Phone' }));
    const urls = form.getByLabelText(/^Apprise URLs/);
    expect(urls).toHaveValue('');
    expect(urls).toHaveAttribute('placeholder', 'Stored. Leave empty to keep it; type to replace it.');
    expect(form.getByText('Stored')).toBeInTheDocument();

    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Sent.')).toBeInTheDocument();
    const test = callsTo(calls, 'POST /api/v1/notifications/test')[0].body as NotificationInput & { id: number };
    expect(test.id).toBe(4);
    expect(test).not.toHaveProperty('urls');

    await user.click(form.getByRole('checkbox', { name: 'Successful jobs' }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/notifications/4')).toHaveLength(1));
    let body = callsTo(calls, 'PUT /api/v1/notifications/4')[0].body as NotificationInput;
    expect(body).not.toHaveProperty('urls');
    expect(body).toMatchObject({ name: 'Phone', apiUrl: 'http://apprise:8000', configKey: '', onSuccess: true });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

    await user.click(screen.getByRole('button', { name: 'Edit Phone' }));
    form = within(await screen.findByRole('dialog', { name: 'Edit notification · Phone' }));
    await user.type(form.getByLabelText(/^Apprise URLs/), 'tgram://bot/chat{enter}ntfy://ntfy.sh/bunkarr');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/notifications/4')).toHaveLength(2));
    body = callsTo(calls, 'PUT /api/v1/notifications/4')[1].body as NotificationInput;
    expect(body.urls).toBe('tgram://bot/chat,ntfy://ntfy.sh/bunkarr');
  });

  it('requires URLs or a config key for a new target', async () => {
    const { calls, user } = renderApp('/settings/connect', {
      'GET /api/v1/notifications': () => ({ body: [] }),
      'POST /api/v1/notifications': () => ({ status: 201, body: phone }),
    });
    await user.click((await screen.findAllByRole('button', { name: 'Add notification' }))[0]);
    const form = within(await screen.findByRole('dialog', { name: 'Add notification' }));
    await user.type(form.getByLabelText('Name'), 'Home');
    await user.type(form.getByLabelText('Apprise API URL'), 'http://apprise:8000');
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('Enter at least one Apprise URL.')).toBeInTheDocument();

    await user.click(form.getByRole('radio', { name: /Stateful/ }));
    expect(form.queryByLabelText(/^Apprise URLs/)).not.toBeInTheDocument();
    await user.type(form.getByLabelText('Configuration key'), 'bunkarr');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/notifications')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/notifications')[0].body).toEqual({
      name: 'Home',
      kind: 'apprise',
      enabled: true,
      apiUrl: 'http://apprise:8000',
      configKey: 'bunkarr',
      onFailure: true,
      onWarning: true,
      onSuccess: false,
    });
  });
});

describe('Settings → Plex', () => {
  const routes = {
    'GET /api/v1/integrations': () => ({ body: [plexIntegration()] }),
    'GET /api/v1/destinations': () => ({ body: [destination()] }),
  };

  it('keeps the stored token, edits path mappings and warns about the butler window', async () => {
    const { calls, user } = renderApp('/settings/plex', {
      ...routes,
      'PUT /api/v1/integrations/3': () => ({ body: plexIntegration() }),
      'POST /api/v1/integrations/test': () => ({ body: { ok: true, message: 'Connected to Plex.', version: '1.43.4', machineIdentifier: 'abc123' } }),
    });

    const card = within(await screen.findByRole('article', { name: 'Plex' }));
    expect(card.getByText('Token stored')).toBeInTheDocument();
    expect(card.getByText('/data → /media')).toBeInTheDocument();
    await user.click(card.getByRole('button', { name: 'Edit Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    const token = form.getByLabelText(/Token/);
    expect(token).toHaveValue('');
    expect(token).toHaveAttribute('type', 'password');

    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Connected to Plex.')).toBeInTheDocument();
    expect(form.getByText(/Version 1.43.4/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/integrations/test')[0].body).toEqual({ type: 'plex', url: 'http://plex:32400', id: 3 });

    await user.click(form.getByRole('button', { name: 'Add mapping' }));
    await user.type(form.getByLabelText('Plex path 2'), '/tv');
    await user.type(form.getByLabelText('Bunkarr path 2'), '/media/tv');

    const schedule = form.getByLabelText('Schedule');
    expect(schedule).toHaveDisplayValue('Daily at 06:00');
    expect(form.queryByText(/maintenance window/)).not.toBeInTheDocument();
    await user.selectOptions(schedule, 'Custom (cron)');
    const cron = form.getByLabelText('Schedule cron expression');
    await user.clear(cron);
    await user.type(cron, '0 3 * * *');
    expect(form.getByText(/maintenance window \(02:00–05:00, Plex default\)/)).toBeInTheDocument();

    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(1));
    const body = callsTo(calls, 'PUT /api/v1/integrations/3')[0].body as IntegrationInput;
    expect(body).not.toHaveProperty('apiKey');
    expect(body.settings).toEqual({
      dataPath: '/plex',
      pathMappings: [
        { plex: '/data', local: '/media' },
        { plex: '/tv', local: '/media/tv' },
      ],
      backup: { destinationId: 1, cron: '0 3 * * *', enabled: true },
    });
  });

  it('shows a test result only for the URL and token it tested', async () => {
    let tests = 0;
    const { user } = renderApp('/settings/plex', {
      ...routes,
      'POST /api/v1/integrations/test': () =>
        ++tests === 1
          ? { body: { ok: true, message: 'Connected to Plex.', version: '1.43.4', butlerStartHour: 6, butlerEndHour: 8 } }
          : { status: 502, body: { message: 'Plex did not answer' } },
    });
    await user.click(within(await screen.findByRole('article', { name: 'Plex' })).getByRole('button', { name: 'Edit Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Connected to Plex.')).toBeInTheDocument();
    // The tested server's butler window (06–08) contains the 06:00 backup.
    expect(form.getByText(/maintenance window \(06:00–08:00\)/)).toBeInTheDocument();

    // Another URL: that result says nothing about it, and the butler window is Plex's default again.
    await user.type(form.getByLabelText('URL'), '0');
    expect(form.queryByText('Connected to Plex.')).not.toBeInTheDocument();
    expect(form.queryByText(/maintenance window/)).not.toBeInTheDocument();
    await user.clear(form.getByLabelText('URL'));
    await user.type(form.getByLabelText('URL'), 'http://plex:32400');
    expect(await form.findByText('Connected to Plex.')).toBeInTheDocument();

    // A new test replaces the result: its failure is not shown next to the old success.
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Plex did not answer')).toBeInTheDocument();
    expect(form.queryByText('Connected to Plex.')).not.toBeInTheDocument();

    // A token typed after the test is not the one tested either.
    tests = 0;
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Connected to Plex.')).toBeInTheDocument();
    await user.type(form.getByLabelText(/Token/), 'new-token');
    expect(form.queryByText('Connected to Plex.')).not.toBeInTheDocument();
  });

  it('shows a test error only for the URL and token it tested', async () => {
    const { user } = renderApp('/settings/plex', {
      ...routes,
      'POST /api/v1/integrations/test': () => ({ status: 400, body: { message: 'enter the Plex token to test a different URL' } }),
    });
    await user.click(within(await screen.findByRole('article', { name: 'Plex' })).getByRole('button', { name: 'Edit Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    await user.type(form.getByLabelText('URL'), '0');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('enter the Plex token to test a different URL')).toBeInTheDocument();

    // The token it asks for: the error was about the request without it.
    await user.type(form.getByLabelText(/Token/), 'new-token');
    expect(form.queryByText('enter the Plex token to test a different URL')).not.toBeInTheDocument();
    await user.clear(form.getByLabelText(/Token/));
    expect(form.getByText('enter the Plex token to test a different URL')).toBeInTheDocument();
    // Back to the stored URL.
    await user.type(form.getByLabelText('URL'), '{Backspace}');
    expect(form.queryByText('enter the Plex token to test a different URL')).not.toBeInTheDocument();
  });

  it('requires a token for a new server and sends it once', async () => {
    const { calls, user } = renderApp('/settings/plex', {
      'GET /api/v1/integrations': () => ({ body: [] }),
      'GET /api/v1/destinations': () => ({ body: [destination()] }),
      'POST /api/v1/integrations': () => ({ status: 201, body: plexIntegration() }),
    });
    await user.click((await screen.findAllByRole('button', { name: 'Add Plex server' }))[0]);
    const form = within(await screen.findByRole('dialog', { name: 'Add Plex server' }));
    await user.type(form.getByLabelText('URL'), 'http://plex:32400');
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('Enter the Plex token.')).toBeInTheDocument();
    await user.type(form.getByLabelText(/Token/), 'secret-token');
    await user.type(form.getByLabelText('Data path'), '/plex');
    await user.selectOptions(form.getByLabelText('Destination'), 'UNAS');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/integrations')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/integrations')[0].body).toEqual({
      type: 'plex',
      name: 'Plex',
      url: 'http://plex:32400',
      enabled: true,
      apiKey: 'secret-token',
      settings: { dataPath: '/plex', pathMappings: [], backup: { destinationId: 1, cron: '0 6 * * *', enabled: true } },
    });
  });

  it('asks for the data path before scheduling database backups', async () => {
    const { calls, user } = renderApp('/settings/plex', {
      'GET /api/v1/integrations': () => ({ body: [] }),
      'GET /api/v1/destinations': () => ({ body: [destination()] }),
      'POST /api/v1/integrations': () => ({ status: 201, body: plexIntegration() }),
    });
    await user.click((await screen.findAllByRole('button', { name: 'Add Plex server' }))[0]);
    const form = within(await screen.findByRole('dialog', { name: 'Add Plex server' }));
    await user.type(form.getByLabelText('URL'), 'http://plex:32400');
    await user.type(form.getByLabelText(/Token/), 'secret-token');
    await user.selectOptions(form.getByLabelText('Destination'), 'UNAS');
    await user.click(form.getByRole('button', { name: 'Save' }));
    // The server would refuse it ("scheduled Plex DB backups need the Plex data path (dataPath)").
    expect(await form.findByText(/Set the data path/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/integrations')).toHaveLength(0);

    await user.selectOptions(form.getByLabelText('Schedule'), 'Manual only (disabled)');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/integrations')).toHaveLength(1));
    expect((callsTo(calls, 'POST /api/v1/integrations')[0].body as IntegrationInput).settings.backup).toEqual({ destinationId: 1, cron: '0 6 * * *', enabled: false });
  });

  it('does not send an invalid backup cron that is not in use', async () => {
    const { calls, user } = renderApp('/settings/plex', { ...routes, 'PUT /api/v1/integrations/3': () => ({ body: plexIntegration() }) });
    await user.click(within(await screen.findByRole('article', { name: 'Plex' })).getByRole('button', { name: 'Edit Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    await user.selectOptions(form.getByLabelText('Schedule'), 'Custom (cron)');
    const cron = form.getByLabelText('Schedule cron expression');
    await user.clear(cron);
    await user.type(cron, '0 25 * * *');
    // No destination: the schedule field goes away, and so must its invalid cron (the server
    // checks backup.cron even without a destination).
    await user.selectOptions(form.getByLabelText('Destination'), 'None (no database backup)');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(1));
    expect((callsTo(calls, 'PUT /api/v1/integrations/3')[0].body as IntegrationInput).settings.backup).toEqual({ destinationId: 0, cron: '', enabled: false });
  });

  it('backs up now and opens the job', async () => {
    const backup = job({ id: 50, type: 'plexdb_backup', status: 'queued', params: { integrationId: 3, destinationId: 1 } });
    const { calls, user } = renderApp('/settings/plex', {
      ...routes,
      'POST /api/v1/integrations/3/plex/backup': () => ({ status: 202, body: backup }),
      'GET /api/v1/jobs/50': () => ({ body: backup }),
      'GET /api/v1/jobs/50/items/summary': () => ({ body: [] }),
      'GET /api/v1/jobs/50/items': () => ({ body: { page: 1, pageSize: 50, totalRecords: 0, records: [] } }),
      'GET /api/v1/jobs/50/logs': () => ({ body: [] }),
    });
    await user.click(await screen.findByRole('button', { name: 'Back up now' }));
    expect(await screen.findByRole('heading', { name: 'Plex DB backup #50' })).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/integrations/3/plex/backup')[0].body).toEqual({ destinationId: 1, dryRun: false });
    expect(screen.getByText('Plex → UNAS')).toBeInTheDocument();
  });
});
