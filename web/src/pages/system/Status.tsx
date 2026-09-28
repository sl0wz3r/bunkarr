import { useEffect, useState } from 'react';
import { api } from '@/api/client';
import type { EngineAvailability, EngineBinaryStatus, SystemStatus } from '@/api/types';
import { Badge } from '@/components/StatusBadge';
import { Page, Section } from '@/components/Page';

export function formatUptime(seconds: number): string {
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h ${m}m`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m ${Math.floor(seconds % 60)}s`;
}

export function Status() {
  const [status, setStatus] = useState<SystemStatus | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api<SystemStatus>('/system/status')
      .then(setStatus)
      .catch((e: Error) => setError(e.message));
  }, []);

  const rows: [string, string][] = status
    ? [
        ['Version', status.version],
        ['Commit', status.commit],
        ['Build date', status.buildDate || 'unknown'],
        ['Uptime', formatUptime(status.uptimeSeconds)],
        ['Started', new Date(status.startTime).toLocaleString()],
        ['Config directory', status.configDir],
        ['Database', `${status.databasePath} (schema v${status.schemaVersion})`],
        ['Runtime', `${status.goVersion} ${status.os}/${status.arch}${status.isDocker ? ' (Docker)' : ''}`],
      ]
    : [];

  return (
    <Page title="Status">
      {error && <p className="text-danger">{error}</p>}
      {status?.engines && <EnginesSection engines={status.engines} />}
      {status && (
        <Section title="About">
          <dl className="grid grid-cols-[10rem_1fr] gap-x-4 gap-y-2 text-sm">
            {rows.map(([k, v]) => (
              <div key={k} className="contents">
                <dt className="text-ink-muted">{k}</dt>
                <dd className="break-all font-mono">{v}</dd>
              </div>
            ))}
          </dl>
        </Section>
      )}
    </Page>
  );
}

/**
 * EnginesSection shows restic and rclone as found at start-up (phase4.md §10.1, §12): available
 * with their version and path, or why not. Destinations of an unavailable engine run no job.
 */
function EnginesSection({ engines }: { engines: EngineAvailability }) {
  const rows: [string, EngineBinaryStatus][] = [
    ['restic', engines.restic],
    ['rclone', engines.rclone],
  ];
  return (
    <Section title="Engines">
      <ul className="space-y-2 text-sm" aria-label="Engines">
        {rows.map(([name, st]) => (
          <li key={name} className="grid gap-1 sm:grid-cols-[10rem_1fr] sm:gap-x-4">
            <span className="font-medium">{name}</span>
            <span className="min-w-0">
              <span className="flex flex-wrap items-center gap-2">
                {st?.available ? <Badge tone="ok">Available</Badge> : <Badge tone="danger">Not available</Badge>}
                {st?.version && <span className="break-all font-mono text-xs">{st.version}</span>}
              </span>
              {st?.path && <span className="block break-all font-mono text-xs text-ink-muted">{st.path}</span>}
              {!st?.available && (
                <span className="block text-xs text-ink-muted">
                  {st?.reason || `${name} is not installed`}. Destinations that use it run no job; set BUNKARR_{name.toUpperCase()}_PATH or install it, then restart.
                </span>
              )}
            </span>
          </li>
        ))}
      </ul>
    </Section>
  );
}
