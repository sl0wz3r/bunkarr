import { useMutation, useQueryClient } from '@tanstack/react-query';
import { FlaskConical } from 'lucide-react';
import { useState, type FormEvent } from 'react';
import { createDestination, testConnection, testDestination, testStoredDestination, testTarget, updateDestination } from '@/api/destinations';
import type {
  AdoptMode,
  B2Remote,
  Bandwidth,
  CredentialsInput,
  CronSchedule,
  Destination,
  DestinationInput,
  DestinationSettings,
  DestinationTestResult,
  DestKind,
  EncryptionInput,
  EngineName,
  EngineTestInput,
  EngineTestResult,
  HardlinkMode,
  Retention,
  S3Remote,
  SftpRemote,
  VerifyMode,
} from '@/api/types';
import { BandwidthEditor } from '@/components/BandwidthEditor';
import { Button } from '@/components/Button';
import { CronInput } from '@/components/CronInput';
import { Checkbox, CheckboxField, FormRow, FormSection, NumberField, SelectField, TextField } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice, WarningList } from '@/components/Notice';
import { isPasswordError, PasswordConfirm } from '@/components/PasswordConfirm';
import { PathField } from '@/components/PathPicker';
import { Badge } from '@/components/StatusBadge';
import { bandwidthOf, normalizeBandwidth, validateBandwidth } from '@/lib/bandwidth';
import { DEFAULT_SYNC_CRON, DEFAULT_VERIFY_CRON, SCHEDULE_PRESETS, validateCron } from '@/lib/cron';
import {
  DEFAULT_RETENTION_CRON,
  EMPTY_B2,
  EMPTY_S3,
  EMPTY_SFTP,
  ENGINE_BADGES,
  KIND_LABELS,
  MAX_TRANSFERS,
  PRUNE_MAX_UNUSED,
  RETENTION_PRESETS,
  SNAPSHOT_RETENTION,
  createNeedsPassword,
  credentialProblem,
  engineSettingsDefaults,
  isRemoteKind,
  kitState,
  locationProblem,
  pendingAt,
  remoteFor,
  sameHostKeys,
  secretProblem,
  typedCredentials,
  updateNeedsPassword,
} from '@/lib/destinationKinds';
import { keys, useDestinations, useSources } from '@/lib/lookups';
import { B2Fields, CredentialFields, HostKeysField, S3Fields, SftpFields } from './ConnectionFields';
import { EncryptionFields, EngineTransferFields, HowField, SampleCapField, SnapshotRetentionFields, WhereField, type EncryptionChoice } from './EngineSections';
import { RecoveryKitPanel } from './RecoveryKit';
import { EngineTestView, TestResultView } from './TestResultView';

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

/** engineNeedsAttach: a restic repository or an rclone Bunkarr marker is already at the location. */
export function engineNeedsAttach(t: EngineTestResult | null, engine: EngineName): boolean {
  if (!t) return false;
  return engine === 'restic' ? t.repository === 'exists' || t.repository === 'locked' : t.marker === 'ok';
}

const whole = (n: number | undefined, min: number, max = Number.MAX_SAFE_INTEGER) => n !== undefined && Number.isInteger(n) && n >= min && n <= max;

