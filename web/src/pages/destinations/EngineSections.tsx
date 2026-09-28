import { useId, type ReactNode } from 'react';
import type { DestinationSettings, DestKind, EngineName, Retention } from '@/api/types';
import { Checkbox, FormRow, NumberField, TextField, inputClass } from '@/components/Form';
import { Badge } from '@/components/StatusBadge';
import { GiB, KIND_HELP, KIND_LABELS, KINDS, MAX_TRANSFERS, SNAPSHOT_RETENTION, engineChoices } from '@/lib/destinationKinds';

// Sections of the add wizard and of Edit that only restic and rclone destinations have
// (docs/design/phase4.md §15 steps 1, 2, 4 and 7): where the backup goes, how it is stored, its
// encryption, the snapshot retention and the transfer settings.

/** EncryptionChoice is the Encryption step's choice: a generated secret, the user's own, or none (rclone only). */
export type EncryptionChoice = 'generate' | 'own' | 'none';

function RadioCard({
  name,
  value,
  checked,
  onChange,
  label,
  help,
  badge,
  disabled,
}: {
  name: string;
  value: string;
  checked: boolean;
  onChange: (v: string) => void;
  label: string;
  help?: ReactNode;
  badge?: ReactNode;
  disabled?: boolean;
}) {
  return (
    <label
      className={`flex min-w-0 cursor-pointer items-start gap-2 rounded border p-2 text-sm ${checked ? 'border-accent/60 bg-accent/10' : 'border-line'} ${disabled ? 'opacity-60' : ''}`}
    >
      <input type="radio" className="mt-1 accent-[var(--color-accent)]" name={name} value={value} checked={checked} disabled={disabled} onChange={() => onChange(value)} />
      <span className="min-w-0">
        <span className="font-medium">{label}</span> {badge}
        {help && <span className="block text-xs text-ink-muted">{help}</span>}
      </span>
    </label>
  );
}

/** WhereField is step 1: the kind of destination. */
export function WhereField({ kind, onChange }: { kind: DestKind; onChange: (k: DestKind) => void }) {
  const name = useId();
  return (
    <FormRow label="Where" group>
      <div className="grid gap-2 sm:grid-cols-2">
        {KINDS.map((k) => (
          <RadioCard key={k} name={name} value={k} checked={kind === k} onChange={(v) => onChange(v as DestKind)} label={KIND_LABELS[k]} help={KIND_HELP[k]} />
        ))}
      </div>
    </FormRow>
  );
}

/** HowField is step 2: the engine, with one line on what it means for restores and costs. */
export function HowField({ kind, engine, onChange }: { kind: DestKind; engine: EngineName; onChange: (e: EngineName) => void }) {
  const name = useId();
  const choices = engineChoices(kind);
  return (
    <FormRow label="How" group>
      <div className="grid gap-2">
        {choices.map((c, i) => (
          <RadioCard
            key={c.value}
            name={name}
            value={c.value}
            checked={engine === c.value}
            onChange={(v) => onChange(v as EngineName)}
            label={c.label}
            help={c.help}
            badge={i === 0 ? <Badge tone="info">Default</Badge> : undefined}
          />
        ))}
      </div>
    </FormRow>
  );
}

/**
 * EncryptionFields is step 4. restic always encrypts: a generated password (recommended) or the
 * user's own, which also attaches an existing repository. rclone: crypt (recommended, generated
 * or the user's own crypt password, with the optional password2 a crypt remote's kit may list)
 * or no encryption, which needs the acknowledgement that the provider can read every file (and
 * the user's password, S29).
 */
