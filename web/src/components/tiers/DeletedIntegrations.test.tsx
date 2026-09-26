import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { coversPath, type DeletedIntegration } from '@/api/deletedIntegrations';
import type { FileDetail, TierPreview } from '@/api/tiers';
import type { ItemCount } from '@/api/types';
import { testFields } from '@/components/tiers/testing';
import { callsTo, type Handler } from '@/test/fetch';
import { destination, GiB, job, paged, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';

const now = new Date().toISOString();

const radarr4k: DeletedIntegration = {
  key: '0123456789abcdef',
  integrationId: 7,
  name: 'Radarr 4K',
  app: 'Radarr',
  folders: ['/media/movies-4k'],
  deletedAt: now,
};

/**
 * deletedRoutes serves GET /integrations/deleted from a mutable list, and DELETE of a key removes
 * it (the server's confirmation).
 */
function deletedRoutes(list: DeletedIntegration[]): Record<string, Handler> {
  const routes: Record<string, Handler> = { 'GET /api/v1/integrations/deleted': () => ({ body: list }) };
  for (const d of [...list]) {
    routes[`DELETE /api/v1/integrations/deleted/${d.key}`] = () => {
      list.splice(
        list.findIndex((x) => x.key === d.key),
        1,
      );
      return { status: 204 };
    };
  }
  return routes;
}

const count = (files: number, bytes: number) => ({ files, bytes });

function preview(unknownSources: TierPreview['unknownSources']): TierPreview {
  return {
    id: '0123456789abcdef0123456789abcdef',
    revision: 3,
    createdAt: now,
    unknownSources,
    staleReferences: [],
    destinations: [
      {
        destinationId: 1,
        name: 'UNAS',
        stored: count(10, 40 * GiB),
        full: { ...count(10, 40 * GiB), uniqueBytes: 40 * GiB },
        manifest: count(0, 0),
        skip: count(0, 0),
        unknownPromoted: count(unknownSources?.length ? 4 : 0, unknownSources?.length ? 8 * GiB : 0),
        toCopy: count(0, 0),
        kept: count(0, 0),
        movedToNonFull: count(0, 0),
        byRule: [],
        configBackups: [],
      },
    ],
  };
}

describe('coversPath', () => {
  it('matches a folder, a path under it and an unmapped root folder, not a sibling with the same prefix', () => {
    expect(coversPath(radarr4k, '/media/movies-4k/Heat (1995)/Heat.mkv')).toBe(true);
    expect(coversPath(radarr4k, '/media/movies-4k')).toBe(true);
    expect(coversPath(radarr4k, '/media/movies-4k-old/Heat.mkv')).toBe(false);
    expect(coversPath(radarr4k, '/media/movies/Heat.mkv')).toBe(false);
    expect(coversPath({ folders: [], unmapped: ['/movies'] }, '/anywhere/x.mkv')).toBe(true);
    expect(coversPath({ folders: ['/'] }, '/x.mkv')).toBe(true);
  });
});

describe('Deleted *arr integrations', () => {
  it('Settings → Tiers lists them and confirms a removal: DELETE with the key, the list and the preview refresh', async () => {
    const list = [{ ...radarr4k }];
    let previews = 0;
    const { calls, user } = renderApp('/settings/tiers', {
      'GET /api/v1/tiers/rules': () => ({ body: { revision: 3, rules: [] } }),
      'GET /api/v1/tiers/fields': () => ({ body: testFields }),
      'GET /api/v1/tiers/presets': () => ({ body: [] }),
      'GET /api/v1/tiers/flags': () => ({ body: [] }),
      'GET /api/v1/destinations': () => ({ body: [destination({ id: 1, name: 'UNAS' })] }),
      'GET /api/v1/sources': () => ({ body: [source({ id: 1, name: 'Movies' })] }),
      'POST /api/v1/tiers/preview': () => {
        previews++;
        // The server names the deleted integration as an unknown source until it is confirmed.
        return {
          body: preview(
            list.length
              ? [{ integrationId: 7, name: 'Radarr 4K', reason: 'Radarr 4K (Radarr) was deleted: the files in its folders that no other *arr manages stay unknown (so full) until you confirm its removal (Settings → Tiers, Deleted *arr integrations)' }]
              : [],
          ),
        };
      },
      'GET /api/v1/tiers/preview/0123456789abcdef0123456789abcdef/items': () => ({ body: paged([], 0, 1, 50) }),
      ...deletedRoutes(list),
    });

    const section = await screen.findByRole('region', { name: 'Deleted *arr integrations' });
    expect(within(section).getByText('A deleted *arr integration needs your confirmation')).toBeInTheDocument();
    expect(within(section).getByText('Radarr 4K')).toBeInTheDocument();
    expect(within(section).getByText('Folders: /media/movies-4k')).toBeInTheDocument();

    // The preview's unknown-source banner offers the same confirmation.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Preview' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: 'Preview' }));
    expect(await screen.findByText('Some facts are unknown')).toBeInTheDocument();
    expect(await screen.findAllByRole('button', { name: 'Confirm removal of Radarr 4K' })).toHaveLength(2);
    expect(previews).toBe(1);

    // Cancelling changes nothing.
    await user.click(within(section).getByRole('button', { name: 'Confirm removal of Radarr 4K' }));
    let dialog = await screen.findByRole('dialog', { name: 'Confirm removal of Radarr 4K' });
    expect(within(dialog).getByText(/Nothing is removed from your destinations/)).toBeInTheDocument();
    expect(within(dialog).getByText(/your tier rules decide them from the next sync or preview/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(callsTo(calls, 'DELETE /api/v1/integrations/deleted/0123456789abcdef')).toHaveLength(0);

    await user.click(within(section).getByRole('button', { name: 'Confirm removal of Radarr 4K' }));
    dialog = await screen.findByRole('dialog', { name: 'Confirm removal of Radarr 4K' });
    await user.click(within(dialog).getByRole('button', { name: 'Confirm removal' }));
    await waitFor(() => expect(callsTo(calls, 'DELETE /api/v1/integrations/deleted/0123456789abcdef')).toHaveLength(1));
    // The list is read again (now empty: the section goes) and the preview is computed again.
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Deleted *arr integrations' })).not.toBeInTheDocument());
    await waitFor(() => expect(previews).toBe(2));
    await waitFor(() => expect(screen.queryByText('Some facts are unknown')).not.toBeInTheDocument());
    expect(screen.queryByRole('button', { name: 'Confirm removal of Radarr 4K' })).not.toBeInTheDocument();
  });

  it('the Library item view offers the confirmation for a file in a deleted integration\'s folder, then reloads the file', async () => {
    const list = [{ ...radarr4k }, { ...radarr4k, key: 'fedcba9876543210', integrationId: 8, name: 'Sonarr', app: 'Sonarr', folders: ['/media/tv'] }];
    const why = 'Radarr 4K (Radarr), which managed this folder, was deleted; the file stays unknown until you confirm its removal (Settings → Tiers, Deleted *arr integrations)';
    const unknownFile: FileDetail = {
      file: { id: 501, relPath: 'Heat (1995)/Heat.mkv', size: 30 * GiB, mtime: now, hardlinkGroup: null },
      source: { id: 1, name: 'Movies 4K' },
      facts: { arr: { state: 'unknown', why }, plex: null, watch: null, requests: null, maintainerr: null, flags: [], unknown: [{ source: 'arr', reason: why }] },
      tiers: [{ destinationId: 1, destinationName: 'UNAS', tier: 'full', ruleId: 1, ruleName: 'Keep', reasons: [], unknown: [], unknownPromoted: true, record: null }],
    };
    const managed: FileDetail = {
      ...unknownFile,
      facts: { ...unknownFile.facts, arr: { state: 'unmanaged' }, unknown: [] },
      tiers: [{ ...unknownFile.tiers![0], tier: 'manifest', ruleId: 2, ruleName: 'Everything else', unknownPromoted: false }],
    };
    const { calls, user } = renderApp('/library/files/501', {
      'GET /api/v1/catalog/files/501': () => ({ body: list.some((d) => d.key === radarr4k.key) ? unknownFile : managed }),
      'GET /api/v1/tiers/fields': () => ({ body: testFields }),
      'GET /api/v1/sources': () => ({ body: [source({ id: 1, name: 'Movies 4K', path: '/media/movies-4k' })] }),
      ...deletedRoutes(list),
    });
    const notice = (await screen.findByText(/keeps this file unknown \(so full\) until you/)).parentElement as HTMLElement;
    // Only the integration whose folder holds the file is offered.
    expect(within(notice).getByRole('button', { name: 'Confirm removal of Radarr 4K' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Confirm removal of Sonarr' })).not.toBeInTheDocument();

    await user.click(within(notice).getByRole('button', { name: 'Confirm removal of Radarr 4K' }));
    const dialog = await screen.findByRole('dialog', { name: 'Confirm removal of Radarr 4K' });
    await user.click(within(dialog).getByRole('button', { name: 'Confirm removal' }));
    await waitFor(() => expect(callsTo(calls, 'DELETE /api/v1/integrations/deleted/0123456789abcdef')).toHaveLength(1));
    expect(callsTo(calls, 'DELETE /api/v1/integrations/deleted/fedcba9876543210')).toHaveLength(0);
    // The file is read again: now unmanaged, decided by the rules.
    await waitFor(() => expect(screen.queryByText(/keeps this file unknown/)).not.toBeInTheDocument());
    expect(await screen.findByText('rule "Everything else"')).toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/catalog/files/501').length).toBeGreaterThanOrEqual(2);
  });

  it('Settings → Connect lists them under the *arr connections and the delete dialog says what stays full', async () => {
    const list = [{ ...radarr4k }];
    const { calls, user } = renderApp('/settings/connect', {
      'GET /api/v1/integrations': () => ({ body: [] }),
      'GET /api/v1/notifications': () => ({ body: [] }),
      'GET /api/v1/sources': () => ({ body: [] }),
      ...deletedRoutes(list),
    });
    const section = await screen.findByRole('region', { name: 'Deleted *arr integrations' });
    expect(within(section).getByText('Radarr 4K')).toBeInTheDocument();
    await user.click(within(section).getByRole('button', { name: 'Confirm removal of Radarr 4K' }));
    const dialog = await screen.findByRole('dialog', { name: 'Confirm removal of Radarr 4K' });
    await user.click(within(dialog).getByRole('button', { name: 'Confirm removal' }));
    await waitFor(() => expect(callsTo(calls, 'DELETE /api/v1/integrations/deleted/0123456789abcdef')).toHaveLength(1));
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Deleted *arr integrations' })).not.toBeInTheDocument());
  });

  it('a sync\'s held-changes notice names them with the action; nothing shows when there are none or they cannot be read', async () => {
    const summary: ItemCount[] = [{ action: 'copy', status: 'held', files: 4, bytes: 8 * GiB }];
    const held = job({ id: 12, status: 'completed_with_warnings', finishedAt: now, summary: '4 held', stats: { filesHeld: 4 } });
    const base: Record<string, Handler> = {
      'GET /api/v1/destinations': () => ({ body: [destination()] }),
      'GET /api/v1/sources': () => ({ body: [] }),
      'GET /api/v1/integrations': () => ({ body: [] }),
      'GET /api/v1/jobs/12': () => ({ body: held }),
      'GET /api/v1/jobs/12/items/summary': () => ({ body: summary }),
      'GET /api/v1/jobs/12/items': () => ({ body: paged([]) }),
      'GET /api/v1/jobs/12/logs': () => ({ body: [] }),
    };
    const list = [{ ...radarr4k }];
    const first = renderApp('/activity/jobs/12', { ...base, ...deletedRoutes(list) });
    expect(await screen.findByText('4 changes were held by the mass-change guard')).toBeInTheDocument();
    expect(await screen.findByText(/A deleted \*arr integration keeps the files in its folders/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Settings → Tiers' })).toHaveAttribute('href', '/settings/tiers');
    await first.user.click(screen.getByRole('button', { name: 'Confirm removal of Radarr 4K' }));
    const dialog = await screen.findByRole('dialog', { name: 'Confirm removal of Radarr 4K' });
    await first.user.click(within(dialog).getByRole('button', { name: 'Confirm removal' }));
    await waitFor(() => expect(callsTo(first.calls, 'DELETE /api/v1/integrations/deleted/0123456789abcdef')).toHaveLength(1));
    await waitFor(() => expect(screen.queryByText(/deleted \*arr integration keeps/)).not.toBeInTheDocument());
    first.unmount();

    // GET /integrations/deleted unrouted (404): the job page shows no hint and no error.
    renderApp('/activity/jobs/12', base);
    expect(await screen.findByText('4 changes were held by the mass-change guard')).toBeInTheDocument();
    expect(screen.queryByText(/deleted \*arr integration/)).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
