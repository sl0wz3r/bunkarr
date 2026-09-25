// Jobs, items, logs and schedules (design §7 "Jobs and schedules").

import { api, query, toPaged } from './client';
import type { ItemCount, ItemListQuery, Job, JobItem, JobListQuery, JobLog, Paged, Schedule } from './types';

export async function listJobs(q: JobListQuery): Promise<Paged<Job>> {
  return toPaged<Job>(await api<unknown>(`/jobs${query({ ...q })}`), q.page, q.pageSize);
}

export function getJob(id: number): Promise<Job> {
  return api<Job>(`/jobs/${id}`);
}

export function cancelJob(id: number): Promise<unknown> {
  return api<unknown>(`/jobs/${id}/cancel`, { method: 'POST' });
}

export async function listItems(jobId: number, q: ItemListQuery): Promise<Paged<JobItem>> {
  return toPaged<JobItem>(await api<unknown>(`/jobs/${jobId}/items${query({ ...q })}`), q.page, q.pageSize);
}

export async function itemSummary(jobId: number): Promise<ItemCount[]> {
  return (await api<ItemCount[] | null>(`/jobs/${jobId}/items/summary`)) ?? [];
}

export async function jobLogs(jobId: number, afterId: number, limit: number): Promise<JobLog[]> {
  return (await api<JobLog[] | null>(`/jobs/${jobId}/logs${query({ afterId, limit })}`)) ?? [];
}

export async function listSchedules(): Promise<Schedule[]> {
  return (await api<Schedule[] | null>('/schedules')) ?? [];
}

export function updateSchedule(id: number, body: { cron: string; enabled: boolean }): Promise<Schedule> {
  return api<Schedule>(`/schedules/${id}`, { method: 'PUT', body });
}

/** runSchedule queues a schedule's job now; { dryRun: true } queues a preview instead. */
export function runSchedule(id: number, body?: { dryRun: boolean }): Promise<Job> {
  return api<Job>(`/schedules/${id}/run`, { method: 'POST', body });
}