export function EncryptionFields({
  engine,
  choice,
  onChoice,
  secret,
  onSecret,
  secret2,
  onSecret2,
  acknowledged,
  onAcknowledge,
}: {
  engine: 'restic' | 'rclone';
  choice: EncryptionChoice;
  onChoice: (c: EncryptionChoice) => void;
  secret: string;
  onSecret: (s: string) => void;
  secret2: string;
  onSecret2: (s: string) => void;
  acknowledged: boolean;
  onAcknowledge: (v: boolean) => void;
}) {
  const name = useId();
  const sub = useId();
  const secretId = useId();
  const secret2Id = useId();
  const own = (
    <div className="mt-2 grid gap-1 sm:grid-cols-[11rem_1fr] sm:gap-4">
      <label htmlFor={secretId} className="pt-2 text-sm font-medium">
        Encryption password
      </label>
      <div className="min-w-0">
        <input
          id={secretId}
          type="password"
          className={`${inputClass} max-w-md font-mono`}
          value={secret}
          autoComplete="new-password"
          spellCheck={false}
          onChange={(e) => onSecret(e.target.value)}
        />
        <div className="mt-1 text-xs text-ink-muted">
          At least 16 characters, no spaces at either end. It can never be changed or shown again except in the recovery kit; you type it once more after
          creating to prove you have it{engine === 'rclone' ? ' (with a password2, the kit’s check code instead)' : ''}. To attach an existing{' '}
          {engine === 'restic' ? 'repository, type its password' : 'crypt remote, type its password (the kit’s “rclone crypt password”)'}.
        </div>
      </div>
      {engine === 'rclone' && (
        <>
          <label htmlFor={secret2Id} className="pt-2 text-sm font-medium">
            Crypt password2 (salt)
          </label>
          <div className="min-w-0">
            <input
              id={secret2Id}
              type="password"
              className={`${inputClass} max-w-md font-mono`}
              value={secret2}
              autoComplete="new-password"
              spellCheck={false}
              onChange={(e) => onSecret2(e.target.value)}
            />
            <div className="mt-1 text-xs text-ink-muted">
              Optional; the recovery kit’s “rclone crypt password2”. A crypt remote whose kit lists a password2 (every one whose passwords Bunkarr generated)
              needs both to attach. Leave it empty when the kit says none, or for a crypt remote made with a password only (rclone’s default salt). With a
              password2, confirm the recovery kit with its check code.
            </div>
          </div>
        </>
      )}
    </div>
  );
  if (engine === 'restic') {
    return (
      <FormRow label="Encryption" group>
        <div className="grid gap-2">
          <RadioCard
            name={name}
            value="generate"
            checked={choice !== 'own'}
            onChange={() => onChoice('generate')}
            label="Generate a password (recommended)"
            help="Bunkarr makes a strong random password. You download it in the recovery kit next."
          />
          <RadioCard
            name={name}
            value="own"
            checked={choice === 'own'}
            onChange={() => onChoice('own')}
            label="Use my own / existing repository"
            help="Type the password yourself, for example to attach a repository you already have."
          />
        </div>
        {choice === 'own' && own}
      </FormRow>
    );
  }
  return (
    <FormRow label="Encryption" group>
      <div className="grid gap-2">
        <RadioCard
          name={name}
          value="crypt"
          checked={choice !== 'none'}
          onChange={() => onChoice('generate')}
          label="Encrypt with rclone crypt (recommended)"
          help="File contents and names are encrypted before they leave this server; sizes and times stay visible to the provider."
        />
        {choice !== 'none' && (
          <div className="grid gap-2 sm:ml-6">
            <RadioCard name={sub} value="generate" checked={choice === 'generate'} onChange={() => onChoice('generate')} label="Generate the crypt passwords" />
            <RadioCard name={sub} value="own" checked={choice === 'own'} onChange={() => onChoice('own')} label="Use my own / an existing crypt password" />
          </div>
        )}
        <RadioCard
          name={name}
          value="none"
          checked={choice === 'none'}
          onChange={() => onChoice('none')}
          label="Do not encrypt"
          help="Plain files at the provider. Plex database and *arr backups (which hold their credentials) are then refused on this destination."
        />
      </div>
      {choice === 'own' && own}
      {choice === 'none' && (
        <div className="mt-2 rounded border border-danger/50 bg-danger/10 p-2 text-sm">
          <Checkbox
            label="I understand that the storage provider can read every file"
            help="Anyone with access to the bucket or server (the provider, a leaked key) can read your media and the file names. Needs your password."
            checked={acknowledged}
            onChange={onAcknowledge}
          />
        </div>
      )}
    </FormRow>
  );
}

/** SnapshotRetentionFields are restic's snapshot retention fields with the server's ranges (§6.5). */
export function SnapshotRetentionFields({ retention, onChange }: { retention: Retention; onChange: (r: Retention) => void }) {
  return (
    <>
      {SNAPSHOT_RETENTION.map((f) => (
        <NumberField
          key={f.key}
          label={f.label}
          value={retention[f.key] ?? f.def}
          onChange={(v) => onChange({ ...retention, [f.key]: v })}
          min={0}
          max={f.max}
          suffix={f.suffix}
        />
      ))}
      <p className="-mt-2 mb-4 text-xs text-ink-muted sm:ml-[12rem]">
        0–{SNAPSHOT_RETENTION[0].max} days, 0–{SNAPSHOT_RETENTION[1].max} weeks, 0–{SNAPSHOT_RETENTION[2].max} months, 0–{SNAPSHOT_RETENTION[3].max} years. On
        top of these, a snapshot is kept while it holds the last version of a deleted or replaced file (for &quot;Keep deleted files&quot;), and the newest
        snapshot of each source is never forgotten.
      </p>
    </>
  );
}

