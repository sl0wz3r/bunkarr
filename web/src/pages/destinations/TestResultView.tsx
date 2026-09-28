import type { Capabilities, DestinationTestResult, EngineTestResult, RemoteMarkerState, RepositoryState } from '@/api/types';
import { Notice, WarningList } from '@/components/Notice';
import { Badge, MarkerBadge, type Tone } from '@/components/StatusBadge';
import { formatBytes, formatNumber, formatPercent } from '@/lib/format';

/** granularity renders the filesystem's mtime granularity ("1 ns", "100 ns", "1 s"). */
function granularity(ns: number | undefined): string {
  if (!ns || ns <= 0) return 'unknown';
  if (ns >= 1e9) return `${ns / 1e9} s`;
  if (ns >= 1e6) return `${ns / 1e6} ms`;
  if (ns >= 1e3) return `${ns / 1e3} µs`;
  return `${ns} ns`;
}

/** CapabilityBadges summarizes what a destination filesystem can store (design §3). */
export function CapabilityBadges({ caps }: { caps: Partial<Capabilities> | null | undefined }) {
  if (!caps || Object.keys(caps).length === 0) {
    return <Badge>Not probed</Badge>;
  }
  return (
    <span className="inline-flex flex-wrap gap-1">
      {caps.hardlinks ? <Badge tone="ok">Hardlinks</Badge> : <Badge title="Hardlinked names are recorded, not recreated">No hardlinks</Badge>}
      {caps.unstableInodes && (
        <Badge
          tone="warn"
          title="The names of one file show different inode numbers here (an SMB mount with noserverino?): Bunkarr compares such names by content, which costs extra reads. Jobs check this again before they rely on it."
        >
          Inode numbers not stable (content compared)
        </Badge>
      )}
      {caps.caseInsensitive && (
        <Badge tone="warn" title="Names differing only in case collide; such files are reported and not copied">
          Case-insensitive
        </Badge>
      )}
      {caps.invalidChars && (
        <Badge tone="warn" title={`Names containing ${caps.invalidChars.split('').join(' ')} cannot be stored and are reported`}>
          Rejects {caps.invalidChars.split('').join(' ')}
        </Badge>
      )}
      {caps.trailingDotSpace === false && (
        <Badge tone="warn" title="Names ending in a dot or space cannot be stored">
          No trailing dot/space
        </Badge>
      )}
    </span>
  );
}

/** TestResultView shows a destination probe: marker, filesystem, capabilities, space, warnings. */
export function TestResultView({ result, creating }: { result: DestinationTestResult; creating?: boolean }) {
  const caps = result.capabilities;
  const used = result.totalBytes > 0 ? 1 - result.freeBytes / result.totalBytes : null;
  return (
    <div role="group" aria-label="Test result" className="mb-4">
      <Notice tone={result.ok ? 'success' : 'error'} reveal revealKey={result}>
        {result.message || (result.ok ? 'The target can be used.' : 'The target cannot be used.')}
      </Notice>
      <dl className="mb-3 grid grid-cols-[9rem_1fr] gap-x-4 gap-y-1 rounded border border-line bg-page p-3 text-sm">
        <dt className="text-ink-muted">Marker</dt>
        <dd>
          {creating && result.marker === 'missing' ? (
            <Badge title="Bunkarr writes .bunkarr/destination.json when it creates the destination">None yet (written on create)</Badge>
          ) : (
            <MarkerBadge marker={result.marker} />
          )}
        </dd>
        <dt className="text-ink-muted">Filesystem</dt>
        <dd className="flex flex-wrap items-center gap-2">
          <span className="font-mono">{result.fsType || caps?.fsType || 'unknown'}</span>
          {result.local ? (
            <Badge tone="warn" title="Same filesystem as / or the config folder, or tmpfs/overlay">
              Local
            </Badge>
          ) : (
            <Badge tone="ok">Separate filesystem</Badge>
          )}
        </dd>
        <dt className="text-ink-muted">Writable</dt>
        <dd>{result.writable ? <Badge tone="ok">Yes</Badge> : <Badge tone="danger">No</Badge>}</dd>
        <dt className="text-ink-muted">Free space</dt>
        <dd>
          {formatBytes(result.freeBytes)} free of {formatBytes(result.totalBytes)}
          {used != null && <span className="text-ink-muted"> ({formatPercent(used)} used)</span>}
        </dd>
        <dt className="text-ink-muted">Top-level entries</dt>
        <dd>{formatNumber(result.entries)}</dd>
        <dt className="text-ink-muted">Capabilities</dt>
        <dd>
          {creating && !caps ? (
            <span className="text-ink-muted">Probed when the destination is created (hardlinks, case, names, time precision).</span>
          ) : (
            <CapabilityBadges caps={caps} />
          )}
        </dd>
        {caps?.mtimeGranularityNs != null && (
          <>
            <dt className="text-ink-muted">Time precision</dt>
            <dd>{granularity(caps.mtimeGranularityNs)}</dd>
          </>
        )}
      </dl>
      <WarningList warnings={result.warnings} />
    </div>
  );
}

