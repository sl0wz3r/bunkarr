// Shared list queries (sources, destinations, integrations) and the job description built from
// them. Pages that only need names use these so the lists are fetched once and cached.

import { useQuery } from '@tanstack/react-query';
import { listDestinations } from '@/api/destinations';
import { listIntegrations } from '@/api/integrations';
import { listSources } from '@/api/library';
import type { Destination, Integration, Job, Source } from '@/api/types';
import { JOB_TYPE_LABELS } from './labels';

export const keys = {
  sources: ['sources'] as const,
  destinations: ['destinations'] as const,
  integrations: ['integrations'] as const,
  notifications: ['notifications'] as const,
  schedules: ['schedules'] as const,
  catalogStats: ['catalog', 'stats'] as const,
  jobs: ['jobs'] as const,
};

export function useSources() {
  return useQuery({ queryKey: keys.sources, queryFn: listSources });
}

export function useDestinations() {
  return useQuery({ queryKey: keys.destinations, queryFn: listDestinations });
}

export function useIntegrations() {
  return useQuery({ queryKey: keys.integrations, queryFn: listIntegrations });
}

/** Names maps ids to display names; a missing entry falls back to "#id". */
export interface Names {
  source: (id: number) => string;
  destination: (id: number) => string;
  integration: (id: number) => string;
}

function namer<T extends { id: number; name: string }>(list: T[] | undefined, kind: string) {
  const byId = new Map((list ?? []).map((x) => [x.id, x.name]));
  return (id: number) => byId.get(id) ?? `${kind} #${id}`;
}

/** useNames loads the lists behind job descriptions (errors fall back to ids). */
export function useNames(): Names {
  const sources = useSources();
  const destinations = useDestinations();
  const integrations = useIntegrations();
  return {
    source: namer<Source>(sources.data, 'source'),
    destination: namer<Destination>(destinations.data, 'destination'),
    integration: namer<Integration>(integrations.data, 'integration'),
  };
}

/** jobTarget says what a job works on: "NAS", "Movies, TV", "Plex → NAS". */
export function jobTarget(job: Pick<Job, 'type' | 'params'>, names: Names): string {
  const p = job.params ?? {};
  switch (job.type) {
    case 'scan':
      return (p.sourceIds ?? []).map(names.source).join(', ') || 'all sources';
    case 'sync': {
      const to = p.destinationId ? names.destination(p.destinationId) : '';
      const n = p.paths?.length ?? 0;
      return n > 0 ? `${to} (${n === 1 ? '1 path' : `${n} paths`})` : to;
    }
    case 'verify':
    case 'manifest_export':
      return p.destinationId ? names.destination(p.destinationId) : '';
    case 'retention':
      return p.destinationId ? names.destination(p.destinationId) : 'all destinations';
    case 'plexdb_backup': {
      const from = p.integrationId ? names.integration(p.integrationId) : 'Plex';
      return p.destinationId ? `${from} → ${names.destination(p.destinationId)}` : from;
    }
    case 'arr_backup': {
      const from = p.integrationId ? names.integration(p.integrationId) : '*arr';
      return p.destinationId ? `${from} → ${names.destination(p.destinationId)}` : from;
    }
    case 'refresh': {
      const of = p.integrationId ? names.integration(p.integrationId) : '';
      const items = p.arrItemIds?.length ?? 0;
      return items > 0 ? `${of} (${items === 1 ? '1 item' : `${items} items`})` : of;
    }
    default:
      return '';
  }
}

/** jobTitle is "Sync · NAS"; the type label alone when there is no target. */
export function jobTitle(job: Pick<Job, 'type' | 'params'>, names: Names): string {
  const target = jobTarget(job, names);
  const label = JOB_TYPE_LABELS[job.type] ?? job.type;
  return target ? `${label} · ${target}` : label;
}

/** isActive reports whether a job is queued or running. */
export function isActive(job: Pick<Job, 'status'>): boolean {
  return job.status === 'queued' || job.status === 'running';
}
