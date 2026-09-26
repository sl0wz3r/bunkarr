import { ArrowDown, ArrowUp, Plus, Trash2 } from 'lucide-react';
import { useId } from 'react';
import { TIERS, type Tier, type TierField, type TierMatch } from '@/api/tiers';
import type { Destination } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { inputClass, Switch } from '@/components/Form';
import { Badge } from '@/components/StatusBadge';
import { ConditionEditor } from './ConditionEditor';
import { conditionFor, TIER_HELP, TIER_LABELS, type DraftRule, type Problem, type WarningsByKey } from './tierText';

/** inline is the look of a select that sits inside a sentence ("When all of…", "then Full"). */
const inline = `${inputClass.replace('w-full', 'w-auto')} py-1`;

/**
 * RuleCard edits one rule of the ordered list: its place (up/down), enabled, name, the conditions
 * and how they combine (all/any), the action, and "Applies at" (every destination, or only the
 * chosen ones: the rule is not evaluated elsewhere, which is not the same as "skip" there).
 */
export function RuleCard({
  rule,
  index,
  count,
  fields,
  destinations,
  problems,
  warnings,
  onChange,
  onMove,
  onRemove,
}: {
  rule: DraftRule;
  index: number;
  count: number;
  fields: TierField[];
  destinations: Destination[];
  problems: Problem[];
  warnings: WarningsByKey;
  onChange: (r: DraftRule) => void;
  onMove: (delta: -1 | 1) => void;
  onRemove: () => void;
}) {
  const nameId = useId();
  const n = index + 1;
  const title = rule.name.trim() || `Rule ${n}`;
  const ruleProblems = problems.filter((p) => !p.condKey);
  const firstField = fields.find((f) => f.available) ?? fields[0];
  const known = new Set(destinations.map((d) => d.id));
  const gone = (rule.destinationIds ?? []).filter((id) => !known.has(id));

  function setDestination(id: number, on: boolean) {
    const list = rule.destinationIds ?? [];
    onChange({ ...rule, destinationIds: on ? [...list, id].sort((a, b) => a - b) : list.filter((x) => x !== id) });
  }

  return (
    <li aria-label={`Rule ${n}: ${title}`} className={`rounded border bg-panel p-3 ${rule.enabled ? 'border-line' : 'border-line/50 opacity-80'}`}>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="w-6 text-center text-sm font-medium text-ink-muted" aria-hidden="true">
          {n}
        </span>
        <IconButton label={`Move rule ${n} up`} icon={ArrowUp} disabled={index === 0} onClick={() => onMove(-1)} />
        <IconButton label={`Move rule ${n} down`} icon={ArrowDown} disabled={index === count - 1} onClick={() => onMove(1)} />
        <Switch checked={rule.enabled} label={`Rule ${n} enabled`} onChange={(enabled) => onChange({ ...rule, enabled })} />
        <label htmlFor={nameId} className="sr-only">
          Rule {n} name
        </label>
        <input
          id={nameId}
          className={`${inputClass} min-w-[10rem] flex-1 py-1.5 font-medium`}
          value={rule.name}
          maxLength={100}
          placeholder="Name"
          onChange={(e) => onChange({ ...rule, name: e.target.value })}
        />
        {!rule.id && <Badge tone="info">New</Badge>}
        {!rule.enabled && <Badge>Disabled</Badge>}
        <IconButton label={`Delete rule ${n}`} icon={Trash2} className="hover:text-danger" onClick={onRemove} />
      </div>

      <div className="space-y-3 pl-0 sm:pl-8">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span>When</span>
          <select
            aria-label={`Rule ${n} match`}
            className={inline}
            value={rule.match}
            disabled={rule.conditions.length < 2}
            onChange={(e) => onChange({ ...rule, match: e.target.value as TierMatch })}
          >
            <option value="all">all</option>
            <option value="any">any</option>
          </select>
          <span>{rule.conditions.length === 0 ? 'of no conditions: the rule matches every file' : 'of these conditions are true'}</span>
        </div>
        {rule.conditions.length > 0 && (
          <ul className="space-y-2">
            {rule.conditions.map((c, ci) => (
              <ConditionEditor
                key={c.key}
                condition={c}
                fields={fields}
                label={`Rule ${n} condition ${ci + 1}`}
                problem={problems.find((p) => p.condKey === c.key)?.message}
                warnings={warnings.get(c.key)}
                onChange={(next) => onChange({ ...rule, conditions: rule.conditions.map((x) => (x.key === c.key ? next : x)) })}
                onRemove={() => onChange({ ...rule, conditions: rule.conditions.filter((x) => x.key !== c.key) })}
              />
            ))}
          </ul>
        )}
        <Button
          small
          variant="ghost"
          icon={Plus}
          disabled={!firstField || rule.conditions.length >= 32}
          onClick={() => firstField && onChange({ ...rule, conditions: [...rule.conditions, conditionFor(firstField)] })}
        >
          Add condition to rule {n}
        </Button>

        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span>then</span>
          <select aria-label={`Rule ${n} action`} className={inline} value={rule.action} onChange={(e) => onChange({ ...rule, action: e.target.value as Tier })}>
            {TIERS.map((t) => (
              <option key={t} value={t}>
                {TIER_LABELS[t]}
              </option>
            ))}
          </select>
          <span className="text-xs text-ink-muted">{TIER_HELP[rule.action]}</span>
        </div>

        <fieldset className="text-sm">
          <legend className="mb-1">Applies at</legend>
          <div className="flex flex-wrap gap-x-4 gap-y-1">
            <label className="flex items-center gap-1">
              <input type="radio" name={`${nameId}-scope`} checked={rule.destinationIds === null} onChange={() => onChange({ ...rule, destinationIds: null })} />
              All destinations
            </label>
            <label className="flex items-center gap-1">
              <input
                type="radio"
                name={`${nameId}-scope`}
                checked={rule.destinationIds !== null}
                onChange={() => onChange({ ...rule, destinationIds: rule.destinationIds ?? [] })}
              />
              Only at…
            </label>
          </div>
          {rule.destinationIds !== null && (
            <div className="mt-1 flex flex-wrap gap-x-4 gap-y-1 pl-5">
              {destinations.map((d) => (
                <label key={d.id} className="flex items-center gap-1">
                  <input
                    type="checkbox"
                    className="h-4 w-4 accent-[var(--color-accent)]"
                    aria-label={`Rule ${n} applies at ${d.name}`}
                    checked={rule.destinationIds?.includes(d.id) ?? false}
                    onChange={(e) => setDestination(d.id, e.target.checked)}
                  />
                  {d.name}
                </label>
              ))}
              {gone.map((id) => (
                <label key={id} className="flex items-center gap-1 text-ink-muted">
                  <input type="checkbox" className="h-4 w-4" checked onChange={() => setDestination(id, false)} aria-label={`Rule ${n} applies at destination #${id}`} />
                  Destination #{id} (deleted)
                </label>
              ))}
            </div>
          )}
          {rule.destinationIds !== null && rule.destinationIds.length === 0 && (
            <p className="mt-1 text-xs text-warn">This rule applies at no destination: choose one, or All destinations. An empty list never means all.</p>
          )}
          <p className="mt-1 text-xs text-ink-muted">
            At other destinations the rule is not evaluated, which is not the same as skip there. For “full at one destination only”, add this rule as Full
            at that destination, then a rule with the same conditions as Skip at the others.
          </p>
        </fieldset>

        {ruleProblems.map((p) => (
          <p key={p.message} role="alert" className="text-xs text-danger">
            {p.message}
          </p>
        ))}
        {warnings.get(rule.key)?.map((w) => (
          <p key={w} className="text-xs text-warn">
            Warning: {w}
          </p>
        ))}
      </div>
    </li>
  );
}
