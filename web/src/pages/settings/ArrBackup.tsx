import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Archive, DatabaseBackup } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { Link } from 'react-router';
import { ARR_NAMES, type ArrTestResult, type ArrType } from '@/api/arr';
import {
  ARR_BACKUP_PRESETS,
  DEFAULT_ARR_BACKUP_CRON,
  MANUAL_BACKUP_WARNING,
  arrBackupOf,
  listArrSnapshots,
  startArrBackup,
  type ArrBackupSettings,
} from '@/api/arrBackup';
import { errorMessage } from '@/api/client';
import type { CronSchedule, Destination, Integration, Snapshot } from '@/api/types';
import { Button } from '@/components/Button';
import { CronInput } from '@/components/CronInput';
import { DataTable, type Column } from '@/components/DataTable';
import { CheckboxField, FormRow, FormSection, NumberField, SelectField } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { PathField } from '@/components/PathPicker';
import { Badge } from '@/components/StatusBadge';
import { describeCron, validateCron } from '@/lib/cron';
import { formatBytes, formatDateTime, formatNumber, formatRelative } from '@/lib/format';
import { keys, useDestinations } from '@/lib/lookups';

// The *arr backup parts of Settings → Connect (design phase2-3 §10, §16): the backup fields of the
// *arr form, the backup line of each connection card with "Back up now", and the list of versions.

/** ArrBackupValue is what the form edits: the backup folder and settings.backup. */
export interface ArrBackupValue {
  backupFolder: string;
  destinationId: number;
  schedule: CronSchedule;
  maxScheduledAgeDays: number;
  acceptInsecureModes: boolean;
}

/** arrBackupValue is the form's starting value for an integration (or a new one). */
export function arrBackupValue(it: Integration | null): ArrBackupValue {
  const { backupFolder, backup } = arrBackupOf(it);
  return {
    backupFolder,
    destinationId: backup.destinationId,
    schedule: backup.destinationId > 0 ? { cron: backup.cron, enabled: backup.enabled } : { cron: DEFAULT_ARR_BACKUP_CRON, enabled: true },
    maxScheduledAgeDays: backup.maxScheduledAgeDays,
    acceptInsecureModes: backup.acceptInsecureModes,
  };
}

/** validateArrBackup returns the first problem with the backup fields, or null. */
export function validateArrBackup(v: ArrBackupValue): string | null {
  const folder = v.backupFolder.trim();
  if (folder && !folder.startsWith('/')) {
    return 'The Backups folder must be an absolute path (for example /arr/radarr-backups), or empty to download over HTTP.';
  }
  if (!Number.isInteger(v.maxScheduledAgeDays) || v.maxScheduledAgeDays < 1 || v.maxScheduledAgeDays > 90) {
    return '"Reuse scheduled backups" must be 1 to 90 days.';
  }
  const cron = v.schedule.cron.trim();
  if (v.destinationId > 0 && (v.schedule.enabled || cron)) {
    const err = validateCron(cron);
    if (err) return `The backup schedule is invalid: ${err}`;
  }
  return null;
}

/** arrBackupSettings turns the form's value into the settings fields it owns. */
export function arrBackupSettings(v: ArrBackupValue): { backupFolder: string; backup: ArrBackupSettings } {
  const has = v.destinationId > 0;
  return {
    backupFolder: v.backupFolder.trim(),
    backup: {
      destinationId: has ? v.destinationId : 0,
      cron: has ? v.schedule.cron.trim() : '',
      enabled: has && v.schedule.enabled,
      maxScheduledAgeDays: v.maxScheduledAgeDays,
      acceptInsecureModes: v.acceptInsecureModes,
    },
  };
}

/** keepsModesPrivate reports whether a destination's last probe found that it keeps file modes. */
function keepsModesPrivate(d: Destination | undefined): boolean {
  return d?.capabilities?.enforcesModes === true;
}

const FOLDER_STATUS: Record<string, string> = {
  ok: 'readable',
  missing: 'not found (mount the Backups folder into Bunkarr)',
  unreadable: 'not readable by Bunkarr (check the PUID)',
  'not-set': 'not set: backups are downloaded over HTTP',
};

const HTTP_STATUS: Record<string, string> = {
  ok: 'works',
  'login-required': 'needs a login (set the Backups folder, or Authentication Required to "Disabled for Local Addresses")',
  unknown: 'not checked yet (no backup to try)',
};

/**
 * ArrBackupFields is the Backup section of the *arr form: the Backups folder, the destination,
 * the schedule, the reuse of the *arr's own scheduled backups and, where the destination does
 * not keep files private, the acceptInsecureModes confirmation. test is the last Test result.
 */
