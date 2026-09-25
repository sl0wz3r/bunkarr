// Integrations (Plex) and notifications (design §7 "Integrations", "Notifications").

import { api } from './client';
import type {
  Integration,
  IntegrationInput,
  IntegrationTestInput,
  IntegrationTestResult,
  Job,
  Notification,
  NotificationInput,
  NotificationTestInput,
  PlexSection,
  TestResult,
} from './types';

export async function listIntegrations(): Promise<Integration[]> {
  return (await api<Integration[] | null>('/integrations')) ?? [];
}

export function createIntegration(body: IntegrationInput): Promise<Integration> {
  return api<Integration>('/integrations', { method: 'POST', body });
}

/** updateIntegration saves an integration; an empty or missing apiKey keeps the stored token. */
export function updateIntegration(id: number, body: IntegrationInput): Promise<Integration> {
  return api<Integration>(`/integrations/${id}`, { method: 'PUT', body });
}

export function deleteIntegration(id: number): Promise<void> {
  return api<void>(`/integrations/${id}`, { method: 'DELETE' });
}

export function testIntegration(body: IntegrationTestInput): Promise<IntegrationTestResult> {
  return api<IntegrationTestResult>('/integrations/test', { method: 'POST', body });
}

export async function plexSections(id: number): Promise<PlexSection[]> {
  return (await api<PlexSection[] | null>(`/integrations/${id}/plex/sections`)) ?? [];
}

export function plexBackup(id: number, body: { destinationId: number; dryRun: boolean }): Promise<Job> {
  return api<Job>(`/integrations/${id}/plex/backup`, { method: 'POST', body });
}

export async function listNotifications(): Promise<Notification[]> {
  return (await api<Notification[] | null>('/notifications')) ?? [];
}

export function createNotification(body: NotificationInput): Promise<Notification> {
  return api<Notification>('/notifications', { method: 'POST', body });
}

export function updateNotification(id: number, body: NotificationInput): Promise<Notification> {
  return api<Notification>(`/notifications/${id}`, { method: 'PUT', body });
}

export function deleteNotification(id: number): Promise<void> {
  return api<void>(`/notifications/${id}`, { method: 'DELETE' });
}

export function testNotification(body: NotificationTestInput): Promise<TestResult> {
  return api<TestResult>('/notifications/test', { method: 'POST', body });
}
