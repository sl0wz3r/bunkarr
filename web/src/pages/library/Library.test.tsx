import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { CatalogFile, PlexSection, SourceInput } from '@/api/types';
import { callsTo } from '@/test/fetch';
import { job, paged, plexIntegration, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';

const stats = { sources: 1, files: 1200, bytes: 3 * 1024 ** 4, uniqueBytes: 2 * 1024 ** 4, hardlinkGroups: 40, hardlinkedFiles: 80 };

describe('Import from Plex', () => {
  const sections: PlexSection[] = [
    { key: '1', title: 'Movies', type: 'movie', locations: [{ path: '/data/movies', localPath: '/media/movies', exists: true }] },
    {
      key: '2',
      title: 'TV Shows',
      type: 'show',
      locations: [
        { path: '/data/tv', localPath: '/media/tv', exists: true },
        { path: '/other/tv', localPath: '', exists: false },
        { path: '/data/anime', localPath: '/media/anime', exists: false },
      ],
    },
    { key: '3', title: 'Music', type: 'artist', locations: [{ path: '/data/music', localPath: '/media/music', exists: true }] },
  ];

  it('shows mapped paths and creates the chosen sources', async () => {
    const created: SourceInput[] = [];
    const { calls, user } = renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [source({ id: 5, name: 'Music', path: '/media/music', destFolder: 'music' })] }),
      'GET /api/v1/catalog/stats': () => ({ body: stats }),
      'GET /api/v1/integrations': () => ({ body: [plexIntegration()] }),
      'GET /api/v1/integrations/3/plex/sections': () => ({ body: sections }),
      'POST /api/v1/sources': (init) => {
        const body = JSON.parse(String(init?.body)) as SourceInput;
        created.push(body);
        return { status: 201, body: source({ id: 10 + created.length, ...body }) };
      },
    });

    await user.click(await screen.findByRole('button', { name: 'Import from Plex' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Import from Plex' }));
    expect(await dialog.findByText('/data/movies')).toBeInTheDocument();
    expect(dialog.getByText('→ /media/movies')).toBeInTheDocument();
    expect(dialog.getByText('No path mapping covers this folder')).toBeInTheDocument();
    expect(dialog.getByText('Not found inside Bunkarr')).toBeInTheDocument();
    expect(dialog.getByText('Already a source (Music)')).toBeInTheDocument();
    expect(dialog.getByRole('checkbox', { name: 'Import /other/tv' })).toBeDisabled();
    expect(dialog.getByRole('checkbox', { name: 'Import /data/anime' })).toBeDisabled();
    expect(dialog.getByRole('checkbox', { name: 'Import /data/music' })).toBeDisabled();

    expect(dialog.getByRole('button', { name: 'Create source' })).toBeDisabled();
    await user.click(dialog.getByRole('checkbox', { name: 'Import /data/movies' }));
    await user.click(dialog.getByRole('checkbox', { name: 'Import /data/tv' }));
    const tvName = dialog.getByLabelText('Source name for /data/tv');
    expect(tvName).toHaveValue('TV Shows (tv)');
    await user.clear(tvName);
    await user.type(tvName, 'TV');
    await user.click(dialog.getByRole('button', { name: 'Create 2 sources' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(calls, 'POST /api/v1/sources')).toHaveLength(2);
    expect(created).toEqual([
      { name: 'Movies', path: '/media/movies', exclude: [], enabled: true, plexIntegrationId: 3, plexSectionId: '1', plexPath: '/data/movies' },
      { name: 'TV', path: '/media/tv', exclude: [], enabled: true, plexIntegrationId: 3, plexSectionId: '2', plexPath: '/data/tv' },
    ]);
  });

  it('keeps the dialog open and reports a source the server rejects', async () => {
    const { user } = renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [] }),
      'GET /api/v1/catalog/stats': () => ({ body: stats }),
      'GET /api/v1/integrations': () => ({ body: [plexIntegration()] }),
      'GET /api/v1/integrations/3/plex/sections': () => ({ body: sections.slice(0, 1) }),
      'POST /api/v1/sources': () => ({ status: 409, body: { message: 'a source already uses /media/movies' } }),
    });
    await user.click((await screen.findAllByRole('button', { name: 'Import from Plex' }))[0]);
    const dialog = within(await screen.findByRole('dialog', { name: 'Import from Plex' }));
    await user.click(await dialog.findByRole('checkbox', { name: 'Import /data/movies' }));
    await user.click(dialog.getByRole('button', { name: 'Create source' }));
    expect(await dialog.findByText('a source already uses /media/movies')).toBeInTheDocument();
  });

  it('reports an unreachable Plex at once instead of retrying', async () => {
    const { calls, user } = renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [] }),
      'GET /api/v1/catalog/stats': () => ({ body: stats }),
      'GET /api/v1/integrations': () => ({ body: [plexIntegration()] }),
      'GET /api/v1/integrations/3/plex/sections': () => ({ status: 502, body: { message: 'plex GET /library/sections: connection refused' } }),
    });
    await user.click((await screen.findAllByRole('button', { name: 'Import from Plex' }))[0]);
    const dialog = within(await screen.findByRole('dialog', { name: 'Import from Plex' }));
    expect(await dialog.findByText('plex GET /library/sections: connection refused')).toBeInTheDocument();
    expect(dialog.queryByText('Loading libraries…')).not.toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/integrations/3/plex/sections')).toHaveLength(1);
  });

  it('points to Settings → Plex when no server is set up', async () => {
    const { user } = renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [] }),
      'GET /api/v1/catalog/stats': () => ({ body: stats }),
      'GET /api/v1/integrations': () => ({ body: [] }),
    });
    await user.click((await screen.findAllByRole('button', { name: 'Import from Plex' }))[0]);
    expect(await screen.findByRole('link', { name: 'Settings → Plex' })).toHaveAttribute('href', '/settings/plex');
  });
});