/** gib reads a byte count as whole GiB for a number field. */
const gib = (bytes: number | undefined, def: number) => Math.round((bytes ?? def) / GiB);

/** EngineTransferFields are the transfer settings of restic and rclone destinations (§9.3, §6.2, §7.3). */
export function EngineTransferFields({
  engine,
  settings,
  onChange,
}: {
  engine: 'restic' | 'rclone';
  settings: DestinationSettings;
  onChange: (s: DestinationSettings) => void;
}) {
  const r = settings.restic;
  const c = settings.rclone;
  return (
    <>
      <NumberField
        label="Transfers"
        value={settings.transfers ?? 4}
        onChange={(transfers) => onChange({ ...settings, transfers })}
        min={1}
        max={MAX_TRANSFERS}
        suffix="at once"
        help={engine === 'rclone' ? 'rclone --transfers (and twice as many checkers).' : 'Connections of restic to a remote repository.'}
      />
      {engine === 'restic' && r && (
        <>
          <NumberField label="Pack size" value={r.packSizeMiB} onChange={(packSizeMiB) => onChange({ ...settings, restic: { ...r, packSizeMiB } })} min={4} max={128} suffix="MiB (4–128)" />
          <NumberField
            label="Batch files"
            value={r.batchFiles}
            onChange={(batchFiles) => onChange({ ...settings, restic: { ...r, batchFiles } })}
            min={1}
            max={1_000_000}
            suffix="files per snapshot at most"
          />
          <NumberField
            label="Batch size"
            value={gib(r.batchBytes, 64 * GiB)}
            onChange={(v) => onChange({ ...settings, restic: { ...r, batchBytes: Math.round(v * GiB) } })}
            min={1}
            suffix="GiB per snapshot at most"
            help="A long first backup is cut into batches, each ending in a snapshot, so an interruption never loses more than one batch."
          />
          <NumberField
            label="Prune every"
            value={r.pruneEveryDays}
            onChange={(pruneEveryDays) => onChange({ ...settings, restic: { ...r, pruneEveryDays } })}
            min={1}
            max={90}
            suffix="days (1–90)"
          />
          <TextField
            label="Prune max unused"
            value={r.pruneMaxUnused}
            onChange={(pruneMaxUnused) => onChange({ ...settings, restic: { ...r, pruneMaxUnused } })}
            mono
            help='How much unused data prune may leave to save downloads: a percentage ("10%"), a size ("5G") or "unlimited".'
          />
        </>
      )}
      {engine === 'rclone' && c && (
        <>
          <NumberField
            label="Batch files"
            value={c.batchFiles}
            onChange={(batchFiles) => onChange({ ...settings, rclone: { ...c, batchFiles } })}
            min={1}
            max={100_000}
            suffix="files per batch at most"
          />
          <NumberField
            label="Batch size"
            value={gib(c.batchBytes, 64 * GiB)}
            onChange={(v) => onChange({ ...settings, rclone: { ...c, batchBytes: Math.round(v * GiB) } })}
            min={1}
            suffix="GiB per batch at most"
          />
        </>
      )}
    </>
  );
}

/** SampleCapField is the byte cap of one sample verify of a restic or rclone destination. */
export function SampleCapField({ settings, onChange, engine }: { settings: DestinationSettings; onChange: (s: DestinationSettings) => void; engine: 'restic' | 'rclone' }) {
  const def = engine === 'restic' ? 4 * GiB : 16 * GiB;
  return (
    <NumberField
      label="Sample cap"
      value={gib(settings.verify.sampleMaxBytes, def)}
      onChange={(v) => onChange({ ...settings, verify: { ...settings.verify, sampleMaxBytes: Math.round(v * GiB) } })}
      min={1}
      suffix="GiB read back per verify at most"
      help={engine === 'restic' ? 'A sample verify restores this much at most and compares hashes; restic also checks the repository.' : 'A sample verify downloads this much at most and compares hashes.'}
    />
  );
}
