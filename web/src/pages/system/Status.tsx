import { useEffect, useState } from 'react';
import { api } from '@/api/client';
import type { SystemStatus } from '@/api/types';
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
