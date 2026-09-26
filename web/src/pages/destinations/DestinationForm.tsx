import { useMutation, useQueryClient } from '@tanstack/react-query';
import { FlaskConical } from 'lucide-react';
import { useState, type FormEvent } from 'react';
import { createDestination, testDestination, testTarget, updateDestination } from '@/api/destinations';
import type { AdoptMode, CronSchedule, Destination, DestinationInput, DestinationSettings, DestinationTestResult, HardlinkMode, Retention, VerifyMode } from '@/api/types';
import { Button } from '@/components/Button';
import { CronInput } from '@/components/CronInput';
import { Checkbox, CheckboxField, FormRow, FormSection, NumberField, SelectField, TextField } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { PathField } from '@/components/PathPicker';
import { DEFAULT_SYNC_CRON, DEFAULT_VERIFY_CRON, SCHEDULE_PRESETS, validateCron } from '@/lib/cron';
import { keys, useSources } from '@/lib/lookups';
import { TestResultView } from './TestResultView';

export const DEFAULT_SETTINGS: DestinationSettings = {
  verify: { mode: 'sample', samplePercent: 5 },
  hardlinks: 'recreate',
  adoptExisting: 'size+mtime',
  mtimeWindowSec: 0,
  maxChangePercent: 10,
  maxChangeFiles: 1000,
};

export const DEFAULT_RETENTION: Retention = { deletedDays: 30, plexDbDaily: 14, plexDbWeekly: 8 };

/** needsAttach: the target already holds a Bunkarr marker, so creating must adopt it. */
export function needsAttach(t: DestinationTestResult | null): boolean {
  return !!t && (t.marker === 'foreign' || t.marker === 'mismatch');
}

/** validate returns the first problem with a destination form, or null. */
export function validateDestination(d: DestinationInput): string | null {
  if (!d.name.trim()) return 'Enter a name.';
  if (!d.target.trim()) return 'Choose the target folder.';
  // As the server (validateSchedule): an enabled schedule needs a cron expression, and any
  // non-empty one must be valid, even on a schedule that is off.
  for (const [label, s] of [
    ['sync schedule', d.schedule],
    ['verify schedule', d.verifySchedule],
  ] as const) {
    const err = s.enabled || s.cron.trim() ? validateCron(s.cron) : null;
    if (err) return `The ${label} is invalid: ${err}`;
  }
  const s = d.settings;
  const r = d.retention;
  const whole = (n: number, min: number, max = Number.MAX_SAFE_INTEGER) => Number.isInteger(n) && n >= min && n <= max;
  if (s.verify.mode === 'sample' && !whole(s.verify.samplePercent, 1, 100)) return 'The verify sample must be 1–100 %.';
  if (!whole(s.mtimeWindowSec, 0, 3600)) return 'The time window must be 0–3600 seconds.';
  if (!whole(s.maxChangePercent, 1, 100)) return 'The change limit must be 1–100 %.';
  if (!whole(s.maxChangeFiles, 1)) return 'The file change limit must be at least 1.';
  // The server's ranges (internal/destinations Retention.Normalize); 0 would mean "the default".
  if (!whole(r.deletedDays, 1, 3650)) return 'Keep deleted files for 1–3650 days.';
  if (!whole(r.plexDbDaily, 1, 365)) return 'Keep 1–365 daily Plex database versions.';
  if (!whole(r.plexDbWeekly, 1, 520)) return 'Keep 1–520 weekly Plex database versions.';
  // Missing on a destination saved before *arr backups: the server fills the defaults.
  if (r.arrDaily !== undefined && !whole(r.arrDaily, 1, 365)) return 'Keep 1–365 daily *arr backup versions.';
  if (r.arrWeekly !== undefined && !whole(r.arrWeekly, 1, 520)) return 'Keep 1–520 weekly *arr backup versions.';
  // manifestWeeks 0 is a value on the server (no weekly manifest versions), not the default.
  if (r.manifestDays !== undefined && !whole(r.manifestDays, 1, 3650)) return 'Keep 1–3650 daily manifest versions.';
  if (r.manifestWeeks !== undefined && !whole(r.manifestWeeks, 0, 520)) return 'Keep 0–520 weekly manifest versions.';
  return null;
}

/**
 * DestinationForm adds or edits a filecopy destination. A new destination must be tested first:
 * the probe reports the marker, filesystem and capabilities, and when it finds an existing marker
 * or a local filesystem the user must confirm attach / allowLocal explicitly (safety rule S3).
 */
