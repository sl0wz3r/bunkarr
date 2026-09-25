// Sources, catalog and the read-only filesystem picker (design §7 "Sources and catalog").

import { api, query, toPaged } from './client';
import type { CatalogFile, CatalogStats, DirListing, FileListQuery, Job, Paged, Source, SourceInput, SourceTestResult } from './types';

export async function listSources(): Promise<Source[]> {
  return (await api<Source[] | null>('/sources')) ?? [];
}

export function getSource(id: number): Promise<Source> {
  return api<Source>(`/sources/${id}`);
}

export function createSource(body: SourceInput): Promise<Source> {
  return api<Source>('/sources', { method: 'POST', body });
}

export function updateSource(id: number, body: SourceInput): Promise<Source> {
  return api<Source>(`/sources/${id}`, { method: 'PUT', body });
}

export function deleteSource(id: number): Promise<void> {
  return api<void>(`/sources/${id}`, { method: 'DELETE' });
}

export function testSource(path: string): Promise<SourceTestResult> {
  return api<SourceTestResult>('/sources/test', { method: 'POST', body: { path } });
}

export function scanSource(id: number): Promise<Job> {
  return api<Job>(`/sources/${id}/scan`, { method: 'POST' });
}

export async function listFiles(sourceId: number, q: FileListQuery): Promise<Paged<CatalogFile>> {
  return toPaged<CatalogFile>(await api<unknown>(`/sources/${sourceId}/files${query({ ...q })}`), q.page, q.pageSize);
}

export function catalogStats(): Promise<CatalogStats> {
  return api<CatalogStats>('/catalog/stats');
}

/** browse lists the directories under path ('' = the server's starting point). */
export function browse(path: string): Promise<DirListing> {
  return api<DirListing>(`/filesystem${query({ path })}`);
}
