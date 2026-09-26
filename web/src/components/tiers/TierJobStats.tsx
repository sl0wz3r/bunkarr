import { Stat, StatGrid } from '@/components/Page';
import { formatBytes, formatNumber } from '@/lib/format';

interface Count {
  files: number;
  bytes: number;
}

function isCount(v: unknown): v is Count {
  return typeof v === 'object' && v !== null && typeof (v as Count).files === 'number' && typeof (v as Count).bytes === 'number';
}

const PARTS: { key: string; label: string; hint: string }[] = [
  { key: 'full', label: 'Full', hint: 'Live files copied to this destination' },
  { key: 'manifest', label: 'Manifest only', hint: "Live files only listed in the destination's manifests" },
  { key: 'skip', label: 'Skip', hint: 'Live files neither copied nor listed' },
  { key: 'unknownPromoted', label: 'Full (unknown)', hint: 'Full only because a fact is unknown; such copies count toward the mass-change guard' },
];

/**
 * TierJobStats shows a sync's live files by tier (stats.tiers, phase2-3.md §8.5). The flat tier
 * figures (kept, released, moved to a non-full location, stale references, revision) are in the
 * job's generic statistics.
 */
export function TierJobStats({ stats }: { stats: Record<string, unknown> | null }) {
  const tiers = stats?.tiers;
  if (typeof tiers !== 'object' || tiers === null) {
    return null;
  }
  const t = tiers as Record<string, unknown>;
  const parts = PARTS.filter((p) => isCount(t[p.key]));
  if (parts.length === 0) {
    return null;
  }
  return (
    <StatGrid label="Files by tier">
      {parts.map((p) => {
        const c = t[p.key] as Count;
        return (
          <Stat
            key={p.key}
            label={p.label}
            hint={p.hint}
            muted={c.files === 0}
            value={
              <>
                {formatNumber(c.files)}
                <span className="block text-xs font-normal text-ink-muted">{formatBytes(c.bytes)}</span>
              </>
            }
          />
        );
      })}
    </StatGrid>
  );
}