export function ArrBackupFields({
  type,
  value,
  onChange,
  test,
}: {
  type: ArrType;
  value: ArrBackupValue;
  onChange: (v: ArrBackupValue) => void;
  test: ArrTestResult | null;
}) {
  const app = ARR_NAMES[type];
  const destinations = useDestinations();
  const set = (patch: Partial<ArrBackupValue>) => onChange({ ...value, ...patch });
  const dest = destinations.data?.find((d) => d.id === value.destinationId);
  const insecure = value.destinationId > 0 && !!dest && !keepsModesPrivate(dest);
  const options = [{ value: '0', label: 'None (no backup)' }, ...(destinations.data ?? []).map((d) => ({ value: String(d.id), label: d.name }))];
  const manual = test?.manualBackups;
  return (
    <FormSection
      title="Backup"
      description={`Copies ${app}'s own backup zip (its settings and database) to a destination, verifies it and keeps versions. The zip holds ${app}'s API key and passwords.`}
    >
      <PathField
        label="Backups folder"
        value={value.backupFolder}
        onChange={(backupFolder) => set({ backupFolder })}
        placeholder={`/arr/${type}-backups`}
        help={
          <>
            {app}&apos;s <code>Backups</code> folder mounted into Bunkarr read-only, for example{' '}
            <code>/mnt/user/appdata/{type}/Backups:/arr/{type}-backups:ro</code>. Leave it empty to download backups over HTTP, which works only when {app} does not
            require a login for Bunkarr&apos;s address.
          </>
        }
      />
      {test?.backup && (
        <FormRow label="Access">
          <ul className="space-y-1 pt-2 text-xs" aria-label="Backup access">
            <li>
              Backups folder: <span className={test.backup.folder === 'ok' || test.backup.folder === 'not-set' ? '' : 'text-warn'}>{FOLDER_STATUS[test.backup.folder] ?? test.backup.folder}</span>
            </li>
            <li>
              Download over HTTP: <span className={test.backup.http === 'login-required' ? 'text-warn' : ''}>{HTTP_STATUS[test.backup.http] ?? test.backup.http}</span>
            </li>
          </ul>
        </FormRow>
      )}
      {manual && (
        <FormRow label="Manual backups">
          <p className={`pt-2 text-xs ${manual.count > MANUAL_BACKUP_WARNING ? 'text-warn' : 'text-ink-muted'}`}>
            {formatNumber(manual.count)} in {app} ({formatBytes(manual.bytes)}).{' '}
            {manual.count > MANUAL_BACKUP_WARNING
              ? `${app} never deletes manual backups: remove old ones in ${app} → System → Backup. Reusing its scheduled backups (below) makes fewer.`
              : `${app} keeps manual backups for good; Bunkarr makes one only when no recent scheduled backup exists.`}
          </p>
        </FormRow>
      )}
      <SelectField
        label="Destination"
        value={String(value.destinationId)}
        onChange={(v) => set({ destinationId: Number(v) })}
        options={options}
      />
      {value.destinationId > 0 && (
        <>
          <CronInput label="Schedule" value={value.schedule} onChange={(schedule) => set({ schedule })} presets={ARR_BACKUP_PRESETS} />
          <NumberField
            label="Reuse scheduled backups"
            value={value.maxScheduledAgeDays}
            onChange={(maxScheduledAgeDays) => set({ maxScheduledAgeDays })}
            min={1}
            max={90}
            suffix="days old at most"
            help={`When ${app}'s own scheduled backup is younger than this, Bunkarr copies it instead of asking ${app} for a new one (${app} makes one every 7 days by default).`}
          />
          {(insecure || value.acceptInsecureModes) && (
            <>
              {insecure && (
                <Notice tone="warning">
                  {dest?.name} does not keep files private (an SMB share without POSIX extensions), or was checked before Bunkarr tested this: test it again under
                  Destinations. The backup holds {app}&apos;s API key and passwords.
                </Notice>
              )}
              <CheckboxField
                label="Insecure modes"
                checked={value.acceptInsecureModes}
                onChange={(acceptInsecureModes) => set({ acceptInsecureModes })}
                text="Accept insecure file modes: write the backups there anyway"
              />
            </>
          )}
          <p className="mb-4 text-xs text-ink-muted sm:ml-[12rem]">
            Versions kept are set per destination (Destinations → Retention, *arr daily and weekly versions).
          </p>
        </>
      )}
    </FormSection>
  );
}

