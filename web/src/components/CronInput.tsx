import { useId, useState, type ReactNode } from 'react';
import type { CronSchedule } from '@/api/types';
import { describeCron, validateCron, type CronPreset } from '@/lib/cron';
import { FormRow, inputClass } from './Form';

const MANUAL = 'manual';
const CUSTOM = 'custom';

/**
 * CronInput edits a {cron, enabled} schedule: a preset, "Manual only" (enabled = false, a valid
 * cron is kept; an invalid one is dropped, since the server refuses it even on a disabled
 * schedule) or a custom 5-field cron expression. With allowManual = false the enabled flag is
 * left alone (it is edited elsewhere, e.g. a switch on System → Tasks).
 */
export function CronInput({
  label,
  value,
  onChange,
  presets,
  allowManual = true,
  help,
  warning,
}: {
  label: string;
  value: CronSchedule;
  onChange: (v: CronSchedule) => void;
  presets: CronPreset[];
  allowManual?: boolean;
  help?: ReactNode;
  warning?: ReactNode;
}) {
  const id = useId();
  const presetIndex = presets.findIndex((p) => p.cron === value.cron.trim());
  const manual = allowManual && !value.enabled;
  const [customChosen, setCustomChosen] = useState(presetIndex < 0);
  const mode = manual ? MANUAL : customChosen || presetIndex < 0 ? CUSTOM : `p${presetIndex}`;
  const error = mode === CUSTOM ? validateCron(value.cron) : null;
  const enabledFor = (enabled: boolean) => (allowManual ? enabled : value.enabled);

  function choose(next: string) {
    if (next === MANUAL) {
      onChange({ cron: validateCron(value.cron) ? '' : value.cron, enabled: false });
    } else if (next === CUSTOM) {
      setCustomChosen(true);
      onChange({ cron: value.cron, enabled: enabledFor(true) });
    } else {
      setCustomChosen(false);
      onChange({ cron: presets[Number(next.slice(1))].cron, enabled: enabledFor(true) });
    }
  }

  return (
    <FormRow label={label} htmlFor={id} help={help}>
      <div className="flex flex-col gap-2 sm:flex-row">
        <select id={id} className={`${inputClass} sm:max-w-[16rem]`} value={mode} onChange={(e) => choose(e.target.value)}>
          {presets.map((p, i) => (
            <option key={p.cron} value={`p${i}`}>
              {p.label}
            </option>
          ))}
          <option value={CUSTOM}>Custom (cron)</option>
          {allowManual && <option value={MANUAL}>Manual only (disabled)</option>}
        </select>
        {mode === CUSTOM && (
          <input
            aria-label={`${label} cron expression`}
            aria-invalid={error ? true : undefined}
            className={`${inputClass} font-mono sm:max-w-[14rem]`}
            value={value.cron}
            placeholder="m h dom mon dow"
            spellCheck={false}
            autoComplete="off"
            onChange={(e) => onChange({ cron: e.target.value, enabled: enabledFor(true) })}
          />
        )}
      </div>
      <div className="mt-1 text-xs">
        {error ? (
          <span className="text-danger">{error}</span>
        ) : manual ? (
          <span className="text-ink-muted">Runs only when started by hand.</span>
        ) : (
          <span className="text-ink-muted">
            {describeCron(value.cron)} <code className="ml-1 text-ink-muted/80">{value.cron}</code> (server time zone)
          </span>
        )}
      </div>
      {warning && <div className="mt-1 text-xs text-warn">{warning}</div>}
    </FormRow>
  );
}
