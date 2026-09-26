import { screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { ItemFlag, TierPreset, TierPreview, TierPreviewItem, TierRule, TierRuleSet } from '@/api/tiers';
import { testFields } from '@/components/tiers/testing';
import { callsTo, type Handler } from '@/test/fetch';
import { destination, GiB, job, paged, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';

const now = new Date().toISOString();

function rule(over: Partial<TierRule>): TierRule {
  return { id: 1, priority: 1, name: 'Rule', enabled: true, match: 'all', conditions: [], action: 'manifest', destinationIds: null, createdAt: now, updatedAt: now, ...over };
}

const bigFiles = rule({ id: 11, priority: 1, name: 'Big files', conditions: [{ field: 'file.size', op: 'gt', value: 50 * GiB }], action: 'skip', destinationIds: [1] });
const tagged = rule({ id: 12, priority: 2, name: 'Tagged', conditions: [{ field: 'arr.tag', op: 'has', value: 'keep' }], action: 'full' });

const presets: TierPreset[] = [
  { id: 'everything', name: 'Back up everything', description: 'The default: no rules.', rules: [] },
  {
    id: 'manifest-by-default',
    name: 'Manifest by default',
    description: 'Only media tagged bunkarr-full is copied in full. This stops copying new untagged media.',
    rules: [
      { name: 'Tagged bunkarr-full', match: 'all', action: 'full', conditions: [{ field: 'arr.tag', op: 'has', value: 'bunkarr-full' }], destinationIds: null },
      { name: 'Everything else', match: 'all', action: 'manifest', conditions: [], destinationIds: null },
    ],
  },
  {
    id: 'maintainerr-manifest',
    name: 'Keep Maintainerr deletions as manifest only',
    description: 'Maintainerr has no authentication.',
    rules: [{ name: 'Pending deletion in Maintainerr', match: 'all', action: 'manifest', conditions: [{ field: 'maintainerr.pendingDelete', op: 'is', value: true }], destinationIds: null }],
  },
];

const count = (files: number, bytes: number) => ({ files, bytes });

function previewOf(over: Partial<TierPreview> = {}, kept = count(0, 0)): TierPreview {
  return {
    id: '0123456789abcdef0123456789abcdef',
    revision: 'draft',
    createdAt: now,
    unknownSources: [],
    staleReferences: [],
    destinations: [
      {
        destinationId: 1,
        name: 'UNAS',
        stored: count(100, 400 * GiB),
        full: { ...count(90, 350 * GiB), uniqueBytes: 340 * GiB },
        manifest: count(8, 40 * GiB),
        skip: count(2, 120 * GiB),
        unknownPromoted: count(3, 6 * GiB),
        toCopy: count(4, 5 * GiB),
        kept,
        movedToNonFull: count(0, 0),
        byRule: [
          { ruleId: 12, name: 'Tagged', action: 'full', files: 20, bytes: 60 * GiB },
          { ruleId: 11, name: 'Big files', action: 'skip', files: 2, bytes: 120 * GiB },
          { ruleId: 0, name: 'no rule matched', action: 'full', files: 70, bytes: 290 * GiB },
        ],
        configBackups: [{ kind: 'plexdb', integrationId: 3, name: 'Plex', lastBytes: 2 * GiB }],
      },
    ],
    ...over,
  };
}

const unknownItem: TierPreviewItem = {
  destinationId: 1,
  fileId: 501,
  sourceId: 1,
  sourceName: 'Movies',
  relPath: 'Heat (1995)/Heat.mkv',
  size: 60 * GiB,
  tier: 'full',
  ruleId: 11,
  ruleName: 'Big files',
  reasons: [
    { ruleId: 11, conditionIndex: 0, field: 'file.size', op: 'gt', value: 50 * GiB, result: 'unknown', source: { kind: 'arr', integrationId: 2 }, why: 'Radarr cache is 31 h old' },
  ],
  unknown: [],
  unknownPromoted: true,
  state: 'to-copy',
};

const flags: ItemFlag[] = [
  {
    id: 3,
    flag: 'irreplaceable',
    kind: 'path',
    integrationId: null,
    externalIds: {},
    lastSourceId: null,
    lastRelPath: null,
    sourceId: 1,
    relPath: 'Home Videos',
    note: 'wedding',
    createdAt: now,
    updatedAt: now,
    resolved: true,
  },
  {
    id: 4,
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
    note: '',
    createdAt: now,
    updatedAt: now,
    resolved: false,
    reason: 'no item with tmdb 603',
  },
];

function routes(set: TierRuleSet, over: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/tiers/rules': () => ({ body: set }),
    'GET /api/v1/tiers/fields': () => ({ body: testFields }),
    'GET /api/v1/tiers/presets': () => ({ body: presets }),
    'GET /api/v1/tiers/flags': () => ({ body: flags }),
    'GET /api/v1/destinations': () => ({ body: [destination({ id: 1, name: 'UNAS' }), destination({ id: 2, name: 'Offsite' })] }),
    'GET /api/v1/sources': () => ({ body: [source({ id: 1, name: 'Movies' })] }),
    'POST /api/v1/tiers/preview': () => ({ body: previewOf() }),
    'GET /api/v1/tiers/preview/0123456789abcdef0123456789abcdef/items': () => ({ body: paged([unknownItem], 1, 1, 50) }),
    ...over,
  };
}

describe('Settings → Tiers', () => {
  it('starts with no rules (every file full) and only loads a preset into the editor', async () => {
    const { calls, user } = renderApp('/settings/tiers', routes({ revision: 3, rules: [] }));
    expect(await screen.findByText('No rules: every file is copied in full to every destination (the default).')).toBeInTheDocument();
    expect(screen.getByText('Irreplaceable')).toBeInTheDocument();
    expect(screen.getByText('Everything else')).toBeInTheDocument();
    expect(screen.getByText('Plex DB and *arr config backups are always full: they are not tiered.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();

    await user.selectOptions(screen.getByLabelText('Preset'), 'manifest-by-default');
    await user.click(screen.getByRole('button', { name: 'Load into editor' }));
    expect(await screen.findByRole('listitem', { name: 'Rule 1: Tagged bunkarr-full' })).toBeInTheDocument();
    expect(screen.getByRole('listitem', { name: 'Rule 2: Everything else' })).toBeInTheDocument();
    expect(screen.getByText('Preset loaded into the editor: Manifest by default')).toBeInTheDocument();
    expect(screen.getByText(/This stops copying new untagged media/)).toBeInTheDocument();
    expect(screen.getByText('Unsaved changes')).toBeInTheDocument();
    // Loading a preset saves and previews nothing.
    expect(callsTo(calls, 'PUT /api/v1/tiers/rules')).toHaveLength(0);
    expect(callsTo(calls, 'POST /api/v1/tiers/preview')).toHaveLength(0);

    // Loading another preset over a draft asks first; its unavailable field is shown with the reason.
    await user.selectOptions(screen.getByLabelText('Preset'), 'maintainerr-manifest');
    await user.click(screen.getByRole('button', { name: 'Load into editor' }));
    const dialog = await screen.findByRole('dialog', { name: 'Load preset' });
    expect(within(dialog).getByText(/Nothing is saved or applied/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Load into editor' }));
    const card = await screen.findByRole('listitem', { name: 'Rule 1: Pending deletion in Maintainerr' });
    expect(within(card).getByText(/Not available: Bunkarr does not read Maintainerr yet/)).toBeInTheDocument();
    expect(callsTo(calls, 'PUT /api/v1/tiers/rules')).toHaveLength(0);

    // Save is the only thing that applies it.
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/tiers/rules')).toHaveLength(1));
    expect(callsTo(calls, 'PUT /api/v1/tiers/rules')[0].body).toEqual({
      revision: 3,
      rules: [
        {
          name: 'Pending deletion in Maintainerr',
          enabled: true,
          match: 'all',
          conditions: [{ field: 'maintainerr.pendingDelete', op: 'is', value: true }],
          action: 'manifest',
          destinationIds: null,
        },
      ],
    });
  });

  it('edits, reorders and scopes rules, saves them with the revision and shows warnings next to their conditions', async () => {
    let saved: unknown = null;
    const { calls, user } = renderApp(
      '/settings/tiers',
      routes(
        { revision: 5, rules: [bigFiles, tagged] },
        {
          'PUT /api/v1/tiers/rules': (init) => {
            saved = JSON.parse(String(init?.body));
            return {
              body: {
                revision: 6,
                rules: [
                  { ...tagged, priority: 1, conditions: [...tagged.conditions, { field: 'arr.monitored', op: 'is', value: false }] },
                  { ...bigFiles, priority: 2 },
                ],
                warnings: [{ ruleIndex: 0, conditionIndex: 0, message: 'no fresh index knows the tag "keep"' }],
              },
            };
          },
          'POST /api/v1/tiers/preview': () => ({ body: previewOf({ revision: 6 }) }),
        },
      ),
    );
    const big = await screen.findByRole('listitem', { name: 'Rule 1: Big files' });
    // The size reads in its unit; the scope is the chosen destination.
    expect(within(big).getByLabelText('Rule 1 condition 1 value')).toHaveValue(50);
    expect(within(big).getByLabelText('Rule 1 condition 1 value unit')).toHaveValue(String(GiB));
    expect(within(big).getByLabelText('Rule 1 applies at UNAS')).toBeChecked();
    expect(within(big).getByLabelText('Rule 1 applies at Offsite')).not.toBeChecked();
    // Unavailable fields are listed disabled, with the reason.
    const fieldSelect = within(big).getByLabelText('Rule 1 condition 1 field');
    expect(within(fieldSelect).getByRole('option', { name: 'Last watched (Tautulli) — not available: Bunkarr does not read Tautulli yet' })).toBeDisabled();
    expect(within(fieldSelect).getByRole('option', { name: 'Added' })).toBeEnabled();

    // An emptied "Applies at" is flagged: it applies nowhere, never everywhere.
    await user.click(within(big).getByLabelText('Rule 1 applies at UNAS'));
    expect(within(big).getByText(/This rule applies at no destination/)).toBeInTheDocument();
    await user.click(within(big).getByLabelText('Rule 1 applies at UNAS'));

    await user.click(screen.getByRole('button', { name: 'Move rule 2 up' }));
    const first = screen.getByRole('listitem', { name: 'Rule 1: Tagged' });
    expect(screen.getByText('Unsaved changes')).toBeInTheDocument();
    await user.click(within(first).getByRole('button', { name: 'Add condition to rule 1' }));
    await user.selectOptions(within(first).getByLabelText('Rule 1 condition 2 field'), 'arr.monitored');
    expect(within(first).getByLabelText('Rule 1 condition 2 value')).toHaveValue('true');
    await user.selectOptions(within(first).getByLabelText('Rule 1 condition 2 value'), 'false');

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(saved).not.toBeNull());
    expect(saved).toEqual({
      revision: 5,
      rules: [
        {
          id: 12,
          name: 'Tagged',
          enabled: true,
          match: 'all',
          conditions: [
            { field: 'arr.tag', op: 'has', value: 'keep' },
            { field: 'arr.monitored', op: 'is', value: false },
          ],
          action: 'full',
          destinationIds: null,
        },
        { id: 11, name: 'Big files', enabled: true, match: 'all', conditions: [{ field: 'file.size', op: 'gt', value: 50 * GiB }], action: 'skip', destinationIds: [1] },
      ],
    });
    expect(await screen.findByText(/Saved \(revision 6\)/)).toBeInTheDocument();
    const saved1 = screen.getByRole('listitem', { name: 'Rule 1: Tagged' });
    expect(within(saved1).getByText('Warning: no fresh index knows the tag "keep"')).toBeInTheDocument();
    expect(screen.queryByText('Unsaved changes')).not.toBeInTheDocument();
    // The saved rules are previewed at once, for what they leave kept.
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/tiers/preview')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/tiers/preview')[0].body).toEqual({});
  });

  it('offers a reload when the save is refused because the rules changed elsewhere (409)', async () => {
    let revision = 5;
    const { calls, user } = renderApp(
      '/settings/tiers',
      routes(
        { revision: 5, rules: [bigFiles] },
        {
          'GET /api/v1/tiers/rules': () => ({ body: revision === 5 ? { revision: 5, rules: [bigFiles] } : { revision: 7, rules: [tagged] } }),
          'PUT /api/v1/tiers/rules': () => {
            revision = 7;
            return { status: 409, body: { message: 'the tier rules changed since they were loaded; reload them and try again' } };
          },
        },
      ),
    );
    const card = await screen.findByRole('listitem', { name: 'Rule 1: Big files' });
    await user.selectOptions(within(card).getByLabelText('Rule 1 action'), 'manifest');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('The rules were changed elsewhere')).toBeInTheDocument();
    expect(callsTo(calls, 'PUT /api/v1/tiers/rules')[0].body).toMatchObject({ revision: 5 });

    await user.click(screen.getByRole('button', { name: 'Reload rules' }));
    expect(await screen.findByRole('listitem', { name: 'Rule 1: Tagged' })).toBeInTheDocument();
    expect(screen.queryByRole('listitem', { name: 'Rule 1: Big files' })).not.toBeInTheDocument();
    expect(screen.queryByText('The rules were changed elsewhere')).not.toBeInTheDocument();
    expect(screen.queryByText('Unsaved changes')).not.toBeInTheDocument();
    expect(callsTo(calls, 'GET /api/v1/tiers/rules').length).toBeGreaterThanOrEqual(2);
  });

  it('shows the server\'s refusal of an invalid rule', async () => {
    const { user } = renderApp(
      '/settings/tiers',
      routes({ revision: 5, rules: [bigFiles] }, { 'PUT /api/v1/tiers/rules': () => ({ status: 400, body: { message: 'rule 1, condition 1: the value of file.size must be a whole number of at least 0' } }) }),
    );
    const card = await screen.findByRole('listitem', { name: 'Rule 1: Big files' });
    await user.selectOptions(within(card).getByLabelText('Rule 1 action'), 'manifest');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('rule 1, condition 1: the value of file.size must be a whole number of at least 0')).toBeInTheDocument();
    expect(screen.getByText('Unsaved changes')).toBeInTheDocument();
  });

  it('keeps an incomplete draft from being saved or previewed', async () => {
    const { calls, user } = renderApp('/settings/tiers', routes({ revision: 1, rules: [] }));
    await user.click(await screen.findByRole('button', { name: 'Add rule' }));
    const card = screen.getByRole('listitem', { name: 'Rule 1: Rule 1' });
    await user.click(within(card).getByRole('button', { name: 'Add condition to rule 1' }));
    await user.selectOptions(within(card).getByLabelText('Rule 1 condition 1 field'), 'file.age');
    expect(within(card).getByText('Enter a value.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Preview' })).toBeDisabled();
    await user.type(within(card).getByLabelText('Rule 1 condition 1 value'), '30');
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: 'Preview' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/tiers/preview')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/tiers/preview')[0].body).toEqual({
      rules: [{ name: 'Rule 1', enabled: true, match: 'all', conditions: [{ field: 'file.age', op: 'olderThan', value: 30 }], action: 'manifest', destinationIds: null }],
    });
    expect(callsTo(calls, 'PUT /api/v1/tiers/rules')).toHaveLength(0);
  });

  it('previews the draft: counts per destination, unknown and stale banners, files with their rule and reasons, filters', async () => {
    let expired = false;
    const { calls, user } = renderApp(
      '/settings/tiers',
      routes(
        { revision: 5, rules: [bigFiles, tagged] },
        {
          'POST /api/v1/tiers/preview': () => ({
            body: previewOf({
              unknownSources: [{ integrationId: 2, name: 'Radarr', reason: 'cache is 31 h old' }],
              staleReferences: [{ ruleIndex: 1, conditionIndex: 0, message: 'no fresh index knows the tag "keep"' }],
            }),
          }),
          'GET /api/v1/tiers/preview/0123456789abcdef0123456789abcdef/items': () =>
            expired ? { status: 404, body: { message: 'preview not found' } } : { body: paged([unknownItem], 1, 1, 50) },
        },
      ),
    );
    const card = await screen.findByRole('listitem', { name: 'Rule 2: Tagged' });
    await user.clear(within(card).getByLabelText('Rule 2 condition 1 value'));
    await user.type(within(card).getByLabelText('Rule 2 condition 1 value'), 'keepers');
    await user.click(screen.getByRole('button', { name: 'Preview' }));

    const panel = await screen.findByRole('region', { name: 'Preview' });
    expect(callsTo(calls, 'POST /api/v1/tiers/preview')[0].body).toMatchObject({ rules: [{ id: 11 }, { id: 12, conditions: [{ value: 'keepers' }] }] });
    expect(within(panel).getByText('Draft (not saved)', { exact: false })).toBeInTheDocument();
    const stats = within(panel).getByRole('group', { name: 'Preview for UNAS' });
    for (const label of ['Stored', 'Full', 'Manifest', 'Skip', 'Unknown', 'To copy', 'Kept', 'Moved to a non-full location']) {
      expect(within(stats).getByText(label)).toBeInTheDocument();
    }
    expect(within(stats).getByText('3 files · 6.0 GiB')).toBeInTheDocument();
    expect(within(panel).getByText('Radarr: cache is 31 h old')).toBeInTheDocument();
    expect(within(panel).getByText('Rule 2, condition 1: no fresh index knows the tag "keep"')).toBeInTheDocument();
    const byRule = within(panel).getByRole('table', { name: 'Files by rule at UNAS' });
    expect(within(byRule).getByText('no rule matched (built-in)')).toBeInTheDocument();
    expect(within(panel).getByText('Plex DB · Plex')).toBeInTheDocument();

    // The files: tier, the unknown-promoted flag, the deciding rule and its reasons with why.
    const files = within(panel).getByRole('table', { name: 'Preview files' });
    expect(await within(files).findByRole('link', { name: 'Heat (1995)/Heat.mkv' })).toHaveAttribute('href', '/library/files/501');
    expect(within(files).getByText('Unknown → full')).toBeInTheDocument();
    expect(within(files).getByText('rule "Big files"')).toBeInTheDocument();
    expect(within(files).getByText('File size more than 50.0 GiB: unknown (Radarr cache is 31 h old)')).toBeInTheDocument();
    expect(within(files).getByText('To copy')).toBeInTheDocument();

    await user.selectOptions(within(panel).getByLabelText('Tier'), 'manifest');
    await waitFor(() => expect(callsTo(calls, 'GET /api/v1/tiers/preview/0123456789abcdef0123456789abcdef/items').at(-1)!.query.get('tier')).toBe('manifest'));
    await user.selectOptions(within(panel).getByLabelText('Rule'), '0');
    await waitFor(() => expect(callsTo(calls, 'GET /api/v1/tiers/preview/0123456789abcdef0123456789abcdef/items').at(-1)!.query.get('ruleId')).toBe('0'));
    await user.selectOptions(within(panel).getByLabelText('State'), 'kept');
    await waitFor(() => expect(callsTo(calls, 'GET /api/v1/tiers/preview/0123456789abcdef0123456789abcdef/items').at(-1)!.query.get('state')).toBe('kept'));

    // The draft changed since: the preview says so. An expired preview offers to run it again.
    await user.type(within(card).getByLabelText('Rule 2 condition 1 value'), '2');
    expect(within(panel).getByText('The rules changed since this preview')).toBeInTheDocument();
    expired = true;
    await user.selectOptions(within(panel).getByLabelText('State'), 'stored');
    expect(await within(panel).findByText('This preview expired')).toBeInTheDocument();
    await user.click(within(panel).getAllByRole('button', { name: 'Preview again' })[0]);
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/tiers/preview')).toHaveLength(2));
  });

  it('offers the release of kept files after a save: a dry run with releaseDemoted, then its job', async () => {
    const releaseJob = job({ id: 40, status: 'queued', dryRun: true, params: { destinationId: 1, releaseDemoted: true } });
    const { calls, user } = renderApp(
      '/settings/tiers',
      routes(
        { revision: 5, rules: [tagged] },
        {
          'PUT /api/v1/tiers/rules': () => ({ body: { revision: 6, rules: [{ ...tagged, action: 'manifest' }], warnings: [] } }),
          'POST /api/v1/tiers/preview': () => ({ body: previewOf({ revision: 6 }, count(12, 3 * GiB)) }),
          'POST /api/v1/destinations/1/sync': () => ({ status: 202, body: releaseJob }),
          'GET /api/v1/jobs/40': () => ({ body: releaseJob }),
          'GET /api/v1/jobs/40/items/summary': () => ({ body: [] }),
          'GET /api/v1/jobs/40/items': () => ({ body: paged([]) }),
          'GET /api/v1/jobs/40/logs': () => ({ body: [] }),
          'GET /api/v1/integrations': () => ({ body: [] }),
        },
      ),
    );
    const card = await screen.findByRole('listitem', { name: 'Rule 1: Tagged' });
    await user.selectOptions(within(card).getByLabelText('Rule 1 action'), 'manifest');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await user.click(await screen.findByRole('button', { name: 'Release 12 files (3.0 GiB) at UNAS…' }));
    const dialog = await screen.findByRole('dialog', { name: 'Release kept files' });
    expect(within(dialog).getByText(/nothing is changed/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Preview the release' }));

    expect(await screen.findByRole('heading', { name: 'Sync #40' })).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/1/sync')[0].body).toEqual({ dryRun: true, allowChanges: false, releaseDemoted: true });
    expect(screen.getByText('Release preview')).toBeInTheDocument();
  });

  it('lists the irreplaceable flags with why one does not resolve, and removes one', async () => {
    let list = flags;
    const { calls, user } = renderApp(
      '/settings/tiers',
      routes(
        { revision: 1, rules: [] },
        {
          'GET /api/v1/tiers/flags': () => ({ body: list }),
          'DELETE /api/v1/tiers/flags/3': () => {
            list = flags.slice(1);
            return { status: 204 };
          },
        },
      ),
    );
    await user.click(await screen.findByRole('button', { name: '2 flags' }));
    const table = screen.getByRole('table', { name: 'Irreplaceable flags' });
    expect(within(table).getByText('Movies: Home Videos')).toBeInTheDocument();
    expect(within(table).getByText('wedding')).toBeInTheDocument();
    expect(within(table).getByText('movie (tmdb 603)')).toBeInTheDocument();
    expect(within(table).getByText('Last folder: Movies: The Matrix (1999)')).toBeInTheDocument();
    expect(within(table).getByText('no item with tmdb 603')).toBeInTheDocument();

    await user.click(within(table).getByRole('button', { name: 'Remove flag #3' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove flag' });
    await user.click(within(dialog).getByRole('button', { name: 'Remove flag' }));
    await waitFor(() => expect(callsTo(calls, 'DELETE /api/v1/tiers/flags/3')).toHaveLength(1));
    expect(await screen.findByRole('button', { name: '1 flag' })).toBeInTheDocument();
  });

  it('is in the Settings navigation', async () => {
    renderApp('/settings/general', {
      'GET /api/v1/settings': () => ({ body: {} }),
      'GET /api/v1/notifications': () => ({ body: [] }),
    });
    expect(await screen.findByRole('link', { name: 'Tiers' })).toHaveAttribute('href', '/settings/tiers');
  });
});

describe('Settings → Tiers: the Seerr "Requested by" picker', () => {
  it('shows a stored user id no live user has, checked, and lets it be unticked; one checkbox per id', async () => {
    const requested = rule({ id: 21, priority: 1, name: 'Requested', conditions: [{ field: 'seerr.requestedBy', op: 'in', value: [7] }], action: 'full' });
    // Two Seerr servers both have user 4: the rule's id matches either, so it is one checkbox.
    const fields = testFields.map((f) =>
      f.field === 'seerr.requestedBy'
        ? {
            ...f,
            suggestions: [
              { value: 4, label: 'alice' },
              { value: 4, label: 'bob' },
            ],
          }
        : f,
    );
    let saved: unknown = null;
    const { user } = renderApp(
      '/settings/tiers',
      routes(
        { revision: 2, rules: [requested] },
        {
          'GET /api/v1/tiers/fields': () => ({ body: fields }),
          'PUT /api/v1/tiers/rules': (init) => {
            saved = JSON.parse(String(init?.body));
            return { body: { revision: 3, rules: [requested] } };
          },
        },
      ),
    );
    const card = await screen.findByRole('listitem', { name: 'Rule 1: Requested' });
    const picker = within(within(card).getByRole('group', { name: 'Rule 1 condition 1 value' }));
    expect(picker.getAllByRole('checkbox')).toHaveLength(2);
    expect(picker.getByRole('checkbox', { name: 'Seerr user #7 (not found)' })).toBeChecked();
    expect(picker.getByRole('checkbox', { name: 'alice / bob' })).not.toBeChecked();

    await user.click(picker.getByRole('checkbox', { name: 'alice / bob' }));
    await user.click(picker.getByRole('checkbox', { name: 'Seerr user #7 (not found)' }));
    // Unticked, the unknown id is gone from the rule (and from the list).
    expect(picker.queryByRole('checkbox', { name: 'Seerr user #7 (not found)' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(saved).not.toBeNull());
    expect((saved as { rules: { conditions: unknown[] }[] }).rules[0].conditions).toEqual([{ field: 'seerr.requestedBy', op: 'in', value: [4] }]);
  });
});

describe('Settings → Tiers: the preview rule filter', () => {
  it('names both built-in decisions (they share ruleId 0) in its one built-in option', async () => {
    const withIrreplaceable = previewOf({ revision: 2 });
    withIrreplaceable.destinations![0].byRule = [
      { ruleId: 12, name: 'Tagged', action: 'full', files: 20, bytes: 60 * GiB },
      { ruleId: 0, name: 'irreplaceable', action: 'full', files: 1, bytes: GiB },
      { ruleId: 0, name: 'no rule matched', action: 'full', files: 70, bytes: 290 * GiB },
    ];
    const { user } = renderApp('/settings/tiers', routes({ revision: 2, rules: [tagged] }, { 'POST /api/v1/tiers/preview': () => ({ body: withIrreplaceable }) }));
    await screen.findByRole('listitem', { name: 'Rule 1: Tagged' });
    await user.click(screen.getByRole('button', { name: 'Preview' }));
    const panel = await screen.findByRole('region', { name: 'Preview' });
    const filter = within(panel).getByLabelText('Rule');
    expect(within(filter).getAllByRole('option').map((o) => o.textContent)).toEqual(['All rules', 'Tagged', 'irreplaceable or no rule matched (built-in)']);
  });
});
