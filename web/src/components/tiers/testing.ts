// Fixtures for the tier UI tests (GET /tiers/fields shaped like internal/tiers Fields).

import type { TierField } from '@/api/tiers';

export const testFields: TierField[] = [
  { field: 'arr.tag', label: '*arr tag', ops: ['has', 'hasNot'], valueType: 'string', source: 'arr', available: true, suggestions: [{ value: 'bunkarr-full', label: 'bunkarr-full (Radarr)' }] },
  { field: 'arr.monitored', label: 'Monitored in the *arr', ops: ['is'], valueType: 'bool', source: 'arr', available: true, suggestions: [] },
  { field: 'source', label: 'Source', ops: ['is', 'isNot'], valueType: 'int', source: 'catalog', available: true, suggestions: [{ value: 1, label: 'Movies' }, { value: 2, label: 'TV' }] },
  { field: 'file.size', label: 'File size', ops: ['gt', 'gte', 'lt', 'lte'], valueType: 'int', unit: 'bytes', source: 'catalog', available: true, suggestions: [] },
  { field: 'file.age', label: 'Added', ops: ['olderThan', 'newerThan'], valueType: 'int', unit: 'days', source: 'catalog', available: true, suggestions: [] },
  {
    field: 'tautulli.lastWatched',
    label: 'Last watched (Tautulli)',
    ops: ['olderThan', 'newerThan', 'never'],
    valueType: 'int',
    noValueOps: ['never'],
    unit: 'days',
    source: 'tautulli',
    available: false,
    reason: 'Bunkarr does not read Tautulli yet',
    suggestions: [],
  },
  { field: 'seerr.requestedBy', label: 'Requested by (Seerr)', ops: ['in', 'notIn'], valueType: 'ints', source: 'seerr', available: true, suggestions: [{ value: 4, label: 'alice' }] },
  { field: 'maintainerr.pendingDelete', label: 'Pending deletion in Maintainerr', ops: ['is'], valueType: 'bool', source: 'maintainerr', available: false, reason: 'Bunkarr does not read Maintainerr yet', suggestions: [] },
];
