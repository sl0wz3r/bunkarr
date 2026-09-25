import { useId, type ReactNode } from 'react';
import { Badge } from './StatusBadge';

/** inputClass is the shared look of text inputs, selects and text areas. */
export const inputClass =
  'w-full rounded border border-line bg-page px-3 py-2 text-sm outline-none focus:border-accent disabled:cursor-not-allowed disabled:opacity-60';

/**
 * FormRow lays out a label (left, stacked on narrow screens), a field and help text. Pass htmlFor
 * to label a single control; groups of controls use `group` (a fieldset with a legend).
 */
export function FormRow({
  label,
  htmlFor,
  help,
  children,
  group,
}: {
  label: ReactNode;
  htmlFor?: string;
  help?: ReactNode;
  children: ReactNode;
  group?: boolean;
}) {
  const grid = 'mb-4 grid gap-1 sm:grid-cols-[11rem_1fr] sm:gap-4';
  const helpNode = help && <div className="mt-1 text-xs text-ink-muted">{help}</div>;
  if (group) {
    return (
      <fieldset className={grid}>
        <legend className="sr-only">{label}</legend>
        <div aria-hidden="true" className="pt-2 text-sm font-medium">
          {label}
        </div>
        <div className="min-w-0">
          {children}
          {helpNode}
        </div>
      </fieldset>
    );
  }
  return (
    <div className={grid}>
      {htmlFor ? (
        <label htmlFor={htmlFor} className="pt-2 text-sm font-medium">
          {label}
        </label>
      ) : (
        <div className="pt-2 text-sm font-medium">{label}</div>
      )}
      <div className="min-w-0">
        {children}
        {helpNode}
      </div>
    </div>
  );
}

interface FieldBase {
  label: string;
  help?: ReactNode;
  disabled?: boolean;
}

export function TextField({
  label,
  help,
  disabled,
  value,
  onChange,
  placeholder,
  type = 'text',
  autoFocus,
  autoComplete = 'off',
  mono,
  required,
}: FieldBase & {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  type?: 'text' | 'url';
  autoFocus?: boolean;
  autoComplete?: string;
  mono?: boolean;
  required?: boolean;
}) {
  const id = useId();
  return (
    <FormRow label={label} htmlFor={id} help={help}>
      <input
        id={id}
        type={type}
        className={`${inputClass} ${mono ? 'font-mono' : ''}`}
        value={value}
        placeholder={placeholder}
        autoFocus={autoFocus}
        autoComplete={autoComplete}
        disabled={disabled}
        required={required}
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
      />
    </FormRow>
  );
}

/** NumberField edits a number; an empty input reports NaN so the form can reject it. */
export function NumberField({
  label,
  help,
  disabled,
  value,
  onChange,
  min,
  max,
  step,
  suffix,
}: FieldBase & { value: number; onChange: (v: number) => void; min?: number; max?: number; step?: number; suffix?: string }) {
  const id = useId();
  return (
    <FormRow label={label} htmlFor={id} help={help}>
      <div className="flex items-center gap-2">
        <input
          id={id}
          type="number"
          className={`${inputClass} max-w-[10rem]`}
          value={Number.isNaN(value) ? '' : value}
          min={min}
          max={max}
          step={step}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value === '' ? Number.NaN : Number(e.target.value))}
        />
        {suffix && <span className="text-sm text-ink-muted">{suffix}</span>}
      </div>
    </FormRow>
  );
}

export interface Option<T extends string> {
  value: T;
  label: string;
}

export function SelectField<T extends string>({
  label,
  help,
  disabled,
  value,
  onChange,
  options,
}: FieldBase & { value: T; onChange: (v: T) => void; options: Option<T>[] }) {
  const id = useId();
  return (
    <FormRow label={label} htmlFor={id} help={help}>
      <select id={id} className={`${inputClass} max-w-md`} value={value} disabled={disabled} onChange={(e) => onChange(e.target.value as T)}>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </FormRow>
  );
}