describe('Sources', () => {
  it('lists sources with stats, tests a path with warnings and saves', async () => {
    const { calls, user } = renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [source()] }),
      'GET /api/v1/catalog/stats': () => ({ body: stats }),
      'POST /api/v1/sources/test': () => ({
        body: { ok: true, exists: true, isDir: true, fsType: 'fuse.shfs', fuse: true, entries: 12, message: 'Readable.', warnings: ['Unraid /mnt/user: enable hard link support'] },
      }),
      'POST /api/v1/sources': () => ({ status: 201, body: source({ id: 2 }) }),
      'POST /api/v1/sources/1/scan': () => ({ status: 202, body: job({ id: 40, type: 'scan', status: 'queued', params: { sourceIds: [1] } }) }),
    });

    expect(await screen.findByRole('link', { name: 'Movies' })).toHaveAttribute('href', '/library/sources/1');
    const table = within(screen.getByRole('table', { name: 'Sources' }));
    expect(table.getByText('3.0 TiB')).toBeInTheDocument();
    expect(table.getByText('2.0 TiB')).toBeInTheDocument();
    expect(within(screen.getByRole('group', { name: 'Catalog' })).getByText('80')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Scan Movies' }));
    expect(await screen.findByRole('link', { name: 'View job #40' })).toHaveAttribute('href', '/activity/jobs/40');

    await user.click(screen.getByRole('button', { name: 'Add source' }));
    const form = within(await screen.findByRole('dialog', { name: 'Add source' }));
    await user.type(form.getByLabelText('Name'), 'TV');
    await user.type(form.getByLabelText('Path'), '/mnt/user/tv');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Unraid /mnt/user: enable hard link support')).toBeInTheDocument();
    expect(form.getByText('FUSE')).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/sources/test')[0].body).toEqual({ path: '/mnt/user/tv' });

    await user.type(form.getByLabelText('Exclude'), '*.partial{enter}  {enter}.recycle/**');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/sources')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/sources')[0].body).toEqual({
      name: 'TV',
      path: '/mnt/user/tv',
      exclude: ['*.partial', '.recycle/**'],
      enabled: true,
    });
  });

  it('keeps the Plex and *arr links when a source is edited (PUT replaces them)', async () => {
    const imported = source({ plexIntegrationId: 3, plexSectionId: '1', plexPath: '/data/movies', arrIntegrationId: 7 });
    const { calls, user } = renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [imported] }),
      'GET /api/v1/catalog/stats': () => ({ body: stats }),
      'PUT /api/v1/sources/1': () => ({ body: imported }),
    });
    await user.click(await screen.findByRole('button', { name: 'Edit Movies' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit source · Movies' }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/sources/1')).toHaveLength(1));
    expect(callsTo(calls, 'PUT /api/v1/sources/1')[0].body).toMatchObject({ plexIntegrationId: 3, plexSectionId: '1', plexPath: '/data/movies', arrIntegrationId: 7 });
  });

  it('shows a never-scanned source (lastScanStatus "") and says when the folder list is cut short', async () => {
    const { user } = renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [source({ lastScanStatus: '', lastScanAt: null, fsType: '' })] }),
      'GET /api/v1/catalog/stats': () => ({ body: stats }),
      'GET /api/v1/filesystem': () => ({ body: { path: '/', parent: '', directories: [{ name: 'media', path: '/media' }], truncated: true } }),
    });
    expect(await screen.findByText('Never scanned')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Add source' }));
    const form = within(await screen.findByRole('dialog', { name: 'Add source' }));
    await user.click(form.getByRole('button', { name: 'Browse for Path' }));
    const picker = within(await screen.findByRole('dialog', { name: 'Choose path' }));
    expect(await picker.findByText(/Only the first 1 folders are listed/)).toBeInTheDocument();
  });

  it('deletes a source only after confirmation', async () => {
    const { calls, user } = renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [source()] }),
      'GET /api/v1/catalog/stats': () => ({ body: stats }),
      'DELETE /api/v1/sources/1': () => ({ status: 204 }),
    });
    await user.click(await screen.findByRole('button', { name: 'Delete Movies' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Delete source' }));
    expect(dialog.getByText(/nothing is deleted from your destinations/)).toBeInTheDocument();
    expect(callsTo(calls, 'DELETE /api/v1/sources/1')).toHaveLength(0);
    await user.click(dialog.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(callsTo(calls, 'DELETE /api/v1/sources/1')).toHaveLength(1));
  });
});

