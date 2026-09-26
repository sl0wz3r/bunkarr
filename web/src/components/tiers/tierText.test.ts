import { describe, expect, it } from 'vitest';
import { uniqueSuggestions, type TierReason, type TierRule } from '@/api/tiers';
import { testFields } from './testing';
import {
  actualText,
  conditionText,
  decisionRuleText,
  draftOf,
  draftProblems,
  emptyRule,
  fieldMap,
  placeWarnings,
  reasonText,
  sameRules,
  sizeParts,
  toInput,
  toInputs,
  valueText,
  type DraftRule,
} from './tierText';

const GiB = 1024 ** 3;

const fields = fieldMap(testFields);

function reason(over: Partial<TierReason>): TierReason {
  return { ruleId: 1, conditionIndex: 0, field: 'arr.tag', op: 'has', value: 'bunkarr-full', result: 'true', source: { kind: 'arr', integrationId: 2 }, ...over };
}

describe('value and reason texts', () => {
  it('renders values by unit and suggestion', () => {
    expect(valueText('file.size', 4 * GiB, fields.get('file.size'))).toBe('4.0 GiB');
    expect(valueText('file.age', 30, fields.get('file.age'))).toBe('30 days');
    expect(valueText('file.age', 1, fields.get('file.age'))).toBe('1 day');
    expect(valueText('source', 2, fields.get('source'))).toBe('TV');
    expect(valueText('arr.monitored', false, fields.get('arr.monitored'))).toBe('no');
    expect(valueText('arr.tag', 'bunkarr-full', fields.get('arr.tag'))).toBe('"bunkarr-full"');
    expect(valueText('plex.section', '1:2', { ...testFields[0], field: 'plex.section', suggestions: [{ value: '1:2', label: 'Plex library 2 (source TV)' }] })).toBe(
      'Plex library 2 (source TV)',
    );
    // A Seerr user appears by id only unless the editor's suggestions name it.
    expect(valueText('seerr.requestedBy', [4, 9], fields.get('seerr.requestedBy'))).toBe('alice, Seerr user #9');
  });

  it('renders conditions in words', () => {
    expect(conditionText({ field: 'file.age', op: 'olderThan', value: 30 }, fields)).toBe('Added more than 30 days ago');
    expect(conditionText({ field: 'file.age', op: 'newerThan', value: 7 }, fields)).toBe('Added within the last 7 days');
    expect(conditionText({ field: 'tautulli.lastWatched', op: 'never' }, fields)).toBe('Last watched (Tautulli) never');
    expect(conditionText({ field: 'unknown.field', op: 'is', value: true }, fields)).toBe('unknown.field is yes');
  });

  it('renders facts: lists, dates, sizes', () => {
    expect(actualText(['4k', 'bunkarr-full'])).toBe('4k, bunkarr-full');
    expect(actualText([])).toBe('none');
    expect(actualText('2026-03-04T10:00:00Z')).toBe('2026-03-04');
    expect(actualText(2 * GiB, 'file.size')).toBe('2.0 GiB');
    expect(actualText(12)).toBe('12');
  });

  it('renders reasons like the job log, with why for unknown', () => {
    expect(reasonText(reason({ actual: ['bunkarr-full', '4k'] }), fields)).toBe('*arr tag has "bunkarr-full": true (bunkarr-full, 4k)');
    expect(
      reasonText(reason({ field: 'maintainerr.pendingDelete', op: 'is', value: true, result: 'unknown', why: 'Maintainerr cache is 30 h old', actual: false }), fields),
    ).toBe('Pending deletion in Maintainerr is yes: unknown (Maintainerr cache is 30 h old)');
    expect(reasonText(reason({ ruleId: 0, conditionIndex: -1, field: 'flag.irreplaceable', op: 'is', value: true, actual: [3, 5] }), fields)).toBe(
      'irreplaceable (flag #3, flag #5)',
    );
  });

  it('names the deciding rule', () => {
    expect(decisionRuleText({ ruleId: 4, ruleName: 'Tagged' })).toBe('rule "Tagged"');
    expect(decisionRuleText({ ruleId: 0, ruleName: 'no rule matched' })).toBe('no rule matched (built-in)');
    expect(decisionRuleText({ ruleId: 0, ruleName: 'irreplaceable' })).toBe('irreplaceable (built-in)');
  });

  it('splits sizes into a whole unit', () => {
    expect(sizeParts(4 * GiB)).toEqual({ amount: 4, unit: GiB });
    expect(sizeParts(512 * 1024 ** 2)).toEqual({ amount: 512, unit: 1024 ** 2 });
    expect(sizeParts(2 * 1024 ** 4)).toEqual({ amount: 2, unit: 1024 ** 4 });
    expect(sizeParts(undefined)).toEqual({ amount: undefined, unit: GiB });
    expect(sizeParts(1000).unit).toBe(GiB);
  });
});

