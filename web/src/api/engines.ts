// The restic and rclone engines (docs/design/phase4.md §9.3, §10.1, §12): their availability on
// this server and the engine settings.

import { api } from './client';
import type { EngineSettings, SystemStatus } from './types';

export function getSystemStatus(): Promise<SystemStatus> {
  return api<SystemStatus>('/system/status');
}

export function getEngineSettings(): Promise<EngineSettings> {
  return api<EngineSettings>('/settings/engines');
}

/** updateEngineSettings saves the settings; the slot count applies after a restart (the answer says so). */
export function updateEngineSettings(body: { uploadSlots?: number; retryBudgetMinutes?: number }): Promise<EngineSettings> {
  return api<EngineSettings>('/settings/engines', { method: 'PUT', body });
}

/** Ranges of the engine settings (internal/api/engines.go). */
export const UPLOAD_SLOTS_MAX = 8;
export const RETRY_BUDGET_MAX = 1440;
