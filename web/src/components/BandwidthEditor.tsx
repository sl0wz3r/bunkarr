import { Plus, Trash2 } from 'lucide-react';
import { useId } from 'react';
import type { Bandwidth, BandwidthEntry, TransferWindow, Weekday } from '@/api/types';
import {
  DAY_LABELS,
  DAYS,
  DEFAULT_ENTRY,
  DEFAULT_WINDOW,
  MAX_GRACE_MINUTES,
  MAX_KIBPS,
  MAX_TIMETABLE_ENTRIES,
  describeWindow,
  formatKiBps,
  type BandwidthProblems,
} from '@/lib/bandwidth';
import { Button, IconButton } from './Button';
import { Checkbox, FormRow, NumberField } from './Form';

// The bandwidth editor of a destination (docs/design/phase4.md §9.1, §9.2, §15 step 7): the base
// limits, the weekly timetable of other limits (with its overlaps shown per day on the line that
// overlaps), and the transfer window. Every control wraps at phone widths.

const small = 'rounded border border-line bg-page px-2 py-1 text-sm outline-none focus:border-accent';

function DayPicker({ label, days, onChange }: { label: string; days: Weekday[]; onChange: (d: Weekday[]) => void }) {
  return (
    <fieldset className="min-w-0">
      <legend className="sr-only">{label}</legend>
      <div className="flex flex-wrap gap-1">
        {DAYS.map((d) => {
          const on = days.includes(d);
          // The checkbox is visually hidden: its label shows the keyboard focus (WCAG 2.4.7).
          return (
            <label
              key={d}
              className={`cursor-pointer select-none rounded border px-2 py-1 text-xs has-[:focus-visible]:outline has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-accent ${on ? 'border-accent/60 bg-accent/15 text-accent' : 'border-line text-ink-muted'}`}
            >
              <input
                type="checkbox"
                className="sr-only"
                checked={on}
                onChange={(e) => onChange(e.target.checked ? DAYS.filter((x) => x === d || days.includes(x)) : days.filter((x) => x !== d))}
              />
              {DAY_LABELS[d]}
            </label>
          );
        })}
      </div>
    </fieldset>
  );
}

function TimeInput({ label, value, onChange }: { label: string; value: string; onChange: (v: string) => void }) {
  return (
    <input
      aria-label={label}
      className={`${small} w-20 font-mono`}
      value={value}
      placeholder="HH:MM"
      inputMode="numeric"
      maxLength={5}
      spellCheck={false}
      autoComplete="off"
      onChange={(e) => onChange(e.target.value)}
    />
  );
}

function RateInput({ label, value, onChange }: { label: string; value: number; onChange: (v: number) => void }) {
  return (
    <input
      aria-label={label}
      type="number"
      min={0}
      max={MAX_KIBPS}
      className={`${small} w-28`}
      value={Number.isNaN(value) ? '' : value}
      onChange={(e) => onChange(e.target.value === '' ? Number.NaN : Number(e.target.value))}
    />
  );
}

