// The release flow on the job page (phase2-3.md §8.5, §16): a release preview's "Apply release"
// and "Apply held changes" of a release job carry releaseDemoted, allowChanges, releaseOf and
// releaseRevision.

import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { ItemCount, Job } from '@/api/types';
import { callsTo, type Handler } from '@/test/fetch';
import { destination, GiB, job, paged, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';

const finished = new Date().toISOString();

const names: Record<string, Handler> = {
  'GET /api/v1/sources': () => ({ body: [source()] }),
  'GET /api/v1/destinations': () => ({ body: [destination({ id: 1, name: 'UNAS' })] }),
  'GET /api/v1/integrations': () => ({ body: [] }),
};

function routesFor(j: Job, summary: ItemCount[], over: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    ...names,
    [`GET /api/v1/jobs/${j.id}`]: () => ({ body: j }),
    [`GET /api/v1/jobs/${j.id}/items/summary`]: () => ({ body: summary }),
    [`GET /api/v1/jobs/${j.id}/items`]: () => ({ body: paged([], 0, 1, 50) }),
    [`GET /api/v1/jobs/${j.id}/logs`]: () => ({ body: [] }),
    ...over,
  };
}

function jobRoutes(j: Job): Record<string, Handler> {
  return routesFor(j, []);
}

const releasePreview = job({
  id: 30,
  dryRun: true,
  status: 'completed',
  finishedAt: finished,
  params: { destinationId: 1, releaseDemoted: true },
  stats: { dryRun: true, filesReleased: 12, bytesReleased: 3 * GiB, filesKept: 12, bytesKept: 3 * GiB, tierRevision: 4, tiers: { full: { files: 90, bytes: 350 * GiB }, manifest: { files: 12, bytes: 3 * GiB }, skip: { files: 0, bytes: 0 }, unknownPromoted: { files: 1, bytes: GiB } } },
});

