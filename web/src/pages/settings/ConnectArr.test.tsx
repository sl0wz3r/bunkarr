import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ArrIntegrationInput, ArrTestInput, IndexView } from '@/api/arr';
import type { Integration } from '@/api/types';
import { callsTo, type Handler } from '@/test/fetch';
import { source } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { webhookBaseProblem } from './ConnectArr';

const KEY = '0123456789abcdef0123456789abcdef';

function radarr(over: Partial<Integration> = {}): Integration {
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
      backup: { destinationId: 2, cron: '30 6 * * 0', enabled: true, maxScheduledAgeDays: 7, acceptInsecureModes: false },
      refresh: { cron: '15 */6 * * *', enabled: true, staleAfterHours: 24 },
    } as unknown as Integration['settings'],
    createdAt: '2026-09-01T10:00:00Z',
    updatedAt: '2026-09-01T10:00:00Z',
    ...over,
  };
}

function index(over: Partial<IndexView> = {}): IndexView {
  return {
    status: 'ok',
    refreshedAt: new Date(Date.now() - 2 * 3600_000).toISOString(),
    attemptedAt: new Date(Date.now() - 2 * 3600_000).toISOString(),
    error: null,
    appVersion: '6.4.4.10685',
    stats: { items: 4, files: 3, filesMapped: 2, filesUnmapped: 1, filesMismatched: 0 },
    fresh: true,
    instanceMatches: true,
    staleAfterHours: 24,
    ...over,
  };
}

function routes(extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/integrations': () => ({ body: [radarr()] }),
    'GET /api/v1/notifications': () => ({ body: [] }),
    'GET /api/v1/sources': () => ({ body: [source({ id: 1, name: 'Movies', path: '/media/movies' })] }),
    'GET /api/v1/integrations/7/index': () => ({ body: index() }),
    ...extra,
  };
}

