import { describe, expect, it } from 'vitest';
import type { JobParams, JobType } from '@/api/types';
import { JOB_TYPE_LABELS } from './labels';
import { jobTarget, jobTitle, type Names } from './lookups';

const names: Names = {
  source: (id) => (id === 1 ? 'Movies' : `source #${id}`),
  destination: (id) => (id === 7 ? 'NAS' : `destination #${id}`),
  integration: (id) => (id === 3 ? 'Radarr 4K' : `integration #${id}`),
};

describe('jobTarget and jobTitle', () => {
  it.each<[JobType, JobParams, string]>([
    ['sync', { destinationId: 7 }, 'Sync · NAS'],
    ['sync', { destinationId: 7, sourceIds: [1], paths: ['Heat (1995)'] }, 'Sync · NAS (1 path)'],
    ['sync', { destinationId: 7, sourceIds: [1], paths: ['a', 'b'] }, 'Sync · NAS (2 paths)'],
    ['refresh', { integrationId: 3 }, 'Refresh · Radarr 4K'],
    ['refresh', { integrationId: 3, arrItemIds: [5], syncAfter: true }, 'Refresh · Radarr 4K (1 item)'],
    ['refresh', { integrationId: 9, arrItemIds: [5, 6] }, 'Refresh · integration #9 (2 items)'],
    ['arr_backup', { integrationId: 3, destinationId: 7 }, '*arr backup · Radarr 4K → NAS'],
    ['manifest_export', { destinationId: 7 }, 'Manifest export · NAS'],
  ])('%s %j → %s', (type, params, want) => {
    expect(jobTitle({ type, params }, names)).toBe(want);
  });

  it('labels every job type the server runs', () => {
    const types: JobType[] = ['scan', 'sync', 'plexdb_backup', 'retention', 'verify', 'refresh', 'arr_backup', 'manifest_export'];
    for (const t of types) {
      expect(JOB_TYPE_LABELS[t]).toBeTruthy();
    }
    expect(jobTarget({ type: 'refresh', params: {} }, names)).toBe('');
  });
});
