import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { FileDetail, ItemFlag } from '@/api/tiers';
import { testFields } from '@/components/tiers/testing';
import { callsTo, type Handler } from '@/test/fetch';
import { GiB, paged, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';

const now = new Date().toISOString();

function detail(over: Partial<FileDetail> = {}): FileDetail {
  return {
    file: { id: 501, relPath: 'The Matrix (1999)/The Matrix.mkv', size: 30 * GiB, mtime: now, hardlinkGroup: null },
    source: { id: 1, name: 'Movies' },
    facts: {
      arr: {
        state: 'item',
        integrationId: 2,
        item: {
          integrationId: 2,
          app: 'Radarr',
          itemId: 77,
          kind: 'movie',
          arrId: 9,
          title: 'The Matrix',
          year: 1999,
          externalIds: { tmdb: 603, imdb: 'tt0133093' },
          tags: ['4k', 'keep'],
          qualityProfile: 'Ultra-HD',
          qualityProfileId: 5,
          rootFolder: '/movies',
          monitored: true,
          genres: ['Action', 'Science Fiction'],
        },
        arrFileId: 12,
        quality: 'Remux-2160p',
      },
      plex: null,
      watch: { known: false, why: 'Tautulli cache is 31 h old', plays: 0 },
      requests: { requested: 'true', integrationId: 5, users: [4, 9] },
      maintainerr: { pending: 'true', integrationId: 6, deleteAfter: now },
      flags: [],
      unknown: [{ source: 'tautulli', reason: 'Tautulli cache is 31 h old' }],
    },
    tiers: [
      {
        destinationId: 1,
        destinationName: 'UNAS',
        tier: 'full',
        ruleId: 12,
        ruleName: 'Tagged',
        reasons: [{ ruleId: 12, conditionIndex: 0, field: 'arr.tag', op: 'has', value: 'keep', actual: ['4k', 'keep'], result: 'true', source: { kind: 'arr', integrationId: 2 } }],
        unknown: [],
        unknownPromoted: false,
        record: { state: 'present', relPath: 'movies/The Matrix (1999)/The Matrix.mkv', size: 30 * GiB, copiedAt: now, verifiedAt: null },
      },
      {
        destinationId: 2,
        destinationName: 'Offsite',
        tier: 'full',
        ruleId: 13,
        ruleName: 'Rarely watched',
        reasons: [{ ruleId: 13, conditionIndex: 0, field: 'tautulli.lastWatched', op: 'olderThan', value: 365, result: 'unknown', source: { kind: 'tautulli', integrationId: 4 }, why: 'Tautulli cache is 31 h old' }],
        unknown: [],
        unknownPromoted: true,
        record: null,
      },
    ],
    ...over,
  };
}

function routes(d: FileDetail, over: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/catalog/files/501': () => ({ body: d }),
    'GET /api/v1/tiers/fields': () => ({ body: testFields }),
    ...over,
  };
}

