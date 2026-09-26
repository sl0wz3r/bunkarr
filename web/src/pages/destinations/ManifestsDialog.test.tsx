import { screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { filenameFromDisposition, type ManifestVersion } from '@/api/manifests';
import { callsTo } from '@/test/fetch';
import { destination, job, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';

function version(over: Partial<ManifestVersion> = {}): ManifestVersion {
  return {
    id: 7,
    destinationId: 1,
    jobId: 40,
    createdAt: '2026-09-25T12:00:00Z',
    path: '.bunkarr/manifests/20260925T120000Z',
    format: 1,
    itemCount: 1234,
    fileCount: 5678,
    bytes: 3 * 1024 ** 4,
    checksum: 'sha256:' + 'ab'.repeat(32),
    integrity: 'ok',
    ...over,
  };
}

const damaged = version({
  id: 6,
  createdAt: '2026-09-24T12:00:00Z',
  path: '.bunkarr/manifests/20260924T120000Z',
  itemCount: 1000,
  fileCount: 2000,
  bytes: 1024 ** 3,
  integrity: 'damaged',
});

function routes(extra = {}) {
  return {
    'GET /api/v1/destinations': () => ({ body: [destination()] }),
    'GET /api/v1/sources': () => ({ body: [source()] }),
    'GET /api/v1/integrations': () => ({ body: [] }),
    'GET /api/v1/destinations/1/manifests': () => ({ body: [version(), damaged] }),
    ...extra,
  };
}

async function openDialog(user: ReturnType<typeof renderApp>['user']) {
  await user.click(await screen.findByRole('button', { name: /^Manifests on / }));
  return within(await screen.findByRole('dialog', { name: /^Manifests · / }));
}

describe('Manifests dialog', () => {
  it('lists the versions with verified downloads, and none for a damaged one', async () => {
    const { user } = renderApp('/destinations', routes());
    const dialog = await openDialog(user);
    expect(await dialog.findByText('1,234')).toBeInTheDocument();
    expect(dialog.getByText('5,678')).toBeInTheDocument();
    expect(dialog.getByText('3.0 TiB')).toBeInTheDocument();
    expect(dialog.getByText('OK')).toBeInTheDocument();
    expect(dialog.getByText('Damaged')).toBeInTheDocument();
    expect(dialog.getByText('Not available')).toBeInTheDocument();
    const json = dialog.getByRole('link', { name: /^Download the manifest .* as JSON$/ });
    const csv = dialog.getByRole('link', { name: /^Download the manifest .* as CSV$/ });
    expect(json).toHaveAttribute('href', '/api/v1/manifests/7/download?format=json');
    expect(csv).toHaveAttribute('href', '/api/v1/manifests/7/download?format=csv');
    expect(json).toHaveAttribute('download');
    // Only the intact version is downloadable.
    expect(dialog.getAllByRole('link', { name: /^Download the manifest/ })).toHaveLength(2);
    // The destination's current view, built on the spot.
    expect(dialog.getByRole('link', { name: 'Current view as JSON' })).toHaveAttribute('href', '/api/v1/manifest/export?format=json&destinationId=1');
    expect(dialog.getByRole('link', { name: 'Current view as CSV' })).toHaveAttribute('href', '/api/v1/manifest/export?format=csv&destinationId=1');
  });

  it('starts an export and links the job; a preview opens its job', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      routes({
        'POST /api/v1/destinations/1/manifest': (init?: RequestInit) => {
          const body = JSON.parse(String(init?.body)) as { dryRun: boolean };
          return { status: 202, body: job({ id: body.dryRun ? 52 : 51, type: 'manifest_export', dryRun: body.dryRun, params: { destinationId: 1 } }) };
        },
        'GET /api/v1/jobs/52': () => ({ body: job({ id: 52, type: 'manifest_export', dryRun: true, params: { destinationId: 1 } }) }),
      }),
    );
    const dialog = await openDialog(user);
    await user.click(dialog.getByRole('button', { name: 'Export now' }));
    expect(await dialog.findByText(/Manifest export of UNAS queued/)).toBeInTheDocument();
    expect(dialog.getByRole('link', { name: 'View job #51' })).toHaveAttribute('href', '/activity/jobs/51');
    expect(callsTo(calls, 'POST /api/v1/destinations/1/manifest')[0].body).toEqual({ dryRun: false });

    await user.click(dialog.getByRole('button', { name: 'Preview' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations/1/manifest')).toHaveLength(2));
    expect(callsTo(calls, 'POST /api/v1/destinations/1/manifest')[1].body).toEqual({ dryRun: true });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: /^Manifests · / })).not.toBeInTheDocument());
  });

  it('shows why an export was refused', async () => {
    const { user } = renderApp(
      '/destinations',
      routes({ 'POST /api/v1/destinations/1/manifest': () => ({ status: 409, body: { message: 'destination "UNAS" is disabled' } }) }),
    );
    const dialog = await openDialog(user);
    await user.click(dialog.getByRole('button', { name: 'Export now' }));
    expect(await dialog.findByText('UNAS: destination "UNAS" is disabled')).toBeInTheDocument();
  });

  it('offers no export for a disabled destination', async () => {
    const { user } = renderApp('/destinations', routes({ 'GET /api/v1/destinations': () => ({ body: [destination({ enabled: false })] }) }));
    const dialog = await openDialog(user);
    expect(dialog.getByRole('button', { name: 'Export now' })).toBeDisabled();
    expect(dialog.getByText(/The destination is disabled/)).toBeInTheDocument();
  });
});

