import { Trash2 } from 'lucide-react';
import { useState } from 'react';
import type { TierField } from '@/api/tiers';
import { IconButton } from '@/components/Button';
import { inputClass } from '@/components/Form';
import { conditionFor, defaultValue, OP_LABELS, sizeParts, SIZE_UNITS, SOURCE_LABELS, SOURCE_ORDER, type DraftCondition } from './tierText';

// Every control sets its own width (full on phones), so the shared class loses its w-full.
const small = `${inputClass.replace('w-full', '')} py-1.5`;

/**
 * ConditionEditor edits one condition: the field (fed by GET /tiers/fields, grouped by where the
 * facts come from; a field whose provider is not built yet is listed disabled with the reason),
 * the operator and the value. label prefixes the controls' accessible names ("Rule 1 condition 2").
 */
export function ConditionEditor({
  condition,
  fields,
  label,
  onChange,
  onRemove,
  problem,
  warnings,
}: {
  condition: DraftCondition;
  fields: TierField[];
  label: string;
  onChange: (c: DraftCondition) => void;
  onRemove: () => void;
  problem?: string;
  warnings?: string[];
}) {
  const field = fields.find((f) => f.field === condition.field);
  const groups = SOURCE_ORDER.map((s) => ({ source: s, fields: fields.filter((f) => f.source === s) })).filter((g) => g.fields.length > 0);
  const others = fields.filter((f) => !SOURCE_ORDER.includes(f.source));
  if (others.length > 0) groups.push({ source: 'other', fields: others });
  const noValue = field?.noValueOps?.includes(condition.op);

  function pickField(name: string) {
    const f = fields.find((x) => x.field === name);
    if (f) onChange({ ...conditionFor(f), key: condition.key });
  }

  function pickOp(op: string) {
    const takesValue = !field?.noValueOps?.includes(op);
    const hadValue = !field?.noValueOps?.includes(condition.op);
    onChange({ ...condition, op, value: takesValue && hadValue ? condition.value : defaultValue(field, op) });
  }

  return (
    <li className="rounded border border-line/70 bg-page/40 p-2">
      <div className="flex flex-wrap items-start gap-2">
        <select aria-label={`${label} field`} className={`${small} w-full sm:w-60`} value={field ? condition.field : ''} onChange={(e) => pickField(e.target.value)}>
          {!field && <option value="">{condition.field ? `${condition.field} (unknown field)` : 'Choose a field…'}</option>}
          {groups.map((g) => (
            <optgroup key={g.source} label={SOURCE_LABELS[g.source] ?? g.source}>
              {g.fields.map((f) => (
                // An unavailable field stays selectable only where it is already used, so a loaded
                // preset keeps it; its reason is in the option and under the row.
                <option key={f.field} value={f.field} disabled={!f.available && f.field !== condition.field}>
                  {f.available ? f.label : `${f.label} — not available: ${f.reason ?? 'no provider'}`}
                </option>
              ))}
            </optgroup>
          ))}
        </select>
        {field && (
          <select aria-label={`${label} operator`} className={`${small} w-full sm:w-44`} value={condition.op} onChange={(e) => pickOp(e.target.value)}>
            {field.ops.map((op) => (
              <option key={op} value={op}>
                {OP_LABELS[op] ?? op}
              </option>
            ))}
          </select>
        )}
        {field && !noValue && <ValueInput field={field} value={condition.value} label={`${label} value`} onChange={(value) => onChange({ ...condition, value })} />}
        <IconButton label={`Remove ${label.toLowerCase()}`} icon={Trash2} className="ml-auto hover:text-danger" onClick={onRemove} />
      </div>
      {field && !field.available && (
        <p className="mt-1 text-xs text-warn">
          Not available: {field.reason ?? 'nothing supplies this fact yet'}. Until it is, this condition is unknown, so its rule can only make files more
          protected, never less.
        </p>
      )}
      {problem && (
        <p role="alert" className="mt-1 text-xs text-danger">
          {problem}
        </p>
      )}
      {warnings?.map((w) => (
        <p key={w} className="mt-1 text-xs text-warn">
          Warning: {w}
        </p>
      ))}
    </li>
  );
}