describe('the draft', () => {
  const stored: TierRule = {
    id: 7,
    priority: 1,
    name: 'Tagged',
    enabled: true,
    match: 'all',
    conditions: [
      { field: 'arr.tag', op: 'has', value: 'bunkarr-full' },
      { field: 'tautulli.lastWatched', op: 'never' },
    ],
    action: 'full',
    destinationIds: [3, 1],
    createdAt: '',
    updatedAt: '',
  };

  it('round-trips a stored rule into the PUT body, dropping values of no-value operators', () => {
    const d = draftOf(stored);
    expect(toInput(d, fields)).toEqual({
      id: 7,
      name: 'Tagged',
      enabled: true,
      match: 'all',
      conditions: [
        { field: 'arr.tag', op: 'has', value: 'bunkarr-full' },
        { field: 'tautulli.lastWatched', op: 'never' },
      ],
      action: 'full',
      destinationIds: [1, 3],
    });
    d.conditions[1].value = 5;
    expect(toInput(d, fields).conditions[1]).toEqual({ field: 'tautulli.lastWatched', op: 'never' });
  });

  it('loads a preset rule without an id, with the defaults', () => {
    const d = draftOf({ name: 'Everything else', conditions: [], action: 'manifest', destinationIds: null, id: 9 }, false);
    expect(d.id).toBeUndefined();
    expect(toInput(d, fields)).toEqual({ name: 'Everything else', enabled: true, match: 'all', conditions: [], action: 'manifest', destinationIds: null });
  });

  it('tells a changed draft from the saved one', () => {
    const a = toInputs([draftOf(stored)], fields);
    const b = toInputs([draftOf(stored)], fields);
    expect(sameRules(a, b)).toBe(true);
    const c = draftOf(stored);
    c.action = 'skip';
    expect(sameRules(a, toInputs([c], fields))).toBe(false);
  });

  it('lists incomplete conditions and rules', () => {
    const r: DraftRule = emptyRule(1);
    r.name = ' ';
    r.conditions = [
      { key: 'c1', field: 'file.size', op: 'gt' },
      { key: 'c2', field: 'file.age', op: 'olderThan', value: 1.5 },
      { key: 'c3', field: 'seerr.requestedBy', op: 'in', value: [] },
      { key: 'c4', field: 'tautulli.lastWatched', op: 'never' },
      { key: 'c5', field: 'arr.tag', op: 'is', value: 'x' },
      { key: 'c6', field: 'gone.field', op: 'is', value: true },
      { key: 'c7', field: 'arr.monitored', op: 'is', value: false },
    ];
    const problems = draftProblems([r], fields);
    expect(problems.find((p) => !p.condKey)?.message).toBe('Give the rule a name.');
    const byCond = Object.fromEntries(problems.filter((p) => p.condKey).map((p) => [p.condKey, p.message]));
    expect(byCond).toEqual({
      c1: 'Enter a value.',
      c2: 'Enter a whole number of at least 0.',
      c3: 'Enter a value.',
      c5: 'Choose an operator.',
      c6: 'Unknown field gone.field.',
    });
  });

  it('places save warnings on the saved draft, so they follow a reorder', () => {
    const a = draftOf(stored);
    const b = emptyRule(2);
    const w = placeWarnings(
      [
        { ruleIndex: 0, conditionIndex: 0, message: 'no fresh index knows the tag "bunkarr-full"' },
        { ruleIndex: 1, conditionIndex: -1, message: 'rule level' },
        { ruleIndex: 5, conditionIndex: 0, message: 'ignored' },
      ],
      [a, b],
    );
    expect(w.get(a.conditions[0].key)).toEqual(['no fresh index knows the tag "bunkarr-full"']);
    expect(w.get(b.key)).toEqual(['rule level']);
    expect(w.size).toBe(2);
  });
});

describe('uniqueSuggestions', () => {
  it('lists a Plex section offered by a source and by the library index once, with both labels', () => {
    const f = uniqueSuggestions({
      ...testFields[0],
      field: 'plex.section',
      suggestions: [
        { value: '1:2', label: 'Plex library 2 (source Movies)' },
        { value: '1:3', label: 'Plex library 3 (source TV)' },
        { value: '1:2', label: 'Home: Movies' },
        { value: '1:2', label: 'Home: Movies' },
      ],
    });
    expect(f.suggestions).toEqual([
      { value: '1:2', label: 'Plex library 2 (source Movies) / Home: Movies' },
      { value: '1:3', label: 'Plex library 3 (source TV)' },
    ]);
    expect(valueText('plex.section', '1:2', f)).toBe('Plex library 2 (source Movies) / Home: Movies');
  });
});
