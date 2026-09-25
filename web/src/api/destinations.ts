// Destinations (design §7 "Destinations").

import { api } from './client';
import type { Destination, DestinationInput, DestinationTestResult, Job, Snapshot } from './types';

export async function listDestinations(): Promise<Destination[]> {
  return (await api<Destination[] | null>('/destinations')) ?? [];
}

export function createDestination(body: DestinationInput): Promise<Destination> {
  return api<Destination>('/destinations', { method: 'POST', body });
}

export function updateDestination(id: number, body: DestinationInput): Promise<Destination> {
  return api<Destination>(`/destinations/${id}`, { method: 'PUT', body });
}

/** deleteDestination forgets a destination; the backup data at the target stays. */
export function deleteDestination(id: number): Promise<void> {
  return api<void>(`/destinations/${id}`, { method: 'DELETE' });
}

/** testTarget probes a target before a destination is created. */
export function testTarget(target: string): Promise<DestinationTestResult> {
  return api<DestinationTestResult>('/destinations/test', { method: 'POST', body: { target } });
}

/** testDestination probes an existing destination (marker, capabilities, free space). */
export function testDestination(id: number): Promise<DestinationTestResult> {
  return api<DestinationTestResult>(`/destinations/${id}/test`, { method: 'POST' });
}

export function syncDestination(id: number, opts: { dryRun: boolean; allowChanges: boolean }): Promise<Job> {
  return api<Job>(`/destinations/${id}/sync`, { method: 'POST', body: opts });
}

export function verifyDestination(id: number): Promise<Job> {
  return api<Job>(`/destinations/${id}/verify`, { method: 'POST' });
}

export async function listSnapshots(id: number): Promise<Snapshot[]> {
  return (await api<Snapshot[] | null>(`/destinations/${id}/snapshots`)) ?? [];
}