describe('Settings → Connect → *arr', () => {
  it('shows each connection with its index status and refreshes it', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'POST /api/v1/integrations/7/refresh': () => ({ status: 202, body: { id: 55, type: 'refresh', status: 'queued' } }),
        'GET /api/v1/catalog/unmapped': () => ({
          body: { page: 1, pageSize: 50, totalRecords: 1, records: [{ integrationId: 7, path: '/movies-4k/Nosferatu (1922)/n.mkv', size: 10, reason: 'unmapped' }] },
        }),
      }),
    );
    const card = within(await screen.findByRole('article', { name: 'Radarr' }));
    expect(card.getByText('/movies → /media/movies')).toBeInTheDocument();
    expect(await card.findByText('Fresh')).toBeInTheDocument();
    expect(card.getByText('4 movies, 3 files')).toBeInTheDocument();
    expect(card.getByText(/1 unmapped, 0 mismatched/)).toBeInTheDocument();

    await user.click(card.getByRole('button', { name: 'Show' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Unmapped files · Radarr' }));
    expect(await dialog.findByText('/movies-4k/Nosferatu (1922)/n.mkv')).toBeInTheDocument();
    expect(dialog.getByText(/No path mapping/)).toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/catalog/unmapped')[0].query.get('integrationId')).toBe('7');
    await user.click(dialog.getByRole('button', { name: 'Close' }));

    await user.click(card.getByRole('button', { name: 'Refresh now' }));
    expect(await card.findByText(/Refresh queued/)).toBeInTheDocument();
    expect(card.getByRole('link', { name: 'Open the job' })).toHaveAttribute('href', '/activity/jobs/55');
    expect(callsTo(calls, 'POST /api/v1/integrations/7/refresh')).toHaveLength(1);
  });

  it('shows a stale index with its reason', async () => {
    renderApp('/settings/connect', routes({ 'GET /api/v1/integrations/7/index': () => ({ body: index({ fresh: false, reason: 'Radarr cache is 31 h old (stale after 24 h)' }) }) }));
    const card = within(await screen.findByRole('article', { name: 'Radarr' }));
    expect(await card.findByText('Stale')).toBeInTheDocument();
    expect(card.getByText(/Radarr cache is 31 h old/)).toBeInTheDocument();
  });

  it('adds a Radarr: tests unsaved mappings and saves with the key and the refresh settings', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations': () => ({ body: [] }),
        'POST /api/v1/integrations/test': () => ({
          body: {
            ok: true,
            message: 'Connected to Radarr 6.4.4.10685.',
            rootFolders: [
              { path: '/movies', accessible: true, localPath: '/media/movies', sourceId: 1, exists: true, reason: '' },
              { path: '/movies-4k', accessible: false, localPath: null, sourceId: null, exists: false, reason: 'no path mapping covers this folder' },
            ],
            recycleBin: null,
            fileDate: 'cinemas',
          },
        }),
        'POST /api/v1/integrations': () => ({ status: 201, body: radarr() }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Add Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Add Radarr' }));
    await user.type(form.getByLabelText('URL'), 'http://radarr:7878');
    await user.type(form.getByLabelText(/^API key/), KEY);
    await user.type(form.getByLabelText('Radarr path 1'), '/movies');
    await user.type(form.getByLabelText('Bunkarr path 1'), '/media/movies');
    await user.click(form.getByRole('button', { name: 'Test' }));

    const table = within(await form.findByRole('table', { name: 'Root folders' }));
    expect(table.getByText('Movies')).toBeInTheDocument();
    expect(table.getByText('Not accessible')).toBeInTheDocument();
    expect(table.getByText('no path mapping covers this folder')).toBeInTheDocument();
    expect(form.getByText(/Change File Date: cinemas/)).toBeInTheDocument();
    const test = callsTo(calls, 'POST /api/v1/integrations/test')[0].body as ArrTestInput;
    expect(test).toEqual({ type: 'radarr', url: 'http://radarr:7878', apiKey: KEY, settings: { pathMappings: [{ arr: '/movies', local: '/media/movies' }] } });

    // Save first, then the webhook panel appears.
    expect(form.getByText(/Save first/)).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/integrations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/integrations')[0].body as ArrIntegrationInput;
    expect(body).toEqual({
      type: 'radarr',
      name: 'Radarr',
      url: 'http://radarr:7878',
      enabled: true,
      apiKey: KEY,
      settings: {
        pathMappings: [{ arr: '/movies', local: '/media/movies' }],
        refresh: { cron: '15 */6 * * *', enabled: true, staleAfterHours: 24 },
        // No backup chosen: the backup fields go with their defaults (ArrBackup.tsx).
        backupFolder: '',
        backup: { destinationId: 0, cron: '', enabled: false, maxScheduledAgeDays: 7, acceptInsecureModes: false },
      },
    });
  });

  it('keeps the stored key and the backup settings when editing', async () => {
    const { calls, user } = renderApp('/settings/connect', routes({ 'PUT /api/v1/integrations/7': () => ({ body: radarr() }) }));
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    expect(form.getByLabelText(/^API key/)).toHaveAttribute('placeholder', 'Stored. Leave empty to keep it; type to replace it.');
    const stale = form.getByLabelText('Stale after');
    await user.clear(stale);
    await user.type(stale, '48');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/7')).toHaveLength(1));
    const body = callsTo(calls, 'PUT /api/v1/integrations/7')[0].body as ArrIntegrationInput;
    expect(body).not.toHaveProperty('apiKey');
    expect(body.settings.refresh).toEqual({ cron: '15 */6 * * *', enabled: true, staleAfterHours: 48 });
    expect(body.settings.backupFolder).toBe('/arr/radarr-backups');
    expect(body.settings.backup).toMatchObject({ destinationId: 2, enabled: true });

    // An out-of-range value is refused before anything is sent.
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const again = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await user.clear(again.getByLabelText('Stale after'));
    await user.type(again.getByLabelText('Stale after'), '1000');
    await user.click(again.getByRole('button', { name: 'Save' }));
    expect(await again.findByText('"Stale after" must be 1 to 720 hours.')).toBeInTheDocument();
    expect(callsTo(calls, 'PUT /api/v1/integrations/7')).toHaveLength(1);
  });

  it('renders the webhook panel from GET /integrations/{id}/webhook, and Show key calls POST …/webhook/key', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations/7/webhook': () => ({
          body: {
            path: '/api/v1/webhook/radarr/7',
            genericPath: '/api/v1/webhook/radarr',
            hasKey: true,
            lastEventAt: new Date(Date.now() - 60_000).toISOString(),
            lastTestAt: new Date(Date.now() - 3600_000).toISOString(),
            last24h: 3,
            warnings: ['Destination NAS has no sync schedule.'],
            recent: [
              {
                id: 1,
                integrationId: 7,
                source: 'radarr',
                eventType: 'Download',
                class: 'download',
                receivedAt: new Date(Date.now() - 60_000).toISOString(),
                processedAt: null,
                outcome: 'queued',
                jobId: 12,
                truncated: false,
                summary: { title: 'Heat' },
              },
            ],
          },
        }),
        'POST /api/v1/integrations/7/webhook/key': (init) => {
          const rotate = (JSON.parse(String(init?.body)) as { rotate: boolean }).rotate;
          return { body: { key: rotate ? 'f'.repeat(32) : KEY } };
        },
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    expect(await form.findByText(/Last Test received: 1 h(our)? ago|Last Test received: /)).toBeInTheDocument();
    expect(form.getByTestId('webhook-url')).toHaveTextContent('/api/v1/webhook/radarr/7');
    expect(form.getByText('Destination NAS has no sync schedule.')).toBeInTheDocument();
    expect(within(form.getByRole('list', { name: 'Recent webhook events' })).getByText('Heat')).toBeInTheDocument();
    expect(within(form.getByRole('list', { name: 'Triggers to tick' })).getByText('On Import')).toBeInTheDocument();
    expect(form.queryByLabelText('Webhook key')).not.toBeInTheDocument();

    await user.click(form.getByRole('button', { name: 'Show key' }));
    expect(await form.findByLabelText('Webhook key')).toHaveValue(KEY);
    expect(callsTo(calls, 'POST /api/v1/integrations/7/webhook/key')[0].body).toEqual({ rotate: false });

    await user.click(form.getByRole('button', { name: 'Regenerate key' }));
    const confirm = within(await screen.findByRole('dialog', { name: 'Regenerate the webhook key' }));
    await user.click(confirm.getByRole('button', { name: 'Regenerate' }));
    await waitFor(() => expect(form.getByLabelText('Webhook key')).toHaveValue('f'.repeat(32)));
    expect(callsTo(calls, 'POST /api/v1/integrations/7/webhook/key')[1].body).toEqual({ rotate: true });
  });

  it('computes the webhook URL when the server has no webhook activity yet', async () => {
    const { user } = renderApp('/settings/connect', routes());
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await waitFor(() => expect(form.getByTestId('webhook-url')).toHaveTextContent(/\/api\/v1\/webhook\/radarr\/7$/));
    expect(form.queryByText(/Last Test received/)).not.toBeInTheDocument();
  });

  it('offers to exclude a recycle bin inside a source', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'POST /api/v1/integrations/test': () => ({
          body: { ok: true, message: 'Connected to Radarr 6.4.4.', rootFolders: [], recycleBin: { path: '/movies/.recycle', sourceId: 1, relPath: '.recycle', excluded: false } },
        }),
        'PUT /api/v1/sources/1': () => ({ body: source({ id: 1 }) }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await user.click(form.getByRole('button', { name: 'Test' }));
    const exclude = await form.findByRole('button', { name: 'Exclude /.recycle/ from Movies' });
    await user.click(exclude);
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/sources/1')).toHaveLength(1));
    const put = callsTo(calls, 'PUT /api/v1/sources/1')[0].body as { exclude: string[]; name: string };
    expect(put.name).toBe('Movies');
    expect(put.exclude).toContain('/.recycle/');
    expect(await form.findByText(/recycle bin is excluded from Movies/)).toBeInTheDocument();
    // The test never sent a key it did not have: the stored one is used server-side.
    expect(callsTo(calls, 'POST /api/v1/integrations/test')[0].body).toMatchObject({ id: 7 });
    expect(callsTo(calls, 'POST /api/v1/integrations/test')[0].body).not.toHaveProperty('apiKey');
  });
});