/** ValueInput edits a condition value by the field's type, unit and suggestions. */
function ValueInput({ field, value, label, onChange }: { field: TierField; value: unknown; label: string; onChange: (v: unknown) => void }) {
  if (field.valueType === 'bool') {
    return (
      <select aria-label={label} className={`${small} w-full sm:w-28`} value={value === false ? 'false' : 'true'} onChange={(e) => onChange(e.target.value === 'true')}>
        <option value="true">yes</option>
        <option value="false">no</option>
      </select>
    );
  }
  if (field.valueType === 'int' && field.unit === 'bytes') {
    return <SizeInput value={typeof value === 'number' ? value : undefined} label={label} onChange={onChange} />;
  }
  if (field.valueType === 'int' && field.suggestions.length > 0 && !field.unit) {
    // A source id: pick from the sources.
    return (
      <select aria-label={label} className={`${small} w-full sm:w-56`} value={typeof value === 'number' ? String(value) : ''} onChange={(e) => onChange(e.target.value === '' ? undefined : Number(e.target.value))}>
        {typeof value !== 'number' && <option value="">Choose…</option>}
        {typeof value === 'number' && !field.suggestions.some((s) => s.value === value) && <option value={String(value)}>#{value} (not found)</option>}
        {field.suggestions.map((s) => (
          <option key={String(s.value)} value={String(s.value)}>
            {s.label}
          </option>
        ))}
      </select>
    );
  }
  if (field.valueType === 'int') {
    return (
      <span className="flex items-center gap-2">
        <input
          type="number"
          min={0}
          step={1}
          aria-label={label}
          className={`${small} w-28`}
          value={typeof value === 'number' && Number.isFinite(value) ? value : ''}
          onChange={(e) => onChange(e.target.value === '' ? undefined : Number(e.target.value))}
        />
        {field.unit && <span className="text-sm text-ink-muted">{field.unit}</span>}
      </span>
    );
  }
  if (field.valueType === 'ints') {
    return <IdsInput field={field} value={Array.isArray(value) ? (value as number[]) : []} label={label} onChange={onChange} />;
  }
  // A string: free text (a renamed tag is accepted with a warning), with the index's values offered.
  const listId = `${label.replace(/\W+/g, '-')}-suggestions`;
  return (
    <>
      <input
        type="text"
        aria-label={label}
        list={field.suggestions.length > 0 ? listId : undefined}
        className={`${small} w-full sm:w-56`}
        value={typeof value === 'string' ? value : ''}
        placeholder={field.field === 'plex.section' ? '<plex id>:<section key>' : undefined}
        spellCheck={false}
        onChange={(e) => onChange(e.target.value === '' ? undefined : e.target.value)}
      />
      {field.suggestions.length > 0 && (
        <datalist id={listId}>
          {field.suggestions.map((s) => (
            <option key={String(s.value)} value={String(s.value)}>
              {s.label}
            </option>
          ))}
        </datalist>
      )}
    </>
  );
}

/** SizeInput edits a byte count as an amount and a unit (MiB, GiB, TiB). */
function SizeInput({ value, label, onChange }: { value: number | undefined; label: string; onChange: (v: number | undefined) => void }) {
  const initial = sizeParts(value);
  const [unit, setUnit] = useState(initial.unit);
  const [text, setText] = useState(initial.amount === undefined ? '' : String(initial.amount));
  function emit(t: string, u: number) {
    const n = Number(t);
    onChange(t.trim() === '' || !Number.isFinite(n) || n < 0 ? undefined : Math.round(n * u));
  }
  return (
    <span className="flex items-center gap-2">
      <input
        type="number"
        min={0}
        step="any"
        aria-label={label}
        className={`${small} w-28`}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          emit(e.target.value, unit);
        }}
      />
      <select
        aria-label={`${label} unit`}
        className={`${small} w-24`}
        value={unit}
        onChange={(e) => {
          const u = Number(e.target.value);
          setUnit(u);
          emit(text, u);
        }}
      >
        {SIZE_UNITS.map((u) => (
          <option key={u.label} value={u.bytes}>
            {u.label}
          </option>
        ))}
      </select>
    </span>
  );
}

/** IdsInput picks ids (Seerr users) from the suggestions, or takes them as a comma list. */
function IdsInput({ field, value, label, onChange }: { field: TierField; value: number[]; label: string; onChange: (v: number[] | undefined) => void }) {
  const [text, setText] = useState(value.join(', '));
  if (field.suggestions.length === 0) {
    return (
      <input
        type="text"
        aria-label={label}
        className={`${small} w-full sm:w-56`}
        placeholder="User ids, e.g. 1, 4"
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          const ids = e.target.value
            .split(/[\s,]+/)
            .filter(Boolean)
            .map(Number);
          onChange(ids.length === 0 ? undefined : ids);
        }}
      />
    );
  }
  const toggle = (id: number, on: boolean) => {
    const next = on ? [...value.filter((x) => x !== id), id] : value.filter((x) => x !== id);
    onChange(next.length === 0 ? undefined : next.sort((a, b) => a - b));
  };
  // A stored id no live user has (deleted in Seerr, or typed while Seerr was unreachable) stays
  // visible and can be unticked, as the source picker shows "#id (not found)".
  const options = [
    ...field.suggestions.map((s) => ({ id: Number(s.value), label: s.label })),
    ...[...new Set(value)].filter((id) => !field.suggestions.some((s) => Number(s.value) === id)).map((id) => ({ id, label: `Seerr user #${id} (not found)` })),
  ];
  return (
    <fieldset aria-label={label} className="flex flex-wrap gap-x-3 gap-y-1 pt-1">
      {options.map((o) => (
        <label key={o.id} className="flex items-center gap-1 text-sm">
          <input type="checkbox" className="h-4 w-4 accent-[var(--color-accent)]" checked={value.includes(o.id)} onChange={(e) => toggle(o.id, e.target.checked)} />
          {o.label}
        </label>
      ))}
    </fieldset>
  );
}