function TimetableLine({
  index,
  entry,
  problems,
  onChange,
  onRemove,
}: {
  index: number;
  entry: BandwidthEntry;
  problems: string[] | undefined;
  onChange: (e: BandwidthEntry) => void;
  onRemove: () => void;
}) {
  const n = index + 1;
  const errId = useId();
  const set = (patch: Partial<BandwidthEntry>) => onChange({ ...entry, ...patch });
  return (
    // The group is inside the list item, so the list keeps its items for screen readers.
    <li>
      <div
        role="group"
        aria-label={`Timetable line ${n}`}
        aria-describedby={problems?.length ? errId : undefined}
        className={`rounded border p-2 ${problems?.length ? 'border-danger/60' : 'border-line/70'}`}
      >
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-xs font-medium text-ink-muted">Line {n}</span>
          <DayPicker label={`Days of line ${n}`} days={entry.days} onChange={(days) => set({ days })} />
          <IconButton label={`Remove timetable line ${n}`} icon={Trash2} className="ml-auto hover:text-danger" onClick={onRemove} />
        </div>
        <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
          <TimeInput label={`Line ${n} from`} value={entry.from} onChange={(from) => set({ from })} />
          <span className="text-ink-muted">to</span>
          <TimeInput label={`Line ${n} to`} value={entry.to} onChange={(to) => set({ to })} />
          <span className="text-ink-muted">up</span>
          <RateInput label={`Line ${n} upload limit`} value={entry.uploadKiBps} onChange={(uploadKiBps) => set({ uploadKiBps })} />
          <span className="text-ink-muted">down</span>
          <RateInput label={`Line ${n} download limit`} value={entry.downloadKiBps} onChange={(downloadKiBps) => set({ downloadKiBps })} />
          <span className="text-xs text-ink-muted">KiB/s</span>
        </div>
        {problems && problems.length > 0 && (
          <ul id={errId} className="mt-1 space-y-0.5 text-xs text-danger">
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}

/**
 * BandwidthEditor edits a destination's bandwidth. problems comes from validateBandwidth(value), so
 * the form and the editor agree on what is wrong.
 */
export function BandwidthEditor({ value, onChange, problems }: { value: Bandwidth; onChange: (v: Bandwidth) => void; problems: BandwidthProblems }) {
  const timetable = value.timetable ?? [];
  const setLine = (i: number, e: BandwidthEntry) => onChange({ ...value, timetable: timetable.map((x, j) => (j === i ? e : x)) });
  const win = value.window;
  const setWindow = (patch: Partial<TransferWindow>) => onChange({ ...value, window: { ...(win ?? DEFAULT_WINDOW), ...patch } });
  return (
    <>
      <NumberField
        label="Upload limit"
        value={value.uploadKiBps}
        onChange={(uploadKiBps) => onChange({ ...value, uploadKiBps })}
        min={0}
        max={MAX_KIBPS}
        suffix={`KiB/s (0 = unlimited${value.uploadKiBps > 0 ? `; ${formatKiBps(value.uploadKiBps)}` : ''})`}
        help="Applies whenever no timetable line covers the time."
      />
      <NumberField
        label="Download limit"
        value={value.downloadKiBps}
        onChange={(downloadKiBps) => onChange({ ...value, downloadKiBps })}
        min={0}
        max={MAX_KIBPS}
        suffix="KiB/s (0 = unlimited)"
        help="Verify and restores read back through it."
      />
      {problems.base.length > 0 && <p className="-mt-2 mb-4 text-xs text-danger sm:ml-[12rem]">{problems.base[0]}</p>}
      <FormRow
        label="Timetable"
        group
        help="Other limits at set times, in the container's time zone. A line whose end is before its start runs into the next day (23:00–06:00). Lines of the same day may not overlap."
      >
        {timetable.length > 0 && (
          <ul className="mb-2 space-y-2" aria-label="Timetable">
            {timetable.map((e, i) => (
              <TimetableLine
                key={i}
                index={i}
                entry={e}
                problems={problems.entries[i]}
                onChange={(x) => setLine(i, x)}
                onRemove={() => onChange({ ...value, timetable: timetable.filter((_, j) => j !== i) })}
              />
            ))}
          </ul>
        )}
        {problems.timetable.length > 0 && <p className="mb-2 text-xs text-danger">{problems.timetable[0]}</p>}
        <Button small icon={Plus} disabled={timetable.length >= MAX_TIMETABLE_ENTRIES} onClick={() => onChange({ ...value, timetable: [...timetable, { ...DEFAULT_ENTRY }] })}>
          Add a timetable line
        </Button>
      </FormRow>
      <FormRow
        label="Transfer window"
        group
        help="Syncs, verifies and retention run only inside the window; a job still running at its end stops cleanly and continues in the next window. Plex DB, *arr and manifest backups ignore it (the limits apply)."
      >
        <div className="pt-2">
          <Checkbox
            label="Only transfer inside a window"
            checked={!!win}
            onChange={(on) => onChange({ ...value, window: on ? { ...DEFAULT_WINDOW, days: [...DEFAULT_WINDOW.days] } : null })}
          />
        </div>
        {win && (
          <div role="group" aria-label="Window" className={`mt-2 rounded border p-2 ${problems.window.length ? 'border-danger/60' : 'border-line/70'}`}>
            <DayPicker label="Days of the window" days={win.days} onChange={(days) => setWindow({ days })} />
            <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
              <TimeInput label="Window from" value={win.from} onChange={(from) => setWindow({ from })} />
              <span className="text-ink-muted">to</span>
              <TimeInput label="Window to" value={win.to} onChange={(to) => setWindow({ to })} />
              <span className="text-ink-muted">grace</span>
              <input
                aria-label="Grace minutes"
                type="number"
                min={0}
                max={MAX_GRACE_MINUTES}
                className={`${small} w-20`}
                value={Number.isNaN(win.graceMinutes) ? '' : win.graceMinutes}
                onChange={(e) => setWindow({ graceMinutes: e.target.value === '' ? Number.NaN : Number(e.target.value) })}
              />
              <span className="text-xs text-ink-muted">minutes after the end to finish a file</span>
            </div>
            <div className="mt-2">
              <Checkbox
                label="Let a file larger than the window run past its end"
                help="Otherwise a file that cannot be transferred within one whole window at the limit in force fails with a warning (and the job completes). With this on, such a file starts alone when the window opens and may run past its end; nothing else starts after the end."
                checked={win.allowOverrun}
                onChange={(allowOverrun) => setWindow({ allowOverrun })}
              />
            </div>
            {problems.window.length > 0 ? (
              <ul className="mt-1 space-y-0.5 text-xs text-danger">
                {problems.window.map((p) => (
                  <li key={p}>{p}</li>
                ))}
              </ul>
            ) : (
              <p className="mt-1 text-xs text-ink-muted">{describeWindow(win)}</p>
            )}
          </div>
        )}
      </FormRow>
    </>
  );
}

