import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { IndexView } from '@/api/arr';
import type { Integration } from '@/api/types';
import { callsTo } from '@/test/fetch';
import { renderApp } from '@/test/render';

// Poll quickly in tests (the panel polls every 5 s).
vi.mock('@/api/arr', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/arr')>()),
  WEBHOOK_POLL_MS: 50,
}));

const radarr: Integration = {
  id: 7,
  type: 'radarr',
  name: 'Radarr',
  url: 'http://radarr:7878',
  enabled: true,
  hasApiKey: true,
  settings: {
    pathMappings: [{ arr: '/movies', local: '/media/movies' }],
    refresh: { cron: '15 */6 * * *', enabled: true, staleAfterHours: 24 },
  } as unknown as Integration['settings'],
  createdAt: '2026-09-01T10:00:00Z',
  updatedAt: '2026-09-01T10:00:00Z',
};

const index: IndexView = {
  status: 'ok',
  refreshedAt: new Date().toISOString(),
  attemptedAt: new Date().toISOString(),
  error: null,
  appVersion: '6.4.4.10685',
  stats: { items: 1, files: 1 },
  fresh: true,
  instanceMatches: true,
  staleAfterHours: 24,
};

describe('Settings → Connect → *arr webhook panel', () => {
  it('keeps its activity current while open: a Test pressed in Radarr shows up without reopening', async () => {
    let lastTestAt: string | null = null;
    const { calls, user } = renderApp('/settings/connect', {
      'GET /api/v1/integrations': () => ({ body: [radarr] }),
      'GET /api/v1/notifications': () => ({ body: [] }),
      'GET /api/v1/sources': () => ({ body: [] }),
      'GET /api/v1/integrations/7/index': () => ({ body: index }),
      'GET /api/v1/integrations/7/webhook': () => ({
        body: {
          path: '/api/v1/webhook/radarr/7',
          genericPath: '/api/v1/webhook/radarr',
          hasKey: true,
          lastEventAt: null,
          lastTestAt,
          last24h: 0,
          warnings: lastTestAt ? [] : ['No Test received yet.'],
          recent: [],
        },
      }),
    });
    await user.click(await screen.findByRole('button', { name: 'Edit Radarr' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Radarr · Radarr' }));
    expect(await form.findByText(/Last Test received: never/)).toBeInTheDocument();
    expect(form.getByText('No Test received yet.')).toBeInTheDocument();

    // The user presses Test in Radarr; the server records it.
    lastTestAt = new Date(Date.now() - 2_000).toISOString();
    await waitFor(() => expect(form.queryByText(/Last Test received: never/)).not.toBeInTheDocument(), { timeout: 2_000 });
    expect(form.queryByText('No Test received yet.')).not.toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/integrations/7/webhook').length).toBeGreaterThanOrEqual(2);
  });
});