export function DestinationForm({ destination, onClose }: { destination: Destination | null; onClose: () => void }) {
  const qc = useQueryClient();
  const sources = useSources();
  const editing = destination != null;
  const [name, setName] = useState(destination?.name ?? '');
  const [target, setTarget] = useState(destination?.target ?? '');
  const [enabled, setEnabled] = useState(destination?.enabled ?? true);
  const [sourceIds, setSourceIds] = useState<number[]>(destination?.sourceIds ?? []);
  const [schedule, setSchedule] = useState<CronSchedule>(destination?.schedule ?? { cron: DEFAULT_SYNC_CRON, enabled: true });
  const [verifySchedule, setVerifySchedule] = useState<CronSchedule>(destination?.verifySchedule ?? { cron: DEFAULT_VERIFY_CRON, enabled: true });
  const [settings, setSettings] = useState<DestinationSettings>({ ...DEFAULT_SETTINGS, ...destination?.settings, verify: { ...DEFAULT_SETTINGS.verify, ...destination?.settings?.verify } });
  const [retention, setRetention] = useState<Retention>({ ...DEFAULT_RETENTION, ...destination?.retention });
  const [attach, setAttach] = useState(false);
  const [allowLocal, setAllowLocal] = useState(false);
  const [test, setTest] = useState<{ target: string; result: DestinationTestResult } | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  // Counts Save clicks, so a repeated form error is shown (scrolled into view) again.
  const [attempt, setAttempt] = useState(0);

  const tester = useMutation({
    mutationFn: (t: string) => (editing ? testDestination(destination.id) : testTarget(t)),
    onSuccess: (result, t) => {
      setTest({ target: t, result });
      setAttach(false);
      setAllowLocal(false);
      // "Test the target first" and similar problems are answered by a new test.
      setFormError(null);
    },
  });
  const saver = useMutation({
    mutationFn: (body: DestinationInput) => (editing ? updateDestination(destination.id, body) : createDestination(body)),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: keys.destinations });
      await qc.invalidateQueries({ queryKey: keys.schedules });
      onClose();
    },
  });

  const current = test && test.target === target.trim() ? test.result : null;
  const caps = current?.capabilities ?? destination?.capabilities ?? null;
  const set = (patch: Partial<DestinationSettings>) => setSettings({ ...settings, ...patch });

  function submit(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    setAttempt((n) => n + 1);
    const body: DestinationInput = {
      name: name.trim(),
      engine: 'filecopy',
      target: target.trim(),
      enabled,
      sourceIds,
      schedule: { cron: schedule.cron.trim(), enabled: schedule.enabled },
      verifySchedule: { cron: verifySchedule.cron.trim(), enabled: verifySchedule.enabled },
      settings,
      retention,
    };
    const problem = validateDestination(body);
    if (problem) {
      setFormError(problem);
      return;
    }
    if (!editing) {
      if (!current) {
        setFormError('Test the target first: Bunkarr checks it is the right filesystem before creating the destination.');
        return;
      }
      if (!current.ok) {
        setFormError(`The target cannot be used: ${current.message || 'see the test result'}. Fix it, then test again.`);
        return;
      }
      if (!current.writable) {
        setFormError('The target is not writable by Bunkarr. Check the mount and the PUID/PGID permissions, then test again.');
        return;
      }
      if (needsAttach(current) && !attach) {
        setFormError('The target already holds a Bunkarr destination. Confirm that you want to attach to it.');
        return;
      }
      if (current.local && !allowLocal) {
        setFormError('The target is on a local filesystem. Confirm that this is intended.');
        return;
      }
      if (attach) body.attach = true;
      if (allowLocal) body.allowLocal = true;
    }
    saver.mutate(body);
  }

  const toggleSource = (id: number, on: boolean) => setSourceIds(on ? [...sourceIds, id] : sourceIds.filter((x) => x !== id));

  return (
    <Modal
      title={editing ? `Edit destination · ${destination.name}` : 'Add destination'}
      size="xl"
      onClose={onClose}
      footer={
        <>
          <Button icon={FlaskConical} busy={tester.isPending} disabled={!target.trim()} onClick={() => tester.mutate(target.trim())}>
            Test
          </Button>
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="destination-form" busy={saver.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="destination-form" onSubmit={submit} noValidate>
        {formError && (
          <Notice key={attempt} tone="error">
            {formError}
          </Notice>
        )}
        <ErrorNotice error={saver.error ?? tester.error} />

        <FormSection title="Target">
          <TextField label="Name" value={name} onChange={setName} autoFocus={!editing} placeholder="UNAS" />
          <PathField
            label="Target"
            value={target}
            onChange={setTarget}
            disabled={editing}
            placeholder="/backup"
            help={
              editing
                ? 'The target cannot change: the destination is tied to the marker and filesystem recorded when it was created.'
                : 'The mounted NAS share as Bunkarr sees it (for example /backup). It must already exist; Bunkarr never creates it, and refuses an empty mount point on the local disk.'
            }
          />
          {!editing && !current && (
            <p className="-mt-2 mb-4 text-xs text-ink-muted sm:ml-[12rem]">Test the target before saving: the probe checks the marker, the filesystem and what it can store.</p>
          )}
          {current && <TestResultView result={current} creating={!editing} />}
          {!editing && current && needsAttach(current) && (
            <div className="mb-4 rounded border border-warn/50 bg-warn/10 p-3 text-sm">
              <Checkbox
                label="Attach to the existing Bunkarr destination in this folder"
                help="The folder holds .bunkarr/destination.json from an earlier setup. Attaching adopts that destination's id and continues its backup there; files already present are adopted or kept in retention, never overwritten."
                checked={attach}
                onChange={setAttach}
              />
            </div>
          )}
          {!editing && current?.local && (
            <div className="mb-4 rounded border border-warn/50 bg-warn/10 p-3 text-sm">
              <Checkbox
                label="Yes, back up to this local filesystem"
                help="The target is on the same filesystem as / or the config folder, or on tmpfs/overlay. That usually means the NAS share is not mounted and this is the empty mount point inside the container. Only allow it if you deliberately back up to a local disk."
                checked={allowLocal}
                onChange={setAllowLocal}
              />
            </div>
          )}
          <CheckboxField label="Enabled" text="Run scheduled and manual syncs for this destination" checked={enabled} onChange={setEnabled} />
        </FormSection>

        <FormSection title="Sources" description="The sources mirrored to this destination, each under its destination folder.">
          {sources.data && sources.data.length === 0 && <p className="text-sm text-ink-muted">No sources yet: add them under Library.</p>}
          <fieldset>
            <legend className="sr-only">Sources</legend>
            <div className="grid gap-2 sm:grid-cols-2">
              {(sources.data ?? []).map((s) => (
                <Checkbox
                  key={s.id}
                  label={s.name}
                  help={`${s.path} → ${s.destFolder}/`}
                  checked={sourceIds.includes(s.id)}
                  onChange={(on) => toggleSource(s.id, on)}
                />
              ))}
            </div>
          </fieldset>
        </FormSection>

        <FormSection title="Schedules" description="Times use the container's time zone (TZ).">
          <CronInput label="Sync" value={schedule} onChange={setSchedule} presets={SCHEDULE_PRESETS} />
          <CronInput
            label="Verify"
            value={verifySchedule}
            onChange={setVerifySchedule}
            presets={SCHEDULE_PRESETS}
            help="Checks that every backed-up file is still there with the right size, and re-reads a sample to compare hashes."
          />
          <SelectField<VerifyMode>
            label="Verify mode"
            value={settings.verify.mode}
            onChange={(mode) => set({ verify: { ...settings.verify, mode } })}
            options={[
              { value: 'sample', label: 'Sample: re-read a share of the files' },
              { value: 'full', label: 'Full: re-read every file (slow)' },
              { value: 'off', label: 'Off: check existence and size only' },
            ]}
            help="Copies are always checked for size. Verify also re-reads files and compares their hash with the one recorded when they were copied."
          />
          {settings.verify.mode === 'sample' && (
            <NumberField
              label="Sample"
              value={settings.verify.samplePercent}
              onChange={(samplePercent) => set({ verify: { ...settings.verify, samplePercent } })}
              min={1}
              max={100}
              suffix="% of files per verify run"
            />
          )}
        </FormSection>

        <FormSection title="Copying">
          <SelectField<HardlinkMode>
            label="Hardlinks"
            value={settings.hardlinks}
            onChange={(hardlinks) => set({ hardlinks })}
            options={[
              { value: 'recreate', label: 'Recreate hardlinks at the destination' },
              { value: 'copy', label: 'Store once, record the other names' },
            ]}
            help={
              <>
                Either way hardlinked content is copied once.{' '}
                {caps && caps.hardlinks === false && settings.hardlinks === 'recreate' && (
                  <span className="text-warn">This destination cannot store hardlinks, so the other names are recorded only (a restore recreates them).</span>
                )}
              </>
            }
          />
          <SelectField<AdoptMode>
            label="Adopt existing files"
            value={settings.adoptExisting}
            onChange={(adoptExisting) => set({ adoptExisting })}
            options={[
              { value: 'size+mtime', label: 'When size and modification time match' },
              { value: 'size+hash', label: 'When size and content hash match (slow, one-time)' },
              { value: 'off', label: 'Never (move unknown files into retention)' },
            ]}
            help="For switching from rsync: a file already at the destination that matches the source is recorded instead of copied again. Files that do not match are moved into retention, never overwritten."
          />
          {settings.adoptExisting === 'size+mtime' && (
            <NumberField
              label="Time window"
              value={settings.mtimeWindowSec}
              onChange={(mtimeWindowSec) => set({ mtimeWindowSec })}
              min={0}
              max={3600}
              suffix="seconds"
              help="Tolerance for modification times, like rsync --modify-window. Use 1–2 for FAT or older SMB servers; 0 otherwise."
            />
          )}
        </FormSection>

        <FormSection
          title="Mass-change guard"
          description="When a sync would retain or update more files than this, those changes are held (nothing moves) and you are notified; review the job and apply them. An update that empties a file or shrinks it to less than half is always held."
        >
          <NumberField
            label="Max changes"
            value={settings.maxChangePercent}
            onChange={(maxChangePercent) => set({ maxChangePercent })}
            min={1}
            max={100}
            suffix="% of a source's files (and more than 20 files)"
          />
          <NumberField label="Max changed files" value={settings.maxChangeFiles} onChange={(maxChangeFiles) => set({ maxChangeFiles })} min={1} suffix="files per sync" />
        </FormSection>

        <FormSection title="Retention" description="Files deleted or replaced at the source are kept in the destination's retention folder, then removed.">
          <NumberField
            label="Keep deleted files"
            value={retention.deletedDays}
            onChange={(deletedDays) => setRetention({ ...retention, deletedDays })}
            min={1}
            max={3650}
            suffix="days"
          />
          <NumberField
            label="Plex DB daily versions"
            value={retention.plexDbDaily}
            onChange={(plexDbDaily) => setRetention({ ...retention, plexDbDaily })}
            min={1}
            max={365}
            suffix="newest good versions"
          />
          <NumberField
            label="Plex DB weekly versions"
            value={retention.plexDbWeekly}
            onChange={(plexDbWeekly) => setRetention({ ...retention, plexDbWeekly })}
            min={1}
            max={520}
            suffix="weeks (newest version of each)"
            help="The newest good version is never deleted; failed versions are kept 7 days for diagnosis."
          />
          <NumberField
            label="*arr daily versions"
            value={retention.arrDaily ?? 14}
            onChange={(arrDaily) => setRetention({ ...retention, arrDaily })}
            min={1}
            max={365}
            suffix="newest good versions per *arr"
          />
          <NumberField
            label="*arr weekly versions"
            value={retention.arrWeekly ?? 8}
            onChange={(arrWeekly) => setRetention({ ...retention, arrWeekly })}
            min={1}
            max={520}
            suffix="weeks (newest version of each)"
            help="Versions of the Sonarr, Radarr and Lidarr backups, kept like the Plex database versions."
          />
          <NumberField
            label="Manifest daily versions"
            value={retention.manifestDays ?? 30}
            onChange={(manifestDays) => setRetention({ ...retention, manifestDays })}
            min={1}
            max={3650}
            suffix="days (newest version of each)"
          />
          <NumberField
            label="Manifest weekly versions"
            value={retention.manifestWeeks ?? 12}
            onChange={(manifestWeeks) => setRetention({ ...retention, manifestWeeks })}
            min={0}
            max={520}
            suffix="weeks (newest version of each; 0 keeps none)"
            help="The newest good manifest version is never deleted."
          />
        </FormSection>
        {editing && (
          <FormRow label="Engine">
            <p className="pt-2 text-sm text-ink-muted">filecopy (mounted share)</p>
          </FormRow>
        )}
      </form>
    </Modal>
  );
}