/** ArrBackupSummary is the backup line of an *arr connection card, with "Back up now" and the versions. */
export function ArrBackupSummary({ integration: i, onNotice }: { integration: Integration; onNotice: (n: { tone: 'success' | 'error'; body: ReactNode }) => void }) {
  const qc = useQueryClient();
  const destinations = useDestinations();
  const [versions, setVersions] = useState(false);
  const { backupFolder, backup } = arrBackupOf(i);
  const destName = (id: number) => destinations.data?.find((d) => d.id === id)?.name ?? `destination #${id}`;
  const runner = useMutation({
    mutationFn: () => startArrBackup(i.id),
    onSuccess: async (job) => {
      onNotice({
        tone: 'success',
        body: (
          <>
            Backup queued.{' '}
            <Link className="text-accent hover:underline" to={`/activity/jobs/${job.id}`}>
              Open the job
            </Link>
          </>
        ),
      });
      await qc.invalidateQueries({ queryKey: keys.jobs });
    },
    onError: (e) => onNotice({ tone: 'error', body: errorMessage(e) }),
  });
  return (
    <>
      <dt className="text-ink-muted">Backup</dt>
      <dd className="text-xs">
        {backup.destinationId > 0 ? (
          <>
            to {destName(backup.destinationId)}, {backup.enabled && backup.cron ? describeCron(backup.cron) : 'manual only'},{' '}
            {backupFolder ? <>from {backupFolder}</> : 'over HTTP'}
          </>
        ) : (
          <span className="text-ink-muted">not set up</span>
        )}
        <div className="mt-1 flex flex-wrap gap-1">
          <Button
            small
            icon={DatabaseBackup}
            busy={runner.isPending}
            disabled={!i.enabled || backup.destinationId <= 0}
            title={backup.destinationId <= 0 ? 'Choose a backup destination first' : undefined}
            onClick={() => runner.mutate()}
          >
            Back up now
          </Button>
          <Button small variant="ghost" icon={Archive} onClick={() => setVersions(true)}>
            Backups
          </Button>
        </div>
        {versions && <ArrSnapshotsDialog integration={i} onClose={() => setVersions(false)} />}
      </dd>
    </>
  );
}

/** ArrSnapshotsDialog lists an *arr's backup versions at every destination (no download: S17). */
export function ArrSnapshotsDialog({ integration, onClose }: { integration: Integration; onClose: () => void }) {
  const list = useQuery({ queryKey: ['integrations', integration.id, 'arr', 'snapshots'], queryFn: () => listArrSnapshots(integration.id) });
  const destinations = useDestinations();
  const app = ARR_NAMES[integration.type as ArrType] ?? integration.type;
  const destName = (id: number) => destinations.data?.find((d) => d.id === id)?.name ?? `destination #${id}`;
  const columns: Column<Snapshot>[] = [
    { key: 'created', header: 'Stored', className: 'whitespace-nowrap', cell: (s) => <span title={formatRelative(s.createdAt)}>{formatDateTime(s.createdAt)}</span> },
    { key: 'destination', header: 'Destination', cell: (s) => destName(s.destinationId) },
    { key: 'backup', header: `${app} backup`, cell: (s) => <BackupName snapshot={s} /> },
    { key: 'size', header: 'Size', className: 'whitespace-nowrap text-right', cell: (s) => formatBytes(s.size) },
    { key: 'integrity', header: 'Integrity', cell: (s) => (s.integrity === 'ok' ? <Badge tone="ok">OK</Badge> : <Badge tone="danger">Failed</Badge>) },
  ];
  return (
    <Modal
      title={`${app} backups · ${integration.name}`}
      size="xl"
      onClose={onClose}
      footer={
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <p className="mb-3 text-xs text-ink-muted">
        Verified copies of {app}&apos;s backup zip, stored under <code>.bunkarr/arr/</code> on each destination (files 0600). They hold {app}&apos;s API key and
        passwords, so Bunkarr never offers them for download: restore one from the share with {app} → System → Backup → Restore.
      </p>
      <ErrorNotice error={list.error} />
      <DataTable columns={columns} rows={list.data} rowKey={(s) => s.id} loading={list.isPending} empty={`No ${app} backups yet.`} caption={`${app} backups`} />
    </Modal>
  );
}

/** BackupName shows the zip's name and whether it was the *arr's own scheduled backup. */
function BackupName({ snapshot }: { snapshot: Snapshot }) {
  const m = (snapshot.manifest ?? {}) as { backup?: { name?: string; type?: string }; method?: string };
  return (
    <div>
      <code className="break-all text-xs">{m.backup?.name ?? snapshot.path}</code>
      <div className="text-xs text-ink-muted">
        {m.backup?.type === 'scheduled' ? 'its scheduled backup' : m.backup?.type === 'manual' ? 'made for Bunkarr' : (m.backup?.type ?? '')}
        {snapshot.method === 'arr_api_folder' ? ' · from the Backups folder' : snapshot.method === 'arr_api_http' ? ' · over HTTP' : ''}
      </div>
    </div>
  );
}
