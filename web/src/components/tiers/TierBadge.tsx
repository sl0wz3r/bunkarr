import type { Tier, TierField, TierReason } from '@/api/tiers';
import { Badge, type Tone } from '@/components/StatusBadge';
import { isIrreplaceableReason, reasonText, TIER_HELP, TIER_LABELS } from './tierText';

const TIER_TONES: Record<Tier, Tone> = { full: 'ok', manifest: 'info', skip: 'muted' };

/** TierBadge shows a tier: Full, Manifest only or Skip. */
export function TierBadge({ tier }: { tier: Tier }) {
  return (
    <Badge tone={TIER_TONES[tier] ?? 'muted'} title={TIER_HELP[tier]}>
      {TIER_LABELS[tier] ?? tier}
    </Badge>
  );
}

/**
 * UnknownPromotedBadge flags a decision that is full only because a fact is unknown (S14): a rule
 * that would have protected the file more could not be decided.
 */
export function UnknownPromotedBadge() {
  return (
    <Badge tone="warn" title="Full only because a fact is unknown: a more protective rule could not be decided, and unknown never lowers protection.">
      Unknown → full
    </Badge>
  );
}

/**
 * ReasonList renders a decision's reasons (the deciding rule's conditions) and the unknown
 * conditions of earlier rules, each as one line. An unknown result says why.
 */
export function ReasonList({
  reasons,
  unknown,
  fields,
  follows,
}: {
  reasons: TierReason[] | null | undefined;
  unknown?: TierReason[] | null;
  fields: Map<string, TierField>;
  follows?: string;
}) {
  const main = reasons ?? [];
  const extra = (unknown ?? []).filter((u) => !main.some((r) => r.ruleId === u.ruleId && r.conditionIndex === u.conditionIndex));
  if (main.length === 0 && extra.length === 0 && !follows) {
    return null;
  }
  return (
    <ul className="space-y-0.5 text-xs">
      {follows && <li className="text-ink-muted">Follows {follows}</li>}
      {main.map((r, i) => (
        <li key={`r${i}`} className={r.result === 'unknown' ? 'text-warn' : isIrreplaceableReason(r) ? 'text-accent' : 'text-ink-muted'}>
          {reasonText(r, fields)}
        </li>
      ))}
      {extra.map((r, i) => (
        <li key={`u${i}`} className="text-warn">
          <span className="sr-only">Earlier rule unknown: </span>
          {reasonText(r, fields)}
        </li>
      ))}
    </ul>
  );
}