describe('Library', () => {
  it('offers the manifest export as JSON and CSV', async () => {
    renderApp('/library', {
      'GET /api/v1/sources': () => ({ body: [source()] }),
      'GET /api/v1/catalog/stats': () => ({ body: { sources: 1, files: 0, bytes: 0, uniqueBytes: 0, hardlinkGroups: 0, hardlinkedFiles: 0 } }),
      'GET /api/v1/integrations': () => ({ body: [] }),
    });
    expect(await screen.findByRole('link', { name: 'Export manifest as JSON' })).toHaveAttribute('href', '/api/v1/manifest/export?format=json');
    expect(screen.getByRole('link', { name: 'Export manifest as CSV' })).toHaveAttribute('href', '/api/v1/manifest/export?format=csv');
  });
});

/**
 * stubDownloads replaces the browser's object URLs (jsdom has none) and records what saveBlob
 * hands to the browser: the file name and the blob's text.
 */
function stubDownloads() {
  const saved: { filename: string; blob: Blob }[] = [];
  const blobs = new Map<string, Blob>();
  let n = 0;
  Object.defineProperty(URL, 'createObjectURL', {
    configurable: true,
    writable: true,
    value: vi.fn((b: Blob) => {
      const url = `blob:test/${++n}`;
      blobs.set(url, b);
      return url;
    }),
  });
  Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, writable: true, value: vi.fn() });
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    const blob = blobs.get(this.href);
    if (blob) saved.push({ filename: this.download, blob });
  });
  return saved;
}

afterEach(() => {
  Reflect.deleteProperty(URL, 'createObjectURL');
  Reflect.deleteProperty(URL, 'revokeObjectURL');
});

describe('Manifest downloads', () => {
  it('shows the server\'s error instead of saving it as the manifest (429 busy, 409 damaged)', async () => {
    const saved = stubDownloads();
    const { calls, user } = renderApp(
      '/destinations',
      routes({
        'GET /api/v1/manifest/export': () => ({
          status: 429,
          body: { message: 'too many manifest exports are running; try again shortly' },
          headers: { 'Retry-After': '5' },
        }),
        'GET /api/v1/manifests/7/download': () => ({ status: 409, body: { message: 'the manifest version is damaged' } }),
      }),
    );
    const dialog = await openDialog(user);
    await user.click(dialog.getByRole('link', { name: 'Current view as JSON' }));
    expect(await dialog.findByText(/too many manifest exports are running; try again shortly Try again in 5 s\./)).toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/manifest/export')[0].query.get('destinationId')).toBe('1');

    await user.click(await dialog.findByRole('link', { name: /^Download the manifest .* as CSV$/ }));
    expect(await dialog.findByText('the manifest version is damaged')).toBeInTheDocument();
    expect(saved).toHaveLength(0);
  });

  it('saves a good answer under the server\'s file name', async () => {
    const saved = stubDownloads();
    const csv = 'kind,title\nmovie,Heat\n';
    const { user } = renderApp(
      '/destinations',
      routes({
        'GET /api/v1/manifests/7/download': () => ({
          text: csv,
          headers: { 'Content-Type': 'text/csv', 'Content-Disposition': 'attachment; filename="bunkarr-manifest-UNAS-20260925T120000Z.csv"' },
        }),
      }),
    );
    const dialog = await openDialog(user);
    await user.click(await dialog.findByRole('link', { name: /^Download the manifest .* as CSV$/ }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0].filename).toBe('bunkarr-manifest-UNAS-20260925T120000Z.csv');
    expect(await saved[0].blob.text()).toBe(csv);
    expect(dialog.queryByRole('alert')).not.toBeInTheDocument();
  });

  it.each([
    ['attachment; filename="a b.json"', 'a b.json'],
    ['attachment; filename=plain.csv', 'plain.csv'],
    ["attachment; filename*=utf-8''caf%C3%A9.json", 'café.json'],
    ['attachment; filename="../../etc/passwd"', 'passwd'],
    ['attachment', null],
    [null, null],
  ])('filenameFromDisposition(%s) = %s', (header, name) => {
    expect(filenameFromDisposition(header)).toBe(name);
  });
});