/** withExecCommand installs document.execCommand (jsdom has none) for one test. */
function withExecCommand(impl: (command: string) => boolean) {
  const fn = vi.fn(impl);
  Object.defineProperty(document, 'execCommand', { value: fn, configurable: true, writable: true });
  return fn;
}

afterEach(() => {
  Reflect.deleteProperty(document, 'execCommand');
});

function webhookInfo(over: Record<string, unknown> = {}) {
  return {
    path: '/api/v1/webhook/radarr/7',
    genericPath: '/api/v1/webhook/radarr',
    hasKey: true,
    lastEventAt: null,
    lastTestAt: null,
    last24h: 0,
    warnings: [],
    recent: [],
    ...over,
  };
}

describe('Settings → Connect → *arr (review fixes)', () => {
  it('offers "Apply held changes" when the refresh guard held removals, and sends allowChanges', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations/7/index': () => ({
          body: index({
            fresh: false,
            reason: 'Radarr cache is 31 h old (stale after 24 h)',
            error: 'Refresh guard held the removals: 900 of 1000 movies are gone',
            stats: { items: 100, files: 90, guardHeld: true },
          }),
        }),
        'POST /api/v1/integrations/7/refresh': () => ({ status: 202, body: { id: 77, type: 'refresh', status: 'queued' } }),
      }),
    );
    const card = within(await screen.findByRole('article', { name: 'Radarr' }));
    expect(await card.findByText(/Refresh guard held the removals: 900 of 1000/)).toBeInTheDocument();
    expect(card.getByText(/until a refresh applies the held changes/)).toBeInTheDocument();
    await user.click(card.getByRole('button', { name: 'Apply held changes' }));
    const confirm = within(await screen.findByRole('dialog', { name: 'Apply held changes · Radarr' }));
    expect(callsTo(calls, 'POST /api/v1/integrations/7/refresh')).toHaveLength(0);
    await user.click(confirm.getByRole('button', { name: 'Apply held changes' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/integrations/7/refresh')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/integrations/7/refresh')[0].body).toEqual({ allowChanges: true });
    expect(await card.findByRole('link', { name: 'Open the job' })).toHaveAttribute('href', '/activity/jobs/77');
  });

  it('shows no "Apply held changes" when nothing was held', async () => {
    renderApp('/settings/connect', routes());
    const card = within(await screen.findByRole('article', { name: 'Radarr' }));
    expect(await card.findByText('Fresh')).toBeInTheDocument();
    expect(card.queryByRole('button', { name: 'Apply held changes' })).not.toBeInTheDocument();
  });

  it('"Check again" re-reads the webhook activity, so a Test pressed in the *arr shows up', async () => {
    let tested: string | null = null;
    const { calls, user } = renderApp('/settings/connect', routes({ 'GET /api/v1/integrations/7/webhook': () => ({ body: webhookInfo({ lastTestAt: tested }) }) }));
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    expect(await form.findByText(/Last Test received: never/)).toBeInTheDocument();
    tested = new Date(Date.now() - 5_000).toISOString();
    await user.click(form.getByRole('button', { name: 'Check again' }));
    await waitFor(() => expect(form.queryByText(/Last Test received: never/)).not.toBeInTheDocument());
    expect(form.getByText(/Last Test received: /)).toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/integrations/7/webhook').length).toBeGreaterThanOrEqual(2);
  });

  it('hides a Test result once the URL, key or mappings it tested change', async () => {
    const { user } = renderApp(
      '/settings/connect',
      routes({
        'POST /api/v1/integrations/test': () => ({
          body: {
            ok: true,
            message: 'Connected to Radarr 6.4.4.10685.',
            rootFolders: [{ path: '/movies', accessible: true, localPath: '/media/movies', sourceId: 1, exists: true, reason: '' }],
            recycleBin: null,
            backup: { folder: 'ok', http: 'ok' },
            manualBackups: { count: 1, bytes: 1000 },
          },
        }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Connected to Radarr 6.4.4.10685.')).toBeInTheDocument();
    expect(form.getByRole('table', { name: 'Root folders' })).toBeInTheDocument();
    expect(form.getByRole('list', { name: 'Backup access' })).toBeInTheDocument();

    // Another API key: the result no longer speaks for what would be saved.
    await user.type(form.getByLabelText(/^API key/), KEY);
    expect(form.queryByText('Connected to Radarr 6.4.4.10685.')).not.toBeInTheDocument();
    expect(form.queryByRole('table', { name: 'Root folders' })).not.toBeInTheDocument();
    expect(form.queryByRole('list', { name: 'Backup access' })).not.toBeInTheDocument();

    // Tested again with that key, then a mapping changes.
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Connected to Radarr 6.4.4.10685.')).toBeInTheDocument();
    await user.type(form.getByLabelText('Radarr path 1'), '-4k');
    expect(form.queryByText('Connected to Radarr 6.4.4.10685.')).not.toBeInTheDocument();
  });

  it('copies the webhook URL over plain http (no navigator.clipboard) and says so', async () => {
    const { user } = renderApp('/settings/connect', routes());
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await waitFor(() => expect(form.getByTestId('webhook-url')).toHaveTextContent(/\/api\/v1\/webhook\/radarr\/7$/));
    vi.spyOn(navigator, 'clipboard', 'get').mockReturnValue(undefined as unknown as Clipboard);
    let copied = '';
    const exec = withExecCommand((cmd) => {
      copied = (document.activeElement as HTMLTextAreaElement | null)?.value ?? '';
      return cmd === 'copy';
    });
    const copy = form.getByRole('button', { name: 'Copy the webhook URL' });
    await user.click(copy);
    expect(exec).toHaveBeenCalledWith('copy');
    expect(copied).toBe(form.getByTestId('webhook-url').textContent);
    await waitFor(() => expect(copy).toHaveTextContent('Copied'));
    expect(within(copy.parentElement as HTMLElement).getByRole('status')).toHaveTextContent('Copied.');
  });

  it('asks to copy by hand when the browser refuses', async () => {
    const { user } = renderApp('/settings/connect', routes());
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    vi.spyOn(navigator, 'clipboard', 'get').mockReturnValue(undefined as unknown as Clipboard);
    withExecCommand(() => false);
    const copy = form.getByRole('button', { name: 'Copy the webhook URL' });
    await user.click(copy);
    await waitFor(() => expect(within(copy.parentElement as HTMLElement).getByRole('status')).toHaveTextContent(/copy it by hand/));
    expect(copy).toHaveTextContent('Copy');
  });

  it('warns about a loopback address in the webhook URL and lets the user change the host', async () => {
    const { user } = renderApp('/settings/connect', routes());
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    // The tests run at http://localhost:3000, as when Bunkarr is opened on the Docker host.
    expect(window.location.hostname).toBe('localhost');
    expect(await form.findByTestId('webhook-base-problem')).toHaveTextContent(/loopback/);
    const base = form.getByLabelText('Bunkarr address');
    await user.clear(base);
    await user.type(base, 'http://bunkarr:8787/');
    expect(form.getByTestId('webhook-url')).toHaveTextContent(/^http:\/\/bunkarr:8787\/api\/v1\/webhook\/radarr\/7$/);
    expect(form.queryByTestId('webhook-base-problem')).not.toBeInTheDocument();
  });

  it.each([
    ['http://localhost:8787', true],
    ['http://127.0.0.1:8787', true],
    ['http://[::1]:8787', true],
    ['http://app.localhost', true],
    ['http://192.168.1.10:8787', false],
    ['https://bunkarr.example.com', false],
    ['bunkarr:8787', true],
    ['ftp://bunkarr', true],
  ])('webhookBaseProblem(%s) → problem: %s', (base, problem) => {
    expect(webhookBaseProblem(base, 'Radarr') !== null).toBe(problem);
  });
});
