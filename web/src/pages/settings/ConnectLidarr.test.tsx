import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { ArrIntegrationInput, ArrTestInput, IndexView } from '@/api/arr';
import type { Integration } from '@/api/types';
import { callsTo, type Handler } from '@/test/fetch';
import { source } from '@/test/fixtures';
import { renderApp } from '@/test/render';

// Lidarr in Settings → Connect (design D2, §16): its own examples, triggers and webhook URL, and
// what Lidarr sends no webhook for.

const KEY = '0123456789abcdef0123456789abcdef';

function lidarr(over: Partial<Integration> = {}): Integration {
  return {
    id: 9,
    type: 'lidarr',
    name: 'Lidarr',
    url: 'http://lidarr:8686',
    enabled: true,
    hasApiKey: true,
    settings: {
      pathMappings: [{ arr: '/music', local: '/media/music' }],
      backupFolder: '',
      backup: { destinationId: 0, cron: '', enabled: false, maxScheduledAgeDays: 7, acceptInsecureModes: false },
      refresh: { cron: '15 */6 * * *', enabled: true, staleAfterHours: 24 },
    } as unknown as Integration['settings'],
    createdAt: '2026-09-01T10:00:00Z',
    updatedAt: '2026-09-01T10:00:00Z',
    ...over,
  };
}

function index(): IndexView {
  return {
    status: 'ok',
    refreshedAt: new Date(Date.now() - 3600_000).toISOString(),
    attemptedAt: new Date(Date.now() - 3600_000).toISOString(),
    error: null,
    appVersion: '3.1.0.4875',
    stats: { items: 12, files: 19, filesMapped: 19, filesUnmapped: 0, filesMismatched: 0 },
    fresh: true,
    instanceMatches: true,
    staleAfterHours: 24,
  };
}

function routes(extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/integrations': () => ({ body: [lidarr()] }),
    'GET /api/v1/notifications': () => ({ body: [] }),
    'GET /api/v1/sources': () => ({ body: [source({ id: 1, name: 'Music', path: '/media/music' })] }),
    'GET /api/v1/integrations/9/index': () => ({ body: index() }),
    ...extra,
  };
}

describe('Settings → Connect → Lidarr', () => {
  it('shows the artists of the index', async () => {
    renderApp('/settings/connect', routes());
    const card = within(await screen.findByRole('article', { name: 'Lidarr' }));
    expect(card.getByText('/music → /media/music')).toBeInTheDocument();
    expect(await card.findByText('12 artists, 19 files')).toBeInTheDocument();
  });

  it('adds a Lidarr with music examples and tests its unsaved mappings', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations': () => ({ body: [] }),
        'POST /api/v1/integrations/test': () => ({
          body: {
            ok: true,
            message: 'Connected to Lidarr 3.1.0.4875.',
            rootFolders: [{ path: '/music', accessible: true, localPath: '/media/music', sourceId: 1, exists: true, reason: '' }],
            recycleBin: null,
            fileDate: 'albumReleaseDate',
          },
        }),
        'POST /api/v1/integrations': () => ({ status: 201, body: lidarr() }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Add Lidarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Add Lidarr' }));
    expect(form.getByLabelText('URL')).toHaveAttribute('placeholder', 'http://lidarr:8686');
    expect(form.getByLabelText('Lidarr path 1')).toHaveAttribute('placeholder', '/music');
    expect(form.getByText(/for example \/music → \/media\/music/)).toBeInTheDocument();
    expect(form.getByText(/index of Lidarr's artists and files/)).toBeInTheDocument();

    await user.type(form.getByLabelText('URL'), 'http://lidarr:8686');
    await user.type(form.getByLabelText(/^API key/), KEY);
    await user.type(form.getByLabelText('Lidarr path 1'), '/music');
    await user.type(form.getByLabelText('Bunkarr path 1'), '/media/music');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Connected to Lidarr 3.1.0.4875.')).toBeInTheDocument();
    expect(form.getByText(/Change File Date: albumReleaseDate/)).toBeInTheDocument();
    const test = callsTo(calls, 'POST /api/v1/integrations/test')[0].body as ArrTestInput;
    expect(test).toEqual({ type: 'lidarr', url: 'http://lidarr:8686', apiKey: KEY, settings: { pathMappings: [{ arr: '/music', local: '/media/music' }] } });

    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/integrations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/integrations')[0].body as ArrIntegrationInput;
    expect(body).toMatchObject({ type: 'lidarr', name: 'Lidarr', url: 'http://lidarr:8686', apiKey: KEY });
    expect(body.settings.pathMappings).toEqual([{ arr: '/music', local: '/media/music' }]);
  });

  it("shows Lidarr's webhook: its URL, its triggers and what it sends no webhook for", async () => {
    const { user } = renderApp('/settings/connect', routes());
    await user.click(await screen.findByRole('button', { name: 'Edit Lidarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Lidarr · Lidarr' }));
    await waitFor(() => expect(form.getByTestId('webhook-url')).toHaveTextContent(/\/api\/v1\/webhook\/lidarr\/9$/));
    const triggers = within(form.getByRole('list', { name: 'Triggers to tick' }));
    for (const t of ['On Release Import', 'On Upgrade', 'On Rename', 'On Track Retag', 'On Artist Add', 'On Album Delete', 'On Artist Delete']) {
      expect(triggers.getByText(t)).toBeInTheDocument();
    }
    expect(triggers.queryByText('On Import')).not.toBeInTheDocument();
    expect(form.getByText(/Lidarr sends no webhook for a manual import unless “Replace existing files” is ticked/)).toBeInTheDocument();
  });

  it('shows no Lidarr note for Radarr', async () => {
    const radarr = lidarr({ id: 7, type: 'radarr', name: 'Radarr', url: 'http://radarr:7878' });
    const { user } = renderApp(
      '/settings/connect',
      routes({ 'GET /api/v1/integrations': () => ({ body: [radarr] }), 'GET /api/v1/integrations/7/index': () => ({ body: index() }) }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    await waitFor(() => expect(form.getByTestId('webhook-url')).toHaveTextContent(/\/api\/v1\/webhook\/radarr\/7$/));
    expect(form.queryByText(/sends no webhook/)).not.toBeInTheDocument();
    expect(form.getByLabelText('Radarr path 1')).toHaveAttribute('placeholder', '/movies');
  });
});