const REPOSITORY_TEXT: Record<RepositoryState, { tone: Tone; label: string; title: string }> = {
  missing: { tone: 'info', label: 'No repository yet', title: 'Bunkarr creates the repository (restic init) when you create the destination.' },
  exists: { tone: 'warn', label: 'Repository exists', title: 'A restic repository is already here: attach it with its password.' },
  'wrong-password': { tone: 'danger', label: 'Wrong password', title: 'A repository is here, and the password does not open it.' },
  locked: { tone: 'warn', label: 'Repository locked', title: 'A repository is here and another process holds a lock on it.' },
};

const REMOTE_MARKER_TEXT: Record<RemoteMarkerState, { tone: Tone; label: string; title: string }> = {
  missing: { tone: 'info', label: 'No marker yet', title: 'Bunkarr writes .bunkarr/destination.json when you create the destination.' },
  ok: { tone: 'warn', label: 'Existing Bunkarr destination', title: 'The remote holds a Bunkarr destination: attach it to continue its backup.' },
  unreadable: { tone: 'danger', label: 'Unreadable', title: 'The marker cannot be read: a wrong crypt password or password2, another crypt remote, or a file that is not a Bunkarr marker.' },
  foreign: { tone: 'danger', label: 'Used by another destination', title: 'The remote belongs to another destination of this Bunkarr.' },
};

/** EngineTestView shows the test of a restic or rclone destination (phase4.md §4.5). */
export function EngineTestView({ result, engine, creating }: { result: EngineTestResult; engine: 'restic' | 'rclone'; creating?: boolean }) {
  const repo = result.repository ? REPOSITORY_TEXT[result.repository] : null;
  const marker = result.marker ? REMOTE_MARKER_TEXT[result.marker] : null;
  const waitingForKeys = !!result.hostKeys?.length;
  return (
    <div role="group" aria-label="Test result" className="mb-4">
      <Notice tone={result.ok ? 'success' : waitingForKeys ? 'warning' : 'error'} reveal revealKey={result}>
        {result.message || (result.ok ? 'The connection works.' : 'The connection does not work.')}
      </Notice>
      <dl className="mb-3 grid grid-cols-[9rem_1fr] gap-x-4 gap-y-1 rounded border border-line bg-page p-3 text-sm">
        <dt className="text-ink-muted">Reachable</dt>
        <dd>{result.reachable ? <Badge tone="ok">Yes</Badge> : <Badge tone="danger">No</Badge>}</dd>
        {engine === 'restic' && (
          <>
            <dt className="text-ink-muted">Repository</dt>
            <dd className="flex flex-wrap items-center gap-2">
              {repo ? (
                <Badge tone={!creating && result.ok && result.repository === 'exists' ? 'ok' : repo.tone} title={repo.title}>
                  {!creating && result.ok && result.repository === 'exists' ? 'Repository OK' : repo.label}
                </Badge>
              ) : (
                <span className="text-ink-muted">not checked</span>
              )}
              {result.id && <span className="break-all font-mono text-xs text-ink-muted">{result.id}</span>}
            </dd>
          </>
        )}
        {engine === 'rclone' && (
          <>
            <dt className="text-ink-muted">Marker</dt>
            <dd className="flex flex-wrap items-center gap-2">
              {marker ? (
                <Badge tone={!creating && result.ok && result.marker === 'ok' ? 'ok' : marker.tone} title={marker.title}>
                  {!creating && result.ok && result.marker === 'ok' ? 'Marker OK' : marker.label}
                </Badge>
              ) : (
                <span className="text-ink-muted">not checked</span>
              )}
              {result.markerName && <span className="text-xs text-ink-muted">({result.markerName})</span>}
            </dd>
          </>
        )}
        <dt className="text-ink-muted">Top-level entries</dt>
        <dd>{formatNumber(result.entries)}</dd>
        {result.freeBytes != null && (
          <>
            <dt className="text-ink-muted">Free space</dt>
            <dd>{formatBytes(result.freeBytes)}</dd>
          </>
        )}
        {result.engineVersion && (
          <>
            <dt className="text-ink-muted">Engine</dt>
            <dd className="break-all font-mono text-xs">{result.engineVersion}</dd>
          </>
        )}
      </dl>
      <WarningList warnings={result.warnings} />
    </div>
  );
}
