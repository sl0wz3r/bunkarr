import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { IndexView } from '@/api/arr';
import type { ProviderIntegrationInput, ProviderTestInput } from '@/api/providers';
import type { Integration } from '@/api/types';
import { callsTo, type Handler } from '@/test/fetch';
import { plexIntegration } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { slowPage } from '@/test/slow';

slowPage();

const TAUTULLI_KEY = '0123456789abcdef0123456789abcdef';

function tautulli(over: Partial<Integration> = {}): Integration {
  return {
    id: 11,
    type: 'tautulli',
    name: 'Tautulli',
    url: 'http://tautulli:8181',
    enabled: true,
    hasApiKey: true,
    settings: { plexIntegrationId: 3, refresh: { cron: '0 2 * * *', enabled: true, staleAfterHours: 72 } } as unknown as Integration['settings'],
    createdAt: '2026-09-01T10:00:00Z',
    updatedAt: '2026-09-01T10:00:00Z',
    ...over,
  };
}

function index(over: Partial<IndexView> = {}): IndexView {
  return {
    status: 'ok',
    refreshedAt: new Date(Date.now() - 3600_000).toISOString(),
    attemptedAt: new Date(Date.now() - 3600_000).toISOString(),
    error: null,
    appVersion: 'v2.18.1',
    stats: { plexIntegrationId: 3, plays: 13, ratingKeys: 12, sectionsWithoutHistory: [], usersWithoutHistory: 0 } as IndexView['stats'],
    fresh: true,
    instanceMatches: true,
    staleAfterHours: 72,
    ...over,
  };
}

const plexOn = () => plexIntegration({ settings: { ...plexIntegration().settings, index: { enabled: true, cron: '0 1 * * *', staleAfterHours: 72 } } });

function routes(extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/integrations': () => ({ body: [plexOn(), tautulli()] }),
    'GET /api/v1/notifications': () => ({ body: [] }),
    'GET /api/v1/sources': () => ({ body: [] }),
    'GET /api/v1/integrations/11/index': () => ({ body: index() }),
    'GET /api/v1/integrations/3/index': () => ({
      body: index({ appVersion: '1.43.4', stats: { sections: 3, items: 32, files: 22, filesUnmapped: 0 } as IndexView['stats'] }),
    }),
    ...extra,
  };
}