describe('Source file browser', () => {
  const files: CatalogFile[] = [
    { id: 1, relPath: 'Heat (1995)/Heat.mkv', size: 8 * 1024 ** 3, mtime: '2026-01-02T03:04:05Z', hardlinkGroup: '9:1', nlink: 2, deleted: false },
    { id: 2, relPath: 'Old (1990)/Old.mkv', size: 1024 ** 3, mtime: '2025-01-02T03:04:05Z', hardlinkGroup: null, nlink: 1, deleted: true },
  ];

  it('pages, searches and filters the catalog', async () => {
    const { calls, user } = renderApp('/library/sources/1', {
      'GET /api/v1/sources/1': () => ({ body: source() }),
      'GET /api/v1/sources/1/files': (_, url) => {
        const search = url.searchParams.get('search') ?? '';
        const filter = url.searchParams.get('filter');
        const records = files.filter((f) => f.relPath.includes(search) && (filter !== 'deleted' || f.deleted) && (filter !== 'hardlinked' || f.hardlinkGroup));
        return { body: paged(records, search || filter !== 'all' ? records.length : 120, Number(url.searchParams.get('page')), 50) };
      },
    });

    expect(await screen.findByRole('heading', { name: 'Movies' })).toBeInTheDocument();
    expect(await screen.findByText('Heat (1995)/Heat.mkv')).toBeInTheDocument();
    expect(screen.getByText('2 names')).toBeInTheDocument();
    expect(screen.getByText('Deleted')).toBeInTheDocument();
    expect(screen.getByText('1–50 of 120')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Next page' }));
    await waitFor(() => expect(callsTo(calls, 'GET /api/v1/sources/1/files').at(-1)!.query.get('page')).toBe('2'));

    await user.selectOptions(screen.getByLabelText('Show'), 'deleted');
    await waitFor(() => expect(screen.queryByText('Heat (1995)/Heat.mkv')).not.toBeInTheDocument());
    let last = callsTo(calls, 'GET /api/v1/sources/1/files').at(-1)!;
    expect(last.query.get('filter')).toBe('deleted');
    expect(last.query.get('page')).toBe('1');

    await user.selectOptions(screen.getByLabelText('Show'), 'all');
    await user.type(screen.getByLabelText('Search'), 'Heat');
    await waitFor(() => expect(callsTo(calls, 'GET /api/v1/sources/1/files').at(-1)!.query.get('search')).toBe('Heat'));
    expect(await screen.findByText('Heat (1995)/Heat.mkv')).toBeInTheDocument();
    last = callsTo(calls, 'GET /api/v1/sources/1/files').at(-1)!;
    expect(last.query.get('filter')).toBe('all');
  });
});