/** validate returns the first problem with a destination form, or null. */
export function validateDestination(d: DestinationInput): string | null {
  if (!d.name.trim()) return 'Enter a name.';
  if ((d.kind ?? 'local') === 'local' && !d.target.trim()) return 'Choose the target folder.';
  // As the server (validateSchedule): an enabled schedule needs a cron expression, and any
  // non-empty one must be valid, even on a schedule that is off.
  const schedules: [string, CronSchedule | undefined][] = [
    ['sync schedule', d.schedule],
    ['verify schedule', d.verifySchedule],
    ['retention schedule', d.retentionSchedule],
  ];
  for (const [label, s] of schedules) {
    if (!s) continue;
    const err = s.enabled || s.cron.trim() ? validateCron(s.cron) : null;
    if (err) return `The ${label} is invalid: ${err}`;
  }
  const s = d.settings;
  const r = d.retention;
  if (s.verify.mode === 'sample' && !whole(s.verify.samplePercent, 1, 100)) return 'The verify sample must be 1–100 %.';
  if (!whole(s.mtimeWindowSec, 0, 3600)) return 'The time window must be 0–3600 seconds.';
  if (!whole(s.maxChangePercent, 1, 100)) return 'The change limit must be 1–100 %.';
  if (!whole(s.maxChangeFiles, 1)) return 'The file change limit must be at least 1.';
  // The engine settings (internal/destinations engine_settings.go).
  if (s.transfers !== undefined && !whole(s.transfers, 1, MAX_TRANSFERS)) return `Transfers must be 1–${MAX_TRANSFERS}.`;
  if (s.verify.sampleMaxBytes !== undefined && !whole(s.verify.sampleMaxBytes, 1 << 20, 2 ** 50)) return 'The sample cap must be at least 1 GiB.';
  if (s.restic) {
    if (!whole(s.restic.packSizeMiB, 4, 128)) return 'The pack size must be 4–128 MiB.';
    if (!whole(s.restic.batchFiles, 1, 1_000_000)) return 'Batch files must be 1–1,000,000.';
    if (!whole(s.restic.batchBytes, 1 << 20, 2 ** 50)) return 'The batch size must be at least 1 GiB.';
    if (!whole(s.restic.pruneEveryDays, 1, 90)) return 'Prune every 1–90 days.';
    if (!PRUNE_MAX_UNUSED.test(s.restic.pruneMaxUnused)) return 'Prune max unused: a percentage (10%), a size (5G) or unlimited.';
  }
  if (s.rclone) {
    if (!whole(s.rclone.batchFiles, 1, 100_000)) return 'Batch files must be 1–100,000.';
    if (!whole(s.rclone.batchBytes, 1 << 20, 2 ** 50)) return 'The batch size must be at least 1 GiB.';
  }
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
  for (const f of SNAPSHOT_RETENTION) {
    const v = r[f.key];
    if (v !== undefined && !whole(v, 0, f.max)) return `${f.label}: 0–${f.max}.`;
  }
  return null;
}

/** snapshotRetention adds restic's snapshot fields (defaults) to r; other engines have none. */
function retentionFor(r: Retention, engine: EngineName): Retention {
  const out: Retention = { ...r };
  for (const f of SNAPSHOT_RETENTION) {
    if (engine === 'restic') out[f.key] = r[f.key] ?? f.def;
    else delete out[f.key];
  }
  return out;
}

/**
 * encryptionFor builds the request's encryption; forTest sends a "none" choice as accepted (a test
 * stores nothing). secret2 (crypt's password2) goes only with the user's own crypt password.
 */
function encryptionFor(engine: EngineName, choice: EncryptionChoice, secret: string, secret2: string, acknowledged: boolean, forTest: boolean): EncryptionInput | undefined {
  if (engine === 'filecopy') return undefined;
  if (engine === 'restic') return choice === 'own' ? { mode: 'restic', generate: false, secret } : { mode: 'restic' };
  if (choice === 'none') return { mode: 'none', acceptUnencrypted: forTest || acknowledged };
  if (choice !== 'own') return { mode: 'crypt' };
  return secret2 ? { mode: 'crypt', generate: false, secret, secret2 } : { mode: 'crypt', generate: false, secret };
}

/**
 * keptEncryption is the encryption of a create that finishes a pending destination (§4.5): its
 * mode and no secret, so the server keeps the one it sealed when the create began (the one in
 * that destination's recovery kit). A new password would not open the repository it initialized.
 */
function keptEncryption(pending: Destination, acknowledged: boolean, forTest: boolean): EncryptionInput {
  const mode = pending.encryption?.mode ?? 'restic';
  return mode === 'none' ? { mode: 'none', acceptUnencrypted: forTest || acknowledged } : { mode };
}

/** remoteState reads a stored destination's remote into the three location states. */
function remoteState(d: Destination | null): { sftp: SftpRemote; s3: S3Remote; b2: B2Remote } {
  const r = d?.remote ?? {};
  return {
    sftp: { ...EMPTY_SFTP, ...(d?.kind === 'sftp' ? r : {}), hostKeys: d?.kind === 'sftp' ? (r.hostKeys ?? []) : [] } as SftpRemote,
    s3: { ...EMPTY_S3, ...(d?.kind === 's3' ? r : {}) } as S3Remote,
    b2: { ...EMPTY_B2, ...(d?.kind === 'b2' ? r : {}) } as B2Remote,
  };
}

