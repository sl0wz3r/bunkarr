// Tier texts and the rule editor's draft model (docs/design/phase2-3.md §8, §16): labels for tiers,
// operators and fact sources; how a condition value, a fact and a reason read; and the draft the
// Tiers page edits (local keys, conversion to the PUT /tiers/rules body, client-side checks).
//
// The server stays the authority: it validates every save and preview (400 naming the rule and
// condition). The checks here only catch an incomplete draft before it is sent.

import type { TierCondition, TierField, TierMatch, TierReason, TierRule, TierRuleInput, TierWarning, Tier } from '@/api/tiers';
import { formatBytes, formatNumber } from '@/lib/format';

export const TIER_LABELS: Record<Tier, string> = {
  full: 'Full',
  manifest: 'Manifest only',
  skip: 'Skip',
};

/** TIER_HELP explains each tier in one sentence (the editor's action picker and the legend). */
export const TIER_HELP: Record<Tier, string> = {
  full: 'Copied to the destination.',
  manifest: "Not copied, but listed in the destination's manifests so it can be acquired again.",
  skip: 'Not copied and not listed as a file of its own (an *arr file is still listed with its item).',
};

/** OP_LABELS name the operators in words (the reason renderer uses the same words). */
export const OP_LABELS: Record<string, string> = {
  is: 'is',
  isNot: 'is not',
  has: 'has',
  hasNot: 'does not have',
  gt: 'more than',
  gte: 'at least',
  lt: 'less than',
  lte: 'at most',
  eq: 'exactly',
  olderThan: 'more than … ago',
  newerThan: 'within the last',
  never: 'never',
  in: 'is one of',
  notIn: 'is none of',
};

/** SOURCE_LABELS name where a field's facts come from (the field picker's groups). */
export const SOURCE_LABELS: Record<string, string> = {
  arr: 'Sonarr, Radarr, Lidarr',
  catalog: 'File',
  plex: 'Plex',
  seerr: 'Seerr',
  tautulli: 'Tautulli',
  maintainerr: 'Maintainerr',
  flag: 'Flags',
};

/** The order of the field picker's groups. */
export const SOURCE_ORDER = ['arr', 'catalog', 'plex', 'tautulli', 'seerr', 'maintainerr', 'flag'];

/** SIZE_UNITS are the units of a file size condition (IEC, like formatBytes). */
export const SIZE_UNITS: { label: string; bytes: number }[] = [
  { label: 'MiB', bytes: 1024 ** 2 },
  { label: 'GiB', bytes: 1024 ** 3 },
  { label: 'TiB', bytes: 1024 ** 4 },
];

