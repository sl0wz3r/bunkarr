import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { Schedule } from '@/api/types';
import { resticDestination } from '@/test/fixtures';
import { renderApp } from '@/test/render';

// System → Status (engine versions and availability) and System → Tasks (retention schedules of
// engine destinations, a fire skipped for a deferred job) (docs/design/phase4.md §15).

const status = {
  appName: 'Bunkarr',
  version: '0.4.0',
  commit: 'abc',
  buildDate: '',
  startTime: new Date().toISOString(),
  uptimeSeconds: 100,
  databasePath: '/config/bunkarr.db',
  schemaVersion: 4,
  configDir: '/config',
  goVersion: 'go1.26',
  os: 'linux',
  arch: 'amd64',
  isDocker: true,
  authenticationMethod: 'forms',
  authenticationRequired: 'enabled',
  engines: {
    restic: { available: true, version: 'restic 0.18.1', path: '/usr/bin/restic' },
    rclone: { available: false, reason: 'rclone 1.60.0 is too old (1.66 or newer is required)' },
  },
};

describe('System → Status', () => {
  it('shows each engine’s version and availability', async () => {
    renderApp('/system/status', { 'GET /api/v1/system/status': () => ({ body: status }) });
    const engines = within(await screen.findByRole('list', { name: 'Engines' }));
    const restic = within(engines.getByText('restic').closest('li')!);
    expect(restic.getByText('Available')).toBeInTheDocument();
    expect(restic.getByText('restic 0.18.1')).toBeInTheDocument();
    expect(restic.getByText('/usr/bin/restic')).toHaveClass('break-all');
    const rclone = within(engines.getByText('rclone').closest('li')!);
    expect(rclone.getByText('Not available')).toBeInTheDocument();
    expect(rclone.getByText(/rclone 1.60.0 is too old/)).toHaveTextContent(/BUNKARR_RCLONE_PATH/);
  });

  it('leaves the engines out for an older server', async () => {
    renderApp('/system/status', { 'GET /api/v1/system/status': () => ({ body: { ...status, engines: undefined } }) });
    expect(await screen.findByText('About')).toBeInTheDocument();
    expect(screen.queryByRole('list', { name: 'Engines' })).not.toBeInTheDocument();
  });
});

describe('System → Tasks', () => {
  it('lists an engine destination’s retention schedule, its block and a skipped fire', async () => {
    const at = new Date(Date.now() - 3600_000).toISOString();
    const schedules: Schedule[] = [
      {
        id: 1,
        jobType: 'retention',
        params: { destinationId: 9 },
        description: 'Retention · Offsite',
        cron: '30 4 * * *',
        enabled: true,
        lastRunAt: null,
        nextRunAt: null,
        blockedReason: 'destination "Offsite": export and confirm the recovery kit first',
      },
      {
        id: 2,
        jobType: 'verify',
        params: { destinationId: 9 },
        description: 'Verify · Offsite',
        cron: '0 5 * * 0',
        enabled: true,
        lastRunAt: at,
        nextRunAt: new Date(Date.now() + 86400_000).toISOString(),
        blockedReason: '',
        lastSkip: { at, reason: 'a deferred job of this destination waits for its transfer window and covers this run' },
      },
    ];
    renderApp('/system/tasks', {
      'GET /api/v1/schedules': () => ({ body: schedules }),
      'GET /api/v1/destinations': () => ({ body: [resticDestination()] }),
      'GET /api/v1/sources': () => ({ body: [] }),
      'GET /api/v1/integrations': () => ({ body: [] }),
    });
    const retention = within((await screen.findByText('Retention · Offsite')).closest('tr')!);
    expect(retention.getByText('Not running')).toBeInTheDocument();
    expect(retention.getByText(/export and confirm the recovery kit first/)).toBeInTheDocument();
    expect(retention.getByRole('button', { name: 'Run Retention · Offsite now' })).toBeDisabled();
    const verify = within(screen.getByText('Verify · Offsite').closest('tr')!);
    expect(verify.getByText('Skipped')).toBeInTheDocument();
    expect(verify.getByText(/a deferred job of this destination waits for its transfer window/)).toBeInTheDocument();
  });
});