describe('Settings → Connect → Tautulli, Seerr and Maintainerr', () => {
  it('shows a connection with its linked Plex server and cache, and refreshes it', async () => {
    const { calls, user } = renderApp('/settings/connect', routes({ 'POST /api/v1/integrations/11/refresh': () => ({ status: 202, body: { id: 70 } }) }));
    const card = within(await screen.findByRole('article', { name: 'Tautulli' }));
    expect(card.getByText('Key stored')).toBeInTheDocument();
    expect(card.getByText('Plex')).toBeInTheDocument();
    expect(await card.findByText('Fresh')).toBeInTheDocument();
    expect(card.getByText('13 plays of 12 items')).toBeInTheDocument();
    const plex = within(await screen.findByRole('article', { name: 'Library index · Plex' }));
    expect(plex.getByText('On')).toBeInTheDocument();
    expect(await plex.findByText('3 libraries, 32 items, 22 files')).toBeInTheDocument();

    await user.click(card.getByRole('button', { name: 'Refresh now' }));
    expect(await card.findByText(/Refresh queued/)).toBeInTheDocument();
    expect(card.getByRole('link', { name: 'Open the job' })).toHaveAttribute('href', '/activity/jobs/70');
    expect(callsTo(calls, 'POST /api/v1/integrations/11/refresh')).toHaveLength(1);
  });

  it('shows a stale cache, lower bounds and a held refresh, and applies it', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations/11/index': () => ({
          body: index({
            fresh: false,
            reason: 'Tautulli cache is 4 d old (stale after 72 h)',
            error: 'Refresh guard held the new rows: the application answered no rows while the cache has 20',
            stats: { plays: 13, ratingKeys: 12, sectionsWithoutHistory: ['2'], usersWithoutHistory: 1 } as IndexView['stats'],
          }),
        }),
        'POST /api/v1/integrations/11/refresh': () => ({ status: 202, body: { id: 71 } }),
      }),
    );
    const card = within(await screen.findByRole('article', { name: 'Tautulli' }));
    expect(await card.findByText('Stale')).toBeInTheDocument();
    expect(card.getByText(/cache is 4 d old/)).toBeInTheDocument();
    expect(card.getByText(/play counts there are lower bounds/)).toBeInTheDocument();
    await user.click(card.getByRole('button', { name: 'Apply held changes' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Apply held changes · Tautulli' }));
    await user.click(dialog.getByRole('button', { name: 'Apply held changes' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/integrations/11/refresh')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/integrations/11/refresh')[0].body).toEqual({ allowChanges: true });
  });

  it('adds a Maintainerr without a key, links the Plex server and turns its index on', async () => {
    const plexOff = plexIntegration();
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations': () => ({ body: [plexOff] }),
        'POST /api/v1/integrations/test': () => ({ body: { ok: true, message: 'Connected to Maintainerr 3.4.1.', version: '3.4.1', plexMatches: null } }),
        'PUT /api/v1/integrations/3': () => ({ body: plexOn() }),
        'POST /api/v1/integrations': () => ({ status: 201, body: tautulli({ id: 12, type: 'maintainerr', name: 'Maintainerr', hasApiKey: false }) }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Add Maintainerr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Add Maintainerr' }));
    expect(form.queryByLabelText(/^API key/)).not.toBeInTheDocument();
    expect(form.getByText(/Maintainerr has no API authentication; Bunkarr only reads from it./)).toBeInTheDocument();
    await user.type(form.getByLabelText('URL'), 'http://maintainerr:6246');
    // The only Plex server is chosen; its index is off, so the form offers to turn it on.
    expect(form.getByLabelText('Plex server')).toHaveValue('3');
    expect(form.getByText('Turn on the library index of Plex when saving')).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Connected to Maintainerr 3.4.1.')).toBeInTheDocument();
    const test = callsTo(calls, 'POST /api/v1/integrations/test')[0].body as ProviderTestInput;
    expect(test).toEqual({ type: 'maintainerr', url: 'http://maintainerr:6246', settings: { plexIntegrationId: 3 } });

    await user.click(form.getByRole('button', { name: 'Save' }));
    // The connection is saved first, then the index is turned on.
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(1));
    expect(calls.findIndex((c) => c.key === 'POST /api/v1/integrations')).toBeLessThan(calls.findIndex((c) => c.key === 'PUT /api/v1/integrations/3'));
    const put = callsTo(calls, 'PUT /api/v1/integrations/3')[0].body as { settings: { index: unknown }; apiKey?: string };
    expect(put.settings.index).toEqual({ enabled: true, cron: '0 1 * * *', staleAfterHours: 72 });
    expect(put.apiKey).toBeUndefined();
    const body = callsTo(calls, 'POST /api/v1/integrations')[0].body as ProviderIntegrationInput;
    expect(body).toEqual({
      type: 'maintainerr',
      name: 'Maintainerr',
      url: 'http://maintainerr:6246',
      enabled: true,
      settings: { plexIntegrationId: 3, refresh: { cron: '45 */6 * * *', enabled: true, staleAfterHours: 24 } },
    });
  });

  it('leaves the Plex library index off when the server refuses the connection', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations': () => ({ body: [plexIntegration()] }),
        'PUT /api/v1/integrations/3': () => ({ body: plexOn() }),
        'POST /api/v1/integrations': () => ({ status: 400, body: { message: 'invalid Maintainerr URL' } }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Add Maintainerr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Add Maintainerr' }));
    await user.type(form.getByLabelText('URL'), 'http://maintainerr:6246');
    expect(form.getByText('Turn on the library index of Plex when saving')).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/invalid Maintainerr URL/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/integrations')).toHaveLength(1);
    expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(0);
  });

  it('after the index fails to turn on, Save again updates the saved connection instead of adding it twice', async () => {
    let indexFails = true;
    const saved = tautulli({ id: 12, type: 'maintainerr', name: 'Maintainerr', hasApiKey: false });
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations': () => ({ body: [plexIntegration()] }),
        'PUT /api/v1/integrations/3': () => (indexFails ? { status: 500, body: { message: 'database is locked' } } : { body: plexOn() }),
        'POST /api/v1/integrations': () => ({ status: 201, body: saved }),
        'PUT /api/v1/integrations/12': () => ({ body: saved }),
        'GET /api/v1/integrations/12/index': () => ({ body: index() }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Add Maintainerr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Add Maintainerr' }));
    await user.type(form.getByLabelText('URL'), 'http://maintainerr:6246');
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Maintainerr was saved, but turning on the library index of Plex failed \(database is locked\)/)).toBeInTheDocument();
    indexFails = false;
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(2));
    expect(callsTo(calls, 'POST /api/v1/integrations')).toHaveLength(1);
    expect(callsTo(calls, 'PUT /api/v1/integrations/12')).toHaveLength(1);
  });

  it('adds a Tautulli: requires its key and shows a Plex server mismatch', async () => {
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations': () => ({ body: [plexOn()] }),
        'POST /api/v1/integrations/test': () => ({
          body: { ok: false, message: 'Connected to Tautulli v2.18.1. It watches another Plex server than Plex.', plexMatches: false },
        }),
        'POST /api/v1/integrations': () => ({ status: 201, body: tautulli() }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Add Tautulli' }));
    const form = within(await screen.findByRole('dialog', { name: 'Add Tautulli' }));
    await user.type(form.getByLabelText('URL'), 'http://tautulli:8181');
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Enter Tautulli's API key/)).toBeInTheDocument();
    await user.type(form.getByLabelText(/^API key/), TAUTULLI_KEY);
    expect(form.queryByText(/Turn on the library index/)).not.toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText(/does not work with the linked Plex server/)).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/integrations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/integrations')[0].body as ProviderIntegrationInput;
    expect(body.apiKey).toBe(TAUTULLI_KEY);
    expect(body.settings.plexIntegrationId).toBe(3);
    expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(0);
  });

  it('keeps the stored key when editing a Seerr, whose Plex server is optional', async () => {
    const seerr = tautulli({ id: 13, type: 'seerr', name: 'Seerr', url: 'http://seerr:5055', settings: {} as Integration['settings'] });
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({
        'GET /api/v1/integrations': () => ({ body: [plexOn(), seerr] }),
        'GET /api/v1/integrations/13/index': () => ({ body: index({ stats: { requests: 10, counted: 9 } as IndexView['stats'] }) }),
        'PUT /api/v1/integrations/13': () => ({ body: seerr }),
      }),
    );
    const card = within(await screen.findByRole('article', { name: 'Seerr' }));
    expect(await card.findByText('10 requests (9 counted)')).toBeInTheDocument();
    expect(card.getByText('not linked (optional)')).toBeInTheDocument();
    await user.click(card.getByRole('button', { name: 'Edit Seerr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Seerr · Seerr' }));
    expect(form.getByLabelText('Plex server')).toHaveValue('0');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/13')).toHaveLength(1));
    const body = callsTo(calls, 'PUT /api/v1/integrations/13')[0].body as ProviderIntegrationInput;
    expect(body.apiKey).toBeUndefined();
    expect(body.settings).toEqual({ plexIntegrationId: 0, refresh: { cron: '30 2 * * *', enabled: true, staleAfterHours: 72 } });
  });

  it('turns a Plex server library index on', async () => {
    const plexOff = plexIntegration();
    const { calls, user } = renderApp(
      '/settings/connect',
      routes({ 'GET /api/v1/integrations': () => ({ body: [plexOff] }), 'PUT /api/v1/integrations/3': () => ({ body: plexOn() }) }),
    );
    const card = within(await screen.findByRole('article', { name: 'Library index · Plex' }));
    expect(card.getByText('Off')).toBeInTheDocument();
    await user.click(card.getByRole('button', { name: 'Library index settings · Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Library index · Plex' }));
    await user.click(form.getByRole('checkbox', { name: /Keep an index of this server/ }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/integrations/3')).toHaveLength(1));
    const body = callsTo(calls, 'PUT /api/v1/integrations/3')[0].body as { name: string; url: string; settings: Record<string, unknown> };
    expect(body.settings.index).toEqual({ enabled: true, cron: '0 1 * * *', staleAfterHours: 72 });
    expect(body.settings.dataPath).toBe('/plex');
    expect(body).not.toHaveProperty('apiKey');
  });
});