describe('Release on the job page', () => {
  it('"Apply release" confirms and starts the real run with the preview and its revision', async () => {
    const real = job({ id: 31, status: 'queued', params: { destinationId: 1, allowChanges: true, releaseDemoted: true, releaseOf: 30, releaseRevision: 4 } });
    const { calls, user } = renderApp(
      '/activity/jobs/30',
      routesFor(releasePreview, [{ action: 'retain', status: 'pending', files: 12, bytes: 3 * GiB }], {
        'POST /api/v1/destinations/1/sync': () => ({ status: 202, body: real }),
        ...jobRoutes(real),
      }),
    );
    expect(await screen.findByText('Release preview')).toBeInTheDocument();
    expect(screen.getByText(/12 kept files \(3.0 GiB\) would be released/)).toBeInTheDocument();
    expect(screen.getByText(/Evaluated at rule revision 4/)).toBeInTheDocument();
    // A release preview is applied as a release, never as a plain sync.
    expect(screen.queryByRole('button', { name: 'Run this sync' })).not.toBeInTheDocument();
    // The tier stats of the sync.
    const tiers = screen.getByRole('group', { name: 'Files by tier' });
    expect(within(tiers).getByText('Manifest only')).toBeInTheDocument();
    expect(within(tiers).getByText('Full (unknown)')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Apply release' }));
    const dialog = await screen.findByRole('dialog', { name: 'Apply release' });
    expect(within(dialog).getByText(/Release 12 files \(3.0 GiB\)/)).toBeInTheDocument();
    expect(within(dialog).getByText(/still at revision 4/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Start release' }));

    expect(await screen.findByRole('heading', { name: 'Sync #31' })).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')[0].body).toEqual({
      dryRun: false,
      allowChanges: true,
      releaseDemoted: true,
      releaseOf: 30,
      releaseRevision: 4,
    });
    expect(screen.getByText(/releases the kept files of release preview #30 \(rule revision 4\)/)).toBeInTheDocument();
  });

  it('keeps the dialog open with the reason when the rules changed since the preview (409)', async () => {
    const { user } = renderApp(
      '/activity/jobs/30',
      routesFor(releasePreview, [], {
        'POST /api/v1/destinations/1/sync': () => ({
          status: 409,
          body: { message: 'rules changed since the preview; run the release preview again (the preview evaluated revision 4, the rules are at 5)' },
        }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Apply release' }));
    const dialog = await screen.findByRole('dialog', { name: 'Apply release' });
    await user.click(within(dialog).getByRole('button', { name: 'Start release' }));
    expect(await within(dialog).findByText(/rules changed since the preview; run the release preview again/)).toBeInTheDocument();
  });

  it('runs the release preview again', async () => {
    const again = job({ id: 32, status: 'queued', dryRun: true, params: { destinationId: 1, releaseDemoted: true } });
    const { calls, user } = renderApp(
      '/activity/jobs/30',
      routesFor(releasePreview, [], { 'POST /api/v1/destinations/1/sync': () => ({ status: 202, body: again }), ...jobRoutes(again) }),
    );
    await user.click(await screen.findByRole('button', { name: 'Run the release preview again' }));
    expect(await screen.findByRole('heading', { name: 'Sync #32' })).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')[0].body).toEqual({ dryRun: true, allowChanges: false, releaseDemoted: true });
    // A queued preview cannot be applied yet.
    expect(screen.queryByRole('button', { name: 'Apply release' })).not.toBeInTheDocument();
    expect(screen.getByText('Apply release is offered when the preview has finished.')).toBeInTheDocument();
  });

  it('offers no Apply release for a preview that recorded no revision or did not finish', async () => {
    const noRevision = { ...releasePreview, stats: { dryRun: true, filesReleased: 2, bytesReleased: GiB } };
    renderApp('/activity/jobs/30', routesFor(noRevision, []));
    expect(await screen.findByText(/recorded no rule revision/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Apply release' })).not.toBeInTheDocument();
  });

  it('"Apply held changes" on a release job copies its release params', async () => {
    const heldRelease = job({
      id: 33,
      status: 'completed_with_warnings',
      finishedAt: finished,
      params: { destinationId: 1, releaseDemoted: true, releaseOf: 30, releaseRevision: 4 },
      stats: { filesReleased: 0, bytesReleased: 0, tierRevision: 4 },
    });
    const next = job({ id: 34, status: 'queued', params: { destinationId: 1, allowChanges: true, releaseDemoted: true, releaseOf: 30, releaseRevision: 4 } });
    const { calls, user } = renderApp(
      '/activity/jobs/33',
      routesFor(heldRelease, [{ action: 'retain', status: 'held', files: 30, bytes: 9 * GiB }], {
        'POST /api/v1/destinations/1/sync': () => ({ status: 202, body: next }),
        ...jobRoutes(next),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Apply held changes' }));
    const dialog = await screen.findByRole('dialog', { name: 'Apply held changes' });
    // The confirmation says the sync continues a release.
    expect(within(dialog).getByText(/continues the release of preview #30 \(rule revision 4\)/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Start sync' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')[0].body).toEqual({
      dryRun: false,
      allowChanges: true,
      releaseDemoted: true,
      releaseOf: 30,
      releaseRevision: 4,
    });
  });

  it('offers no "Apply held changes" on a release preview: only "Apply release" (with its release wording) applies it', async () => {
    const { calls, user } = renderApp('/activity/jobs/30', routesFor(releasePreview, [{ action: 'retain', status: 'held', files: 12, bytes: 3 * GiB }]));
    expect(await screen.findByText(/12 changes would be held by the mass-change guard/)).toBeInTheDocument();
    expect(screen.getByText(/"Apply release" above also applies them/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Apply held changes' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Apply release' }));
    expect(within(await screen.findByRole('dialog', { name: 'Apply release' })).getByText(/Release 12 files \(3.0 GiB\)/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')).toHaveLength(0);
  });

  it('shows a dry run skip item\'s tier note', async () => {
    const dry = job({ id: 36, dryRun: true, status: 'completed', finishedAt: finished, params: { destinationId: 1 } });
    renderApp('/activity/jobs/36', {
      ...routesFor(dry, [{ action: 'skip', status: 'pending', files: 1, bytes: GiB }]),
      'GET /api/v1/jobs/36/items': () => ({
        body: paged([{ id: 1, jobId: 36, relPath: 'movies/A.mkv', action: 'skip', status: 'pending', bytes: GiB, detail: { reason: 'not copied', note: 'not copied: manifest (rule "Everything else")' } }], 1, 1, 50),
      }),
    });
    expect(await screen.findByText('reason: not copied · tier: not copied: manifest (rule "Everything else")')).toBeInTheDocument();
    expect(screen.queryByText('Release preview')).not.toBeInTheDocument();
  });
});