describe('Library → item view', () => {
  it('shows the facts with unknown ones marked, the tier per destination with reasons, and the records', async () => {
    renderApp('/library/files/501', routes(detail()));
    expect(await screen.findByRole('heading', { name: 'The Matrix.mkv' })).toBeInTheDocument();
    expect(screen.getByText('The Matrix (1999)/The Matrix.mkv')).toBeInTheDocument();
    expect(screen.getByText('Tautulli: Tautulli cache is 31 h old')).toBeInTheDocument();

    const facts = screen.getByRole('table', { name: 'Facts' });
    const row = (label: string) => within(facts).getByRole('rowheader', { name: label }).closest('tr')!;
    expect(within(row('*arr item')).getByText(/The Matrix \(1999\)/)).toBeInTheDocument();
    expect(within(row('*arr item')).getByText(/Radarr movie #9/)).toBeInTheDocument();
    expect(within(row('Tags')).getByText('4k, keep')).toBeInTheDocument();
    expect(within(row('Quality profile')).getByText('Ultra-HD')).toBeInTheDocument();
    expect(within(row('Root folder')).getByText('/movies')).toBeInTheDocument();
    expect(within(row('Monitored')).getByText('Yes')).toBeInTheDocument();
    expect(within(row('Plex section')).getByText('Not read (no integration supplies it)')).toBeInTheDocument();
    expect(within(row('Plays')).getByText('Unknown: Tautulli cache is 31 h old')).toBeInTheDocument();
    expect(within(row('Requested by')).getByText('alice, Seerr user #9')).toBeInTheDocument();
    expect(within(row('Maintainerr')).getByText(/Pending deletion after/)).toBeInTheDocument();
    expect(within(row('Flags')).getByText('None')).toBeInTheDocument();

    const tiers = screen.getByRole('table', { name: 'Tier per destination' });
    expect(within(tiers).getByText('rule "Tagged"')).toBeInTheDocument();
    expect(within(tiers).getByText('*arr tag has "keep": true (4k, keep)')).toBeInTheDocument();
    expect(within(tiers).getByText('Unknown → full')).toBeInTheDocument();
    expect(within(tiers).getByText('Last watched (Tautulli) more than 365 days ago: unknown (Tautulli cache is 31 h old)')).toBeInTheDocument();

    const records = screen.getByRole('table', { name: 'Destination records' });
    expect(within(records).getByText('present')).toBeInTheDocument();
    expect(within(records).getByText('movies/The Matrix (1999)/The Matrix.mkv')).toBeInTheDocument();
    expect(within(records).getByText('Not copied yet')).toBeInTheDocument();
  });

  it('marks the *arr item irreplaceable by default', async () => {
    const flag: ItemFlag = {
      id: 8,
      flag: 'irreplaceable',
      kind: 'arr',
      integrationId: 2,
      arrKind: 'movie',
      arrId: 9,
      externalIds: { tmdb: 603 },
      lastSourceId: 1,
      lastRelPath: 'The Matrix (1999)',
      sourceId: null,
      relPath: null,
      note: 'the original',
      createdAt: now,
      updatedAt: now,
      resolved: true,
    };
    let flagged = false;
    const { calls, user } = renderApp(
      '/library/files/501',
      routes(detail(), {
        'GET /api/v1/catalog/files/501': () => ({ body: flagged ? detail({ facts: { ...detail().facts, flags: [8] } }) : detail() }),
        'POST /api/v1/tiers/flags': () => {
          flagged = true;
          return { status: 201, body: flag };
        },
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Mark irreplaceable' }));
    const dialog = await screen.findByRole('dialog', { name: 'Mark irreplaceable' });
    expect(within(dialog).getByRole('radio', { name: /The movie “The Matrix \(1999\)”/ })).toBeChecked();
    expect(within(dialog).getByText(/found by tmdb 603, imdb tt0133093/)).toBeInTheDocument();
    await user.type(within(dialog).getByPlaceholderText('Why it is irreplaceable (optional)'), 'the original');
    await user.click(within(dialog).getByRole('button', { name: 'Mark irreplaceable' }));

    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/tiers/flags')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/tiers/flags')[0].body).toEqual({ flag: 'irreplaceable', target: { integrationId: 2, kind: 'movie', arrId: 9 }, note: 'the original' });
    expect(await screen.findByText(/Flagged irreplaceable \(flag #8\)/)).toBeInTheDocument();
    expect(await screen.findByText('Irreplaceable (flag #8)')).toBeInTheDocument();
  });

  it('marks the file or its folder when there is no *arr item, and removes a flag', async () => {
    const unmanaged = detail({ facts: { ...detail().facts, arr: { state: 'unmanaged' }, flags: [3] } });
    const { calls, user } = renderApp(
      '/library/files/501',
      routes(unmanaged, {
        'POST /api/v1/tiers/flags': () => ({ status: 201, body: { id: 9 } }),
        'DELETE /api/v1/tiers/flags/3': () => ({ status: 204 }),
      }),
    );
    expect(await screen.findByText('Not managed: outside every *arr root folder')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Mark irreplaceable' }));
    const dialog = await screen.findByRole('dialog', { name: 'Mark irreplaceable' });
    expect(within(dialog).queryByRole('radio', { name: /The movie/ })).not.toBeInTheDocument();
    expect(within(dialog).getByRole('radio', { name: /This file/ })).toBeChecked();
    await user.click(within(dialog).getByRole('radio', { name: /Its folder/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Mark irreplaceable' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/tiers/flags')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/tiers/flags')[0].body).toEqual({ flag: 'irreplaceable', target: { sourceId: 1, relPath: 'The Matrix (1999)' }, note: '' });

    await user.click(screen.getByRole('button', { name: 'Remove flag #3' }));
    await user.click(within(await screen.findByRole('dialog', { name: 'Remove flag' })).getByRole('button', { name: 'Remove flag' }));
    await waitFor(() => expect(callsTo(calls, 'DELETE /api/v1/tiers/flags/3')).toHaveLength(1));
  });

  it('shows a flag refusal in the dialog', async () => {
    const { user } = renderApp(
      '/library/files/501',
      routes(detail(), { 'POST /api/v1/tiers/flags': () => ({ status: 409, body: { message: 'this item is already flagged irreplaceable (flag #8)' } }) }),
    );
    await user.click(await screen.findByRole('button', { name: 'Mark irreplaceable' }));
    const dialog = await screen.findByRole('dialog', { name: 'Mark irreplaceable' });
    await user.click(within(dialog).getByRole('button', { name: 'Mark irreplaceable' }));
    expect(await within(dialog).findByText('this item is already flagged irreplaceable (flag #8)')).toBeInTheDocument();
  });

  it('reports a file that is not in the catalog', async () => {
    renderApp('/library/files/501', routes(detail(), { 'GET /api/v1/catalog/files/501': () => ({ status: 404, body: { message: 'catalog file 501 not found' } }) }));
    expect(await screen.findByText('catalog file 501 not found')).toBeInTheDocument();
  });

  it('is linked from the source file browser', async () => {
    renderApp('/library/sources/1', {
      'GET /api/v1/sources/1': () => ({ body: source() }),
      'GET /api/v1/sources/1/files': () => ({
        body: paged(
          [
            { id: 501, relPath: 'The Matrix (1999)/The Matrix.mkv', size: GiB, mtime: now, nlink: 1, hardlinkGroup: null, deleted: false },
            { id: 502, relPath: 'Old/Gone.mkv', size: GiB, mtime: now, nlink: 1, hardlinkGroup: null, deleted: true },
          ],
          2,
          1,
          50,
        ),
      }),
    });
    expect(await screen.findByRole('link', { name: 'The Matrix (1999)/The Matrix.mkv' })).toHaveAttribute('href', '/library/files/501');
    expect(screen.queryByRole('link', { name: 'Old/Gone.mkv' })).not.toBeInTheDocument();
  });
});