/** Checkbox is a labelled checkbox without a row (for lists and inline options). */
export function Checkbox({ label, checked, onChange, disabled, help }: { label: ReactNode; checked: boolean; onChange: (v: boolean) => void; disabled?: boolean; help?: ReactNode }) {
  return (
    <label className={`flex items-start gap-2 text-sm ${disabled ? 'opacity-60' : 'cursor-pointer'}`}>
      <input type="checkbox" className="mt-0.5 h-4 w-4 accent-[var(--color-accent)]" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      <span>
        {label}
        {help && <span className="block text-xs text-ink-muted">{help}</span>}
      </span>
    </label>
  );
}

export function CheckboxField({ label, help, disabled, checked, onChange, text }: FieldBase & { checked: boolean; onChange: (v: boolean) => void; text?: string }) {
  return (
    <FormRow label={label} group help={help}>
      <div className="pt-2">
        <Checkbox label={text ?? label} checked={checked} onChange={onChange} disabled={disabled} />
      </div>
    </FormRow>
  );
}

export function TextAreaField({
  label,
  help,
  disabled,
  value,
  onChange,
  placeholder,
  rows = 4,
  mono,
}: FieldBase & { value: string; onChange: (v: string) => void; placeholder?: string; rows?: number; mono?: boolean }) {
  const id = useId();
  return (
    <FormRow label={label} htmlFor={id} help={help}>
      <textarea
        id={id}
        rows={rows}
        className={`${inputClass} ${mono ? 'font-mono' : ''}`}
        value={value}
        placeholder={placeholder}
        disabled={disabled}
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
      />
    </FormRow>
  );
}

/**
 * SecretField edits a write-only secret (token, notification URLs). The stored value is never
 * shown: when one exists the field says "Stored" and an empty input keeps it.
 */
export function SecretField({
  label,
  help,
  disabled,
  value,
  onChange,
  stored,
  placeholder,
  multiline,
  reenter,
}: FieldBase & {
  value: string;
  onChange: (v: string) => void;
  stored: boolean;
  placeholder?: string;
  multiline?: boolean;
  /** The address the secret belongs to changed: the stored value cannot be kept. */
  reenter?: boolean;
}) {
  const id = useId();
  const hint = stored
    ? reenter
      ? 'Enter it again: the URL changed, and a saved secret is only sent to the URL it was saved with.'
      : 'Stored. Leave empty to keep it; type to replace it.'
    : placeholder;
  return (
    <FormRow
      label={
        <span className="inline-flex items-center gap-2">
          {label}
          {stored && <Badge tone="ok">Stored</Badge>}
        </span>
      }
      htmlFor={id}
      help={help}
    >
      {multiline ? (
        <textarea
          id={id}
          rows={3}
          className={`${inputClass} font-mono`}
          value={value}
          placeholder={hint}
          disabled={disabled}
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => onChange(e.target.value)}
        />
      ) : (
        <input
          id={id}
          type="password"
          className={`${inputClass} font-mono`}
          value={value}
          placeholder={hint}
          disabled={disabled}
          autoComplete="new-password"
          spellCheck={false}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </FormRow>
  );
}

/** Switch is an on/off toggle (role="switch") for list rows. */
export function Switch({ checked, onChange, label, disabled }: { checked: boolean; onChange: (v: boolean) => void; label: string; disabled?: boolean }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors disabled:opacity-50 ${checked ? 'bg-accent' : 'bg-line'}`}
    >
      <span className={`inline-block h-4 w-4 rounded-full bg-page transition-transform ${checked ? 'translate-x-4' : 'translate-x-0.5'}`} />
    </button>
  );
}

/** FormSection groups rows under a heading inside a form or modal. */
export function FormSection({ title, children, description }: { title: string; children: ReactNode; description?: ReactNode }) {
  return (
    <section className="mb-6">
      <h3 className="mb-1 border-b border-line pb-1 text-sm font-semibold uppercase tracking-wide text-ink-muted">{title}</h3>
      {description && <p className="mb-3 text-xs text-ink-muted">{description}</p>}
      <div className="mt-3">{children}</div>
    </section>
  );
}