/**
 * DestinationForm adds or edits a destination (docs/design/phase4.md §15). Adding is a short
 * wizard in one dialog: Where (a local folder, SFTP, S3-compatible, B2), How (filecopy or restic
 * locally; restic or rclone off-site), the Connection, the Encryption, a Test (required before
 * Create), then the recovery kit step for an encrypted destination. The sources, schedules,
 * retention, bandwidth, verify and transfer sections follow. Every change of safety rule S29
 * asks for the user's password in this dialog. Editing keeps kind, engine, location and
 * encryption read-only.
 *
 * A filecopy target is tested as in Phase 1: the probe reports the marker, filesystem and
 * capabilities, and an existing marker or a local filesystem needs an explicit attach /
 * allowLocal (safety rule S3).
 *
 * A create at the location of a pending destination (a create that did not finish, §4.5)
 * finishes that one: the wizard sends its encryption mode without a secret (the server keeps the
 * sealed one) and does not ask to attach the repository it initialized. resume prefills the
 * wizard from such a destination (its storage credentials are asked again).
 */
export function DestinationForm({ destination, resume, onClose }: { destination: Destination | null; resume?: Destination | null; onClose: () => void }) {
  const qc = useQueryClient();
  const sources = useSources();
  const all = useDestinations();
  const editing = destination != null;
  // What the form starts from: the destination edited, or the pending one to finish.
  const init = destination ?? resume ?? null;
  const initialRemote = remoteState(init);
  const [kind, setKind] = useState<DestKind>(init?.kind ?? 'local');
  const [engine, setEngine] = useState<EngineName>(init?.engine ?? 'filecopy');
  const [name, setName] = useState(init?.name ?? '');
  const [target, setTarget] = useState(init && !isRemoteKind(init.kind) ? init.target : '');
  const [sftp, setSftp] = useState<SftpRemote>(initialRemote.sftp);
  const [s3, setS3] = useState<S3Remote>(initialRemote.s3);
  const [b2, setB2] = useState<B2Remote>(initialRemote.b2);
  const [creds, setCreds] = useState<CredentialsInput>({});
  const [encChoice, setEncChoice] = useState<EncryptionChoice>('generate');
  const [secret, setSecret] = useState('');
  const [secret2, setSecret2] = useState('');
  const [acknowledged, setAcknowledged] = useState(false);
  const [enabled, setEnabled] = useState(destination?.enabled ?? true);
  const [sourceIds, setSourceIds] = useState<number[]>(init?.sourceIds ?? []);
  const [schedule, setSchedule] = useState<CronSchedule>(init?.schedule ?? { cron: DEFAULT_SYNC_CRON, enabled: true });
  const [verifySchedule, setVerifySchedule] = useState<CronSchedule>(init?.verifySchedule ?? { cron: DEFAULT_VERIFY_CRON, enabled: true });
  const [retentionSchedule, setRetentionSchedule] = useState<CronSchedule>(init?.retentionSchedule ?? { cron: DEFAULT_RETENTION_CRON, enabled: true });
  const [settings, setSettings] = useState<DestinationSettings>({ ...DEFAULT_SETTINGS, ...init?.settings, verify: { ...DEFAULT_SETTINGS.verify, ...init?.settings?.verify } });
  const [retention, setRetention] = useState<Retention>({ ...DEFAULT_RETENTION, ...init?.retention });
  const [bandwidth, setBandwidth] = useState<Bandwidth>(bandwidthOf(init?.bandwidth));
  const [attach, setAttach] = useState(false);
  const [allowLocal, setAllowLocal] = useState(false);
  // A local restic repository on a local disk needs allowLocal: the server says so on Create.
  const [askAllowLocal, setAskAllowLocal] = useState(false);
  const [password, setPassword] = useState('');
  const [test, setTest] = useState<{ target: string; result: DestinationTestResult } | null>(null);
  const [engineTest, setEngineTest] = useState<{ key: string; result: EngineTestResult } | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  // Counts Save clicks, so a repeated form error is shown (scrolled into view) again.
  const [attempt, setAttempt] = useState(0);
  // The created destination whose recovery kit comes next (§15 step 6).
  const [created, setCreated] = useState<Destination | null>(null);
  // A saved destination whose create or update returned warnings (§4.5, SE-16): shown before closing.
  const [warned, setWarned] = useState<Destination | null>(null);

  const isEng = engine !== 'filecopy';
  const remoteKind = isRemoteKind(kind);
  const locations = { sftp, s3, b2 };
  const typed = typedCredentials(kind, creds);
  const engineSettings = engineSettingsDefaults(settings, engine, kind);
  const bandwidthProblems = validateBandwidth(bandwidth);
  // The pending destination this create finishes (same kind, engine and location), if any.
  const resuming = !editing && isEng ? pendingAt(all.data, kind, engine, target.trim(), remoteKind ? remoteFor(kind, locations) : undefined) : null;
  const plainOffsite = resuming ? resuming.encryption?.mode === 'none' : engine === 'rclone' && encChoice === 'none';

  // The connection a test speaks for: a changed field (or password) needs a new test.
  const testInput: EngineTestInput | null = isEng
    ? {
        kind,
        engine: engine as 'restic' | 'rclone',
        ...(kind === 'local' ? { target: target.trim() } : { remote: remoteFor(kind, locations) }),
        ...(typed ? { credentials: typed } : {}),
        encryption: resuming ? keptEncryption(resuming, acknowledged, true) : encryptionFor(engine, encChoice, secret, secret2, acknowledged, true),
      }
    : null;
  const testKey = JSON.stringify(testInput);
  const currentEngineTest = !editing && engineTest && engineTest.key === testKey ? engineTest.result : null;
  // Editing tests the stored destination with its stored secrets (never the form's fields).
  const storedEngineTest = editing && engineTest ? engineTest.result : null;
  const current = !isEng && test && test.target === target.trim() ? test.result : null;
  const caps = current?.capabilities ?? destination?.capabilities ?? null;
  const set = (patch: Partial<DestinationSettings>) => setSettings({ ...settings, ...patch });

  // S29: which changes need the password now.
  const needsPassword = editing
    ? updateNeedsPassword(destination, {
        credentials: typed,
        hostKeys: destination.kind === 'sftp' ? sftp.hostKeys : undefined,
        caCert: destination.kind === 's3' ? s3.caCert.trim() : undefined,
        sourceIds,
      })
    : createNeedsPassword(kind, plainOffsite);

  const tester = useMutation({
    mutationFn: async (v: { key: string; filecopy: string | null; input: EngineTestInput | null }) => {
      if (editing) {
        return destination.engine === 'filecopy' ? { filecopy: await testDestination(destination.id) } : { engine: await testStoredDestination(destination.id) };
      }
      return v.input ? { engine: await testConnection(v.input) } : { filecopy: await testTarget(v.filecopy ?? '') };
    },
    onSuccess: (r, v) => {
      if (r.filecopy) setTest({ target: v.filecopy ?? target.trim(), result: r.filecopy });
      if (r.engine) setEngineTest({ key: v.key, result: r.engine });
      setAttach(false);
      setAllowLocal(false);
      // "Test the target first" and similar problems are answered by a new test.
      setFormError(null);
    },
  });
  const saver = useMutation({
    mutationFn: (body: DestinationInput) => (editing ? updateDestination(destination.id, body) : createDestination(body)),
    onSuccess: async (saved) => {
      setPassword('');
      await qc.invalidateQueries({ queryKey: keys.destinations });
      await qc.invalidateQueries({ queryKey: keys.schedules });
      if (!editing && saved && kitState(saved) === 'unconfirmed') {
        setCreated(saved);
        return;
      }
      // The safety warnings of a create or update (another Bunkarr may write to an attached
      // repository, a non-empty remote, an unrestricted B2 key, an http endpoint) are delivered
      // only here: keep the dialog open until they are read.
      if (saved?.warnings?.length) {
        setWarned(saved);
        return;
      }
      onClose();
    },
    onError: (e) => {
      if (!editing && kind === 'local' && engine === 'restic' && /allowLocal/.test(String((e as Error)?.message))) {
        setAskAllowLocal(true);
      }
    },
  });

  function runTest() {
    setFormError(null);
    if (!editing && isEng) {
      const problem = kind === 'local' ? (target.trim() ? null : 'Choose the repository folder.') : (locationProblem(kind, locations) ?? credentialProblem(kind, creds));
      if (problem) {
        setFormError(problem);
        setAttempt((n) => n + 1);
        return;
      }
    }
    tester.mutate({ key: testKey, filecopy: isEng ? null : target.trim(), input: testInput });
  }

  /** engineCreateProblem checks the connection, encryption and the test of a new restic or rclone destination. */
  function engineCreateProblem(): string | null {
    if (kind !== 'local') {
      const p = locationProblem(kind, locations) ?? credentialProblem(kind, creds);
      if (p) return p;
    }
    if (resuming) {
      // The test used a new password, so a repository initialized with the kept one reads as
      // "wrong password" (or exists): expected. Only the connection must work.
      if (plainOffsite && !acknowledged) return 'Confirm that the storage provider can read every file.';
      const t = currentEngineTest;
      if (!t) return 'Test the connection first: Bunkarr checks the location and the credentials before finishing the create.';
      if (t.hostKeys?.length) return 'Pin the server’s host keys first (Fetch host keys), then test again.';
      if (!t.reachable) return `The connection cannot be used: ${t.message || 'see the test result'}. Fix it, then test again.`;
      return null;
    }
    if (encChoice === 'own') {
      const p = secretProblem(secret) ?? (engine === 'rclone' && secret2 ? secretProblem(secret2, 'The crypt password2') : null);
      if (p) return p;
    }
    if (engine === 'rclone' && encChoice === 'none' && !acknowledged) {
      return 'Confirm that the storage provider can read every file, or choose encryption.';
    }
    const t = currentEngineTest;
    if (!t) return 'Test the connection first: Bunkarr checks the location, the credentials and what is there before creating the destination.';
    if (t.hostKeys?.length) return 'Pin the server’s host keys first (Fetch host keys), then test again.';
    if (engine === 'restic') {
      if (t.repository === 'wrong-password') {
        return encChoice === 'own'
          ? 'The password does not open the repository at this location.'
          : 'A restic repository already exists here. Choose “Use my own / existing repository” and type its password to attach it, or choose another location.';
      }
      if (engineNeedsAttach(t, engine)) {
        if (encChoice !== 'own') {
          return 'A restic repository already exists here. Choose “Use my own / existing repository” and type its password to attach it, or choose another location.';
        }
        if (!attach) return 'Confirm that you want to attach the existing repository.';
        return null;
      }
    } else {
      if (t.marker === 'unreadable') {
        return 'The remote holds a marker that cannot be read: a wrong crypt password or password2 (a crypt remote whose recovery kit lists a password2 needs both), another crypt remote, or a file that is not a Bunkarr marker.';
      }
      if (t.marker === 'foreign') return 'The remote belongs to another destination of this Bunkarr.';
      if (engineNeedsAttach(t, engine)) {
        if (!attach) return 'Confirm that you want to attach the existing Bunkarr destination on this remote.';
        return null;
      }
    }
    if (!t.ok) return `The connection cannot be used: ${t.message || 'see the test result'}. Fix it, then test again.`;
    return null;
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    setAttempt((n) => n + 1);
    const body: DestinationInput = {
      name: name.trim(),
      engine,
      target: editing ? destination.target : kind === 'local' ? target.trim() : '',
      enabled,
      sourceIds,
      schedule: { cron: schedule.cron.trim(), enabled: schedule.enabled },
      verifySchedule: { cron: verifySchedule.cron.trim(), enabled: verifySchedule.enabled },
      settings: engineSettings,
      retention: retentionFor(retention, engine),
    };
    if (isEng) {
      body.retentionSchedule = { cron: retentionSchedule.cron.trim(), enabled: retentionSchedule.enabled };
    }
    const problem = validateDestination({ ...body, kind });
    if (problem) {
      setFormError(problem);
      return;
    }
    if (bandwidthProblems.first) {
      setFormError(`Bandwidth: ${bandwidthProblems.first}`);
      return;
    }
    body.kind = kind;
    body.bandwidth = normalizeBandwidth(bandwidth);
    if (editing) {
      if (typed) body.credentials = typed;
      if (destination.kind === 'sftp' || destination.kind === 's3') {
        const remote = remoteFor(destination.kind, locations);
        const changed =
          destination.kind === 'sftp' ? !sameHostKeys(sftp.hostKeys, destination.remote.hostKeys) : s3.caCert.trim() !== (destination.remote.caCert ?? '').trim();
        if (changed && remote) body.remote = remote;
      }
      if (destination.kind === 'sftp' && sftp.hostKeys.length === 0) {
        setFormError('Pin the server’s host keys: Bunkarr connects only to a server with a pinned key.');
        return;
      }
    } else if (isEng) {
      const p = engineCreateProblem();
      if (p) {
        setFormError(p);
        return;
      }
      if (remoteKind) body.remote = remoteFor(kind, locations);
      if (typed) body.credentials = typed;
      body.encryption = resuming ? keptEncryption(resuming, acknowledged, false) : encryptionFor(engine, encChoice, secret, secret2, acknowledged, false);
      if (attach && !resuming) body.attach = true;
      if (allowLocal) body.allowLocal = true;
    } else {
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
    if (needsPassword) {
      if (!password) {
        setFormError('Enter your Bunkarr password to confirm (at the end of the form).');
        return;
      }
      body.currentPassword = password;
    }
    saver.mutate(body);
  }

  function chooseKind(k: DestKind) {
    setKind(k);
    setEngine(k === 'local' ? 'filecopy' : 'restic');
    setEncChoice('generate');
    setAcknowledged(false);
    setCreds({});
    setAttach(false);
  }

  function chooseEngine(e: EngineName) {
    setEngine(e);
    setEncChoice('generate');
    setAcknowledged(false);
    setAttach(false);
  }

  const toggleSource = (id: number, on: boolean) => setSourceIds(on ? [...sourceIds, id] : sourceIds.filter((x) => x !== id));
  const saveError = saver.error && !isPasswordError(saver.error) ? saver.error : null;

  if (created) {
    return (
      <Modal
        title={`Recovery kit · ${created.name}`}
        size="lg"
        onClose={onClose}
        footer={
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        }
      >
        <Notice tone="success">{created.name} was created.</Notice>
        <WarningList warnings={created.warnings} />
        {/* A crypt password typed with a password2 is confirmed by the kit's check code only. */}
        <RecoveryKitPanel destination={created} retype={!(engine === 'rclone' && encChoice === 'own' && secret2)} />
      </Modal>
    );
  }

  if (warned) {
    return (
      <Modal
        title={`${editing ? 'Saved' : 'Created'} · ${warned.name}`}
        size="lg"
        onClose={onClose}
        footer={
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        }
      >
        <Notice tone="success">
          {warned.name} was {editing ? 'saved' : 'created'}.
        </Notice>
        <WarningList warnings={warned.warnings} />
      </Modal>
    );
  }

  const passwordReason = editing
    ? 'Changing where data goes (credentials, host keys, the CA certificate, or a new source for an off-site destination) needs your password.'
    : plainOffsite && remoteKind
      ? 'This sends data off this server without encryption, so it needs your password.'
      : 'This sends your data to a place off this server, so it needs your password. The API key cannot do this.';

  return (
    <Modal
      title={editing ? `Edit destination · ${destination.name}` : resume ? `Finish creating · ${resume.name}` : 'Add destination'}
      size="xl"
      onClose={onClose}
      footer={
        <>
          <Button icon={FlaskConical} busy={tester.isPending} disabled={!editing && !isEng && !target.trim()} onClick={runTest}>
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
        <ErrorNotice error={saveError ?? tester.error} />

        {editing ? (
          <FormSection title="Destination">
            <TextField label="Name" value={name} onChange={setName} />
            <FormRow label="Kind">
              <div className="flex flex-wrap items-center gap-2 pt-2 text-sm">
                <span>{KIND_LABELS[destination.kind] ?? destination.kind}</span>
                <Badge>{ENGINE_BADGES[destination.engine] ?? destination.engine}</Badge>
                {destination.encryption?.mode && destination.encryption.mode !== 'none' ? <Badge tone="ok">Encrypted</Badge> : <Badge tone="warn">Not encrypted</Badge>}
              </div>
              <p className="mt-1 text-xs text-ink-muted">Kind, engine, location and encryption cannot change: create a new destination instead.</p>
            </FormRow>
          </FormSection>
        ) : (
          <>
            <FormSection title="1 · Where">
              <TextField label="Name" value={name} onChange={setName} autoFocus placeholder={kind === 'local' ? 'UNAS' : 'Off-site'} />
              <WhereField kind={kind} onChange={chooseKind} />
            </FormSection>
            <FormSection title="2 · How">
              <HowField kind={kind} engine={engine} onChange={chooseEngine} />
            </FormSection>
          </>
        )}

        <FormSection title={editing ? 'Connection' : '3 · Connection'}>
          {kind === 'local' ? (
            <PathField
              label="Target"
              value={target}
              onChange={setTarget}
              disabled={editing}
              placeholder="/backup"
              help={
                editing
                  ? 'The target cannot change: the destination is tied to the marker and filesystem recorded when it was created.'
                  : engine === 'restic'
                    ? 'The folder of the restic repository as Bunkarr sees it (a mounted share or a disk). It must exist; an empty folder gets a new repository.'
                    : 'The mounted NAS share as Bunkarr sees it (for example /backup). It must already exist; Bunkarr never creates it, and refuses an empty mount point on the local disk.'
              }
            />
          ) : kind === 'sftp' ? (
            <>
              <SftpFields value={sftp} onChange={setSftp} fixed={editing} />
              <HostKeysField host={sftp.host} port={sftp.port} pinned={sftp.hostKeys} onPin={(hostKeys) => setSftp({ ...sftp, hostKeys })} />
            </>
          ) : kind === 's3' ? (
            <S3Fields value={s3} onChange={setS3} fixed={editing} />
          ) : (
            <B2Fields value={b2} onChange={setB2} fixed={editing} />
          )}
          {kind !== 'local' && <CredentialFields kind={kind} values={creds} onChange={setCreds} stored={editing ? destination.hasCredentials : null} />}
          {editing && isEng && typed && (
            <p className="-mt-2 mb-4 text-xs text-ink-muted sm:ml-[12rem]">The new credentials are tested against the stored location before they are saved.</p>
          )}
          {!editing && !isEng && !current && (
            <p className="-mt-2 mb-4 text-xs text-ink-muted sm:ml-[12rem]">Test the target before saving: the probe checks the marker, the filesystem and what it can store.</p>
          )}
          {current && <TestResultView result={current} creating={!editing} />}
          {storedEngineTest && isEng && <EngineTestView result={storedEngineTest} engine={engine as 'restic' | 'rclone'} />}
          {!editing && !isEng && current && needsAttach(current) && (
            <div className="mb-4 rounded border border-warn/50 bg-warn/10 p-3 text-sm">
              <Checkbox
                label="Attach to the existing Bunkarr destination in this folder"
                help="The folder holds .bunkarr/destination.json from an earlier setup. Attaching adopts that destination's id and continues its backup there; files already present are adopted or kept in retention, never overwritten."
                checked={attach}
                onChange={setAttach}
              />
            </div>
          )}
          {!editing && !isEng && current?.local && (
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

        {!editing && isEng && (
          <FormSection
            title="4 · Encryption"
            description={
              engine === 'restic'
                ? 'A restic repository is always encrypted. The password is needed to read the backup: Bunkarr keeps it sealed, and you keep it in the recovery kit.'
                : 'Encrypted by default: the provider sees sizes and times, never names or contents.'
            }
          >
            {resuming ? (
              <>
                <Notice tone="warning" title={`Finishes the create of ${resuming.name}`}>
                  Creating {resuming.name} at this location did not finish. Save finishes it with the encryption{' '}
                  {resuming.encryption?.mode === 'none' ? 'choice' : 'password'} Bunkarr kept for it (the one in its recovery kit); a new one would not open what
                  that create made. Delete {resuming.name} on the Destinations page to start over instead.
                </Notice>
                {plainOffsite && (
                  <Checkbox
                    label="I understand the storage provider can read every file"
                    help="This destination stores plain files off this server."
                    checked={acknowledged}
                    onChange={setAcknowledged}
                  />
                )}
              </>
            ) : (
              <EncryptionFields
                engine={engine as 'restic' | 'rclone'}
                choice={encChoice}
                onChoice={(c) => {
                  setEncChoice(c);
                  setAttach(false);
                }}
                secret={secret}
                onSecret={setSecret}
                secret2={secret2}
                onSecret2={setSecret2}
                acknowledged={acknowledged}
                onAcknowledge={setAcknowledged}
              />
            )}
          </FormSection>
        )}

        {!editing && isEng && (
          <FormSection title="5 · Test" description="Required before Create: Bunkarr checks the location, the credentials and what is already there. Nothing is written.">
            {currentEngineTest ? (
              <>
                <EngineTestView result={currentEngineTest} engine={engine as 'restic' | 'rclone'} creating />
                {resuming && currentEngineTest.reachable && !currentEngineTest.hostKeys?.length && (
                  <p className="-mt-2 mb-4 text-xs text-ink-muted">
                    The test opens the location with a new password, so what the earlier create made reads as locked to it: expected. Save uses the kept one.
                  </p>
                )}
              </>
            ) : (
              <p className="mb-4 text-sm text-ink-muted">Not tested yet: use Test below. A change to the connection or the encryption needs a new test.</p>
            )}
            {!resuming && currentEngineTest && engineNeedsAttach(currentEngineTest, engine) && (engine === 'rclone' || encChoice === 'own') && (
              <div className="mb-4 rounded border border-warn/50 bg-warn/10 p-3 text-sm">
                <Checkbox
                  label={engine === 'restic' ? 'Attach the existing repository' : 'Attach the existing Bunkarr destination on this remote'}
                  help={
                    engine === 'restic'
                      ? 'Bunkarr backs up into it with a new tag of its own; snapshots already there are never forgotten by this destination.'
                      : 'Its id is adopted; objects already there that match are kept, the others move into retention, never overwritten.'
                  }
                  checked={attach}
                  onChange={setAttach}
                />
              </div>
            )}
            {askAllowLocal && (
              <div className="mb-4 rounded border border-warn/50 bg-warn/10 p-3 text-sm">
                <Checkbox
                  label="Yes, keep the repository on this local disk"
                  help="The folder is on the same disk as the container or Bunkarr's config: a backup there does not survive a disk failure."
                  checked={allowLocal}
                  onChange={setAllowLocal}
                />
              </div>
            )}
          </FormSection>
        )}

        <FormSection
          title="Sources"
          description={
            remoteKind
              ? 'The sources backed up to this destination. Adding a source to an off-site destination needs your password.'
              : 'The sources mirrored to this destination, each under its destination folder.'
          }
        >
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
            help={
              isEng
                ? 'Checks that every backed-up file is in the backup, and reads a sample back to compare hashes.'
                : 'Checks that every backed-up file is still there with the right size, and re-reads a sample to compare hashes.'
            }
          />
          {isEng && (
            <CronInput
              label="Retention"
              value={retentionSchedule}
              onChange={setRetentionSchedule}
              presets={RETENTION_PRESETS}
              help={engine === 'restic' ? 'Forgets the snapshots retention no longer keeps, and prunes the repository on its schedule (below).' : 'Removes retained versions whose time is over.'}
            />
          )}
        </FormSection>

        <FormSection title="Verify">
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
          {isEng && <SampleCapField engine={engine as 'restic' | 'rclone'} settings={engineSettings} onChange={setSettings} />}
        </FormSection>

        {!isEng && (
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
        )}

        {isEng && (
          <FormSection title="Transfers" description="How the engine moves data. The defaults suit most connections.">
            <EngineTransferFields engine={engine as 'restic' | 'rclone'} settings={engineSettings} onChange={setSettings} />
          </FormSection>
        )}

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

        <FormSection
          title="Retention"
          description={
            engine === 'restic'
              ? 'Deleted and replaced files keep their last version in a snapshot for this long; the snapshot periods below keep extra history.'
              : "Files deleted or replaced at the source are kept in the destination's retention folder, then removed."
          }
        >
          <NumberField
            label="Keep deleted files"
            value={retention.deletedDays}
            onChange={(deletedDays) => setRetention({ ...retention, deletedDays })}
            min={1}
            max={3650}
            suffix="days"
          />
          {engine === 'restic' && <SnapshotRetentionFields retention={retentionFor(retention, 'restic')} onChange={setRetention} />}
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

        <FormSection title="Bandwidth" description="Limits and the transfer window apply to syncs, verifies and retention of this destination.">
          <BandwidthEditor value={bandwidth} onChange={setBandwidth} problems={bandwidthProblems} />
        </FormSection>

        {editing && !isEng && (
          <FormRow label="Engine">
            <p className="pt-2 text-sm text-ink-muted">filecopy (mounted share)</p>
          </FormRow>
        )}

        {needsPassword && <PasswordConfirm value={password} onChange={setPassword} error={saver.error} reason={passwordReason} />}
      </form>
    </Modal>
  );
}