/** sizeParts splits a byte count into the largest unit that holds it exactly (GiB for 0). */
export function sizeParts(bytes: number | undefined): { amount: number | undefined; unit: number } {
  if (bytes === undefined || !Number.isFinite(bytes)) {
    return { amount: undefined, unit: SIZE_UNITS[1].bytes };
  }
  for (let i = SIZE_UNITS.length - 1; i >= 0; i--) {
    const u = SIZE_UNITS[i].bytes;
    if (bytes !== 0 && bytes % u === 0) {
      return { amount: bytes / u, unit: u };
    }
  }
  if (bytes === 0) {
    return { amount: 0, unit: SIZE_UNITS[1].bytes };
  }
  // Not a whole number of MiB: show it in GiB with decimals.
  return { amount: Math.round((bytes / SIZE_UNITS[1].bytes) * 1000) / 1000, unit: SIZE_UNITS[1].bytes };
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** suggestionLabel is the label the field's suggestions give a value, if any. */
function suggestionLabel(field: TierField | undefined, value: unknown): string | undefined {
  return field?.suggestions.find((s) => s.value === value || (typeof s.value === 'string' && typeof value === 'string' && s.value.toLowerCase() === value.toLowerCase()))?.label;
}

/** seerrUser names a Seerr user by its suggestion label, else by id (a user appears by id only). */
function seerrUser(field: TierField | undefined, id: unknown): string {
  return suggestionLabel(field, id) ?? `Seerr user #${String(id)}`;
}

/** valueText renders a condition value for people: "4.0 GiB", "30 days", "Movies", "yes". */
export function valueText(fieldName: string, value: unknown, field?: TierField): string {
  if (value === undefined || value === null) {
    return '';
  }
  if (fieldName === 'seerr.requestedBy' && Array.isArray(value)) {
    return value.map((id) => seerrUser(field, id)).join(', ');
  }
  if (Array.isArray(value)) {
    return value.map(String).join(', ');
  }
  if (typeof value === 'boolean') {
    return value ? 'yes' : 'no';
  }
  if (typeof value === 'number') {
    switch (field?.unit ?? (fieldName === 'file.size' ? 'bytes' : fieldName === 'file.age' || fieldName === 'tautulli.lastWatched' ? 'days' : '')) {
      case 'bytes':
        return formatBytes(value);
      case 'days':
        return value === 1 ? '1 day' : `${formatNumber(value)} days`;
      case 'plays':
        return value === 1 ? '1 play' : `${formatNumber(value)} plays`;
    }
    return suggestionLabel(field, value) ?? formatNumber(value);
  }
  if (typeof value === 'string') {
    // A Plex section reads by its library's name; tags, profiles, folders and genres as typed.
    const label = fieldName === 'plex.section' ? suggestionLabel(field, value) : undefined;
    return label ?? `"${value}"`;
  }
  return JSON.stringify(value);
}

const ISO_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/;

/** actualText renders the fact a condition compared: a tag list, a profile, a size, a date, plays. */
export function actualText(actual: unknown, fieldName = '', field?: TierField): string {
  if (actual === undefined || actual === null) {
    return '';
  }
  if (Array.isArray(actual)) {
    if (actual.length === 0) {
      return 'none';
    }
    return fieldName === 'seerr.requestedBy' ? actual.map((id) => seerrUser(field, id)).join(', ') : actual.map((a) => actualText(a)).join(', ');
  }
  if (typeof actual === 'string' && ISO_TIME.test(actual)) {
    const t = Date.parse(actual);
    return Number.isNaN(t) ? actual : new Date(t).toISOString().slice(0, 10);
  }
  if (typeof actual === 'number' && fieldName === 'file.size') {
    return formatBytes(actual);
  }
  if (typeof actual === 'boolean') {
    return actual ? 'yes' : 'no';
  }
  if (typeof actual === 'number' || typeof actual === 'string') {
    return String(actual);
  }
  if (isRecord(actual)) {
    return Object.entries(actual)
      .map(([k, v]) => `${k}: ${actualText(v)}`)
      .join(', ');
  }
  return JSON.stringify(actual);
}

/** isIrreplaceableReason reports the built-in override's reason (rule 0, condition -1). */
export function isIrreplaceableReason(r: Pick<TierReason, 'ruleId' | 'conditionIndex' | 'field'>): boolean {
  return r.ruleId === 0 && r.conditionIndex < 0 && r.field === 'flag.irreplaceable';
}

/** conditionText renders a condition: `*arr tag has "bunkarr-full"`, `Last watched (Tautulli) never`. */
export function conditionText(c: Pick<TierCondition, 'field' | 'op' | 'value'>, fields: Map<string, TierField>): string {
  const f = fields.get(c.field);
  const label = f?.label ?? c.field;
  const op = OP_LABELS[c.op] ?? c.op;
  const v = f?.noValueOps?.includes(c.op) ? '' : valueText(c.field, c.value, f);
  if (c.op === 'olderThan' && v) {
    return `${label} more than ${v} ago`;
  }
  return [label, op, v].filter(Boolean).join(' ');
}

/**
 * reasonText renders one evaluated condition the way the job log does (internal/tiers Reason.Text),
 * in words: `*arr tag has "bunkarr-full": true (bunkarr-full, 4k)`, `irreplaceable (flag #3)`,
 * `Pending deletion in Maintainerr is yes: unknown (Maintainerr cache is 30 h old)`.
 */
export function reasonText(r: TierReason, fields: Map<string, TierField>): string {
  if (isIrreplaceableReason(r)) {
    const ids = Array.isArray(r.actual) ? r.actual : [];
    return `irreplaceable (${ids.length > 0 ? ids.map((id) => `flag #${String(id)}`).join(', ') : 'flag'})`;
  }
  let s = `${conditionText(r, fields)}: ${r.result}`;
  if (r.result === 'unknown' && r.why) {
    s += ` (${r.why})`;
  } else if (r.actual !== undefined && r.actual !== null) {
    s += ` (${actualText(r.actual, r.field, fields.get(r.field))})`;
  }
  return s;
}

/** decisionRuleText names the deciding rule: `rule "Keep tagged"`, "no rule matched", "irreplaceable". */
export function decisionRuleText(d: { ruleId: number; ruleName: string }): string {
  if (d.ruleId === 0) {
    return d.ruleName === 'irreplaceable' ? 'irreplaceable (built-in)' : `${d.ruleName || 'no rule matched'} (built-in)`;
  }
  return `rule "${d.ruleName}"`;
}

/** fieldMap indexes the editor's fields by name. */
export function fieldMap(fields: TierField[] | undefined): Map<string, TierField> {
  return new Map((fields ?? []).map((f) => [f.field, f]));
}

// ---- The draft ----

export interface DraftCondition {
  /** A local key: conditions have no ids. */
  key: string;
  field: string;
  op: string;
  /** undefined until the user enters one (an incomplete condition). */
  value?: unknown;
}

export interface DraftRule {
  key: string;
  /** The stored rule's id; absent for a rule not saved yet. */
  id?: number;
  name: string;
  enabled: boolean;
  match: TierMatch;
  conditions: DraftCondition[];
  action: Tier;
  destinationIds: number[] | null;
}

let nextKey = 0;

/** newKey returns a key no other draft element has in this page's lifetime. */
export function newKey(prefix: string): string {
  nextKey += 1;
  return `${prefix}${nextKey}`;
}

/** draftOf turns a stored rule, or a preset's rule, into an editable draft. */
export function draftOf(rule: TierRule | TierRuleInput, keepId = true): DraftRule {
  return {
    key: newKey('r'),
    id: keepId && rule.id ? rule.id : undefined,
    name: rule.name,
    enabled: rule.enabled ?? true,
    match: rule.match ?? 'all',
    conditions: (rule.conditions ?? []).map((c) => ({ key: newKey('c'), field: c.field, op: c.op, value: c.value })),
    action: rule.action,
    destinationIds: rule.destinationIds === undefined ? null : rule.destinationIds,
  };
}

/** emptyRule is the rule "Add rule" appends: no conditions, manifest only, every destination. */
export function emptyRule(position: number): DraftRule {
  return { key: newKey('r'), name: `Rule ${position}`, enabled: true, match: 'all', conditions: [], action: 'manifest', destinationIds: null };
}

/** defaultValue is the value a condition starts with when its field is chosen. */
export function defaultValue(field: TierField | undefined, op: string): unknown {
  if (!field || field.noValueOps?.includes(op)) {
    return undefined;
  }
  if (field.valueType === 'bool') {
    return true;
  }
  if (field.field === 'source' && field.suggestions.length > 0) {
    return field.suggestions[0].value;
  }
  return undefined;
}

/** conditionFor builds a new condition on a field: its first operator and default value. */
export function conditionFor(field: TierField): DraftCondition {
  const op = field.ops[0] ?? '';
  return { key: newKey('c'), field: field.field, op, value: defaultValue(field, op) };
}

/** toInput is a draft rule as PUT /tiers/rules and the preview carry it. */
export function toInput(r: DraftRule, fields: Map<string, TierField>): TierRuleInput {
  const out: TierRuleInput = {
    name: r.name.trim(),
    enabled: r.enabled,
    match: r.match,
    conditions: r.conditions.map((c) => {
      const noValue = fields.get(c.field)?.noValueOps?.includes(c.op);
      return noValue || c.value === undefined ? { field: c.field, op: c.op } : { field: c.field, op: c.op, value: c.value };
    }),
    action: r.action,
    destinationIds: r.destinationIds === null ? null : [...r.destinationIds].sort((a, b) => a - b),
  };
  if (r.id) {
    out.id = r.id;
  }
  return out;
}

/** toInputs converts the whole draft. */
export function toInputs(rules: DraftRule[], fields: Map<string, TierField>): TierRuleInput[] {
  return rules.map((r) => toInput(r, fields));
}

/** sameRules reports whether two rule lists are the same set (the draft is not dirty). */
export function sameRules(a: TierRuleInput[], b: TierRuleInput[]): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

/** Problem is an incomplete part of the draft; condKey is absent for a rule-level problem. */
export interface Problem {
  ruleKey: string;
  condKey?: string;
  message: string;
}

function missing(v: unknown): boolean {
  return v === undefined || v === null || v === '' || (typeof v === 'number' && !Number.isFinite(v)) || (Array.isArray(v) && v.length === 0);
}

/**
 * draftProblems lists what keeps the draft from being sent: a rule without a name, a condition
 * without a value, a value of the wrong kind, too many rules or conditions. The limits are the
 * server's (100 rules, 32 conditions, names of 100 characters).
 */
export function draftProblems(rules: DraftRule[], fields: Map<string, TierField>): Problem[] {
  const out: Problem[] = [];
  if (rules.length > 100) {
    out.push({ ruleKey: rules[100].key, message: 'A rule set has at most 100 rules.' });
  }
  for (const r of rules) {
    const name = r.name.trim();
    if (!name) {
      out.push({ ruleKey: r.key, message: 'Give the rule a name.' });
    } else if ([...name].length > 100) {
      out.push({ ruleKey: r.key, message: 'The name is longer than 100 characters.' });
    }
    if (r.conditions.length > 32) {
      out.push({ ruleKey: r.key, message: 'A rule has at most 32 conditions.' });
    }
    for (const c of r.conditions) {
      const f = fields.get(c.field);
      if (!f) {
        out.push({ ruleKey: r.key, condKey: c.key, message: c.field ? `Unknown field ${c.field}.` : 'Choose a field.' });
        continue;
      }
      if (!f.ops.includes(c.op)) {
        out.push({ ruleKey: r.key, condKey: c.key, message: 'Choose an operator.' });
        continue;
      }
      if (f.noValueOps?.includes(c.op)) {
        continue;
      }
      if (missing(c.value)) {
        out.push({ ruleKey: r.key, condKey: c.key, message: 'Enter a value.' });
        continue;
      }
      const v = c.value;
      const bad =
        (f.valueType === 'bool' && typeof v !== 'boolean') ||
        (f.valueType === 'string' && (typeof v !== 'string' || !v.trim())) ||
        (f.valueType === 'int' && (typeof v !== 'number' || !Number.isInteger(v) || v < 0)) ||
        (f.valueType === 'ints' && (!Array.isArray(v) || v.some((x) => typeof x !== 'number' || !Number.isInteger(x) || x < 1)));
      if (bad) {
        out.push({
          ruleKey: r.key,
          condKey: c.key,
          message: f.valueType === 'int' ? 'Enter a whole number of at least 0.' : f.valueType === 'ints' ? 'Choose at least one.' : 'Enter a value.',
        });
      }
    }
  }
  return out;
}

/** WarningsByKey places the save's stale-reference warnings on the draft's rules and conditions. */
export type WarningsByKey = Map<string, string[]>;

/**
 * placeWarnings maps warnings (0-based positions in the saved list) onto the keys of the draft
 * that was saved, so they stay next to their conditions when the rules are reordered.
 */
export function placeWarnings(warnings: TierWarning[] | null | undefined, rules: DraftRule[]): WarningsByKey {
  const out: WarningsByKey = new Map();
  for (const w of warnings ?? []) {
    const r = rules[w.ruleIndex];
    if (!r) continue;
    const key = w.conditionIndex >= 0 && r.conditions[w.conditionIndex] ? r.conditions[w.conditionIndex].key : r.key;
    out.set(key, [...(out.get(key) ?? []), w.message]);
  }
  return out;
}

/** positionText is "Rule 2, condition 1" for a warning or problem shown away from its rule. */
export function positionText(ruleIndex: number, conditionIndex: number): string {
  return conditionIndex >= 0 ? `Rule ${ruleIndex + 1}, condition ${conditionIndex + 1}` : `Rule ${ruleIndex + 1}`;
}
