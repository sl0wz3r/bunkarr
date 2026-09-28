// Destination kinds, engines and their encryption (docs/design/phase4.md §4, §5, §15): labels,
// the choices the add wizard offers, provider presets, the credential fields of each kind, when a
// change needs the user's password (S29), and the recovery kit custody.

import type {
  B2Remote,
  CredentialField,
  CredentialsInput,
  Destination,
  DestinationSettings,
  DestKind,
  EngineName,
  EngineState,
  HostKey,
  Retention,
  S3Provider,
  S3Remote,
  SftpRemote,
} from '@/api/types';
import type { CronPreset } from './cron';

export const KINDS: DestKind[] = ['local', 'sftp', 's3', 'b2'];

/** KIND_LABELS are the wizard's "Where" choices. */
export const KIND_LABELS: Record<DestKind, string> = {
  local: 'Local or mounted folder',
  sftp: 'SFTP server',
  s3: 'S3-compatible storage',
  b2: 'Backblaze B2',
};

export const KIND_HELP: Record<DestKind, string> = {
  local: 'A share mounted into the container (for example a NAS over NFS or SMB), or a disk.',
  sftp: 'A server you reach over SSH: another NAS, a VPS, a friend’s machine.',
  s3: 'Amazon S3, Wasabi, Cloudflare R2, MinIO or another S3 service.',
  b2: 'A Backblaze B2 bucket, through its native API.',
};

/** KIND_BADGES are short names for badges. */
export const KIND_BADGES: Record<DestKind, string> = { local: 'Local', sftp: 'SFTP', s3: 'S3', b2: 'B2' };

export const ENGINE_BADGES: Record<EngineName, string> = { filecopy: 'filecopy', restic: 'restic', rclone: 'rclone' };

/** isRemoteKind reports whether data of a destination of kind leaves this machine. */
export function isRemoteKind(kind: DestKind | '' | undefined): boolean {
  return !!kind && kind !== 'local';
}

/** isEngine reports whether a destination runs restic or rclone. */
export function isEngine(d: Pick<Destination, 'engine'>): boolean {
  return d.engine === 'restic' || d.engine === 'rclone';
}

export interface EngineChoice {
  value: EngineName;
  label: string;
  /** One line on what the choice means for restores and costs. */
  help: string;
}

/** engineChoices are the wizard's "How" choices for a kind; the first is the default. */
export function engineChoices(kind: DestKind): EngineChoice[] {
  if (kind === 'local') {
    return [
      {
        value: 'filecopy',
        label: 'Plain files (filecopy)',
        help: 'A browsable mirror: restore by copying files back. Deleted and replaced files are kept in a retention folder. Not encrypted.',
      },
      {
        value: 'restic',
        label: 'restic repository',
        help: 'Encrypted, deduplicated snapshots: restore with restic (or Bunkarr later). Needs less space for many versions; files are not browsable.',
      },
    ];
  }
  return [
    {
      value: 'restic',
      label: 'restic (snapshots, deduplicated)',
      help: 'Always encrypted; versions share their data, so storage and uploads stay small. Restores need restic and the recovery kit.',
    },
    {
      value: 'rclone',
      label: 'rclone (plain copy of the files)',
      help: 'One object per file (encrypted by rclone crypt unless you opt out): restore single files with rclone; every version is stored whole, which costs more space.',
    },
  ];
}

export interface ProviderPreset {
  provider: S3Provider;
  label: string;
  /** The endpoint the preset fills in; {region} and {account} are placeholders to replace. */
  endpoint: string;
  region: string;
  forcePathStyle: boolean;
  /** Storage classes the server accepts ('' = the provider's default). */
  storageClasses: string[];
  help: string;
}

/** PROVIDER_PRESETS fill the endpoint pattern of each S3 provider (internal/destinations remote.go). */
export const PROVIDER_PRESETS: ProviderPreset[] = [
  {
    provider: 'AWS',
    label: 'Amazon S3',
    endpoint: '',
    region: 'us-east-1',
    forcePathStyle: false,
    storageClasses: ['', 'STANDARD', 'STANDARD_IA', 'ONEZONE_IA', 'INTELLIGENT_TIERING', 'GLACIER_IR', 'REDUCED_REDUNDANCY'],
    help: 'Leave the endpoint empty: the region’s endpoint is used.',
  },
  {
    provider: 'Wasabi',
    label: 'Wasabi',
    endpoint: 'https://s3.{region}.wasabisys.com',
    region: 'us-east-1',
    forcePathStyle: false,
    storageClasses: ['', 'STANDARD'],
    help: 'Wasabi bills 90 days minimum per object: short retention costs the same as 90 days.',
  },
  {
    provider: 'Cloudflare',
    label: 'Cloudflare R2',
    endpoint: 'https://{account}.r2.cloudflarestorage.com',
    region: 'auto',
    forcePathStyle: false,
    storageClasses: ['', 'STANDARD', 'STANDARD_IA'],
    help: 'Replace {account} with your account id (R2 → Overview).',
  },
  {
    provider: 'Minio',
    label: 'MinIO',
    endpoint: 'https://minio.example.lan:9000',
    region: 'us-east-1',
    forcePathStyle: true,
    storageClasses: ['', 'STANDARD', 'REDUCED_REDUNDANCY'],
    help: 'Your MinIO server’s address; http only on a private network. A self-signed certificate needs its CA certificate below.',
  },
  {
    provider: 'Other',
    label: 'Other S3-compatible',
    endpoint: 'https://',
    region: '',
    forcePathStyle: false,
    storageClasses: ['', 'STANDARD'],
    help: 'The service’s S3 endpoint URL (https).',
  },
];

export function presetOf(provider: S3Provider | string | undefined): ProviderPreset {
  return PROVIDER_PRESETS.find((p) => p.provider === provider) ?? PROVIDER_PRESETS[PROVIDER_PRESETS.length - 1];
}

/** fillEndpoint puts the region into a preset's endpoint pattern ({account} stays for the user). */
export function fillEndpoint(p: ProviderPreset, region: string): string {
  return p.endpoint.replace('{region}', region || p.region);
}

export interface CredentialFieldSpec {
  field: CredentialField;
  label: string;
  multiline?: boolean;
  help?: string;
}

/** CREDENTIAL_FIELDS are the write-only storage credentials of each remote kind (§4.3). */
export const CREDENTIAL_FIELDS: Record<Exclude<DestKind, 'local'>, CredentialFieldSpec[]> = {
  sftp: [
    { field: 'privateKey', label: 'Private key', multiline: true, help: 'OpenSSH or PKCS#8 PEM. A key is safer than a password.' },
    { field: 'privateKeyPassphrase', label: 'Key passphrase', help: 'Only if the key is encrypted (at least 8 characters).' },
    { field: 'password', label: 'Password', help: 'Instead of a key (at least 8 characters).' },
  ],
  s3: [
    { field: 'accessKeyId', label: 'Access key ID' },
    { field: 'secretAccessKey', label: 'Secret access key' },
  ],
  b2: [
    { field: 'keyId', label: 'Application key ID', help: 'Create a key restricted to this bucket.' },
    { field: 'applicationKey', label: 'Application key' },
  ],
};

/**
 * typedCredentials keeps the credential fields the user typed: a field sent replaces the stored
 * one, except SFTP, whose private key or password sent replaces the whole stored login.
 */
export function typedCredentials(kind: DestKind, values: CredentialsInput): CredentialsInput | undefined {
  if (kind === 'local') return undefined;
  const out: CredentialsInput = {};
  for (const { field } of CREDENTIAL_FIELDS[kind]) {
    const v = values[field];
    if (v !== undefined && v !== '') out[field] = v;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** credentialProblem checks a new destination's credentials as the server does, or null. */
export function credentialProblem(kind: DestKind, c: CredentialsInput): string | null {
  switch (kind) {
    case 's3':
      return c.accessKeyId && c.secretAccessKey ? null : 'Enter the access key ID and the secret access key.';
    case 'b2':
      return c.keyId && c.applicationKey ? null : 'Enter the application key ID and the application key.';
    case 'sftp':
      if (!c.privateKey && !c.password) return 'Enter a private key or a password.';
      if (!c.privateKey && c.privateKeyPassphrase) return 'A key passphrase needs the private key.';
      for (const f of ['password', 'privateKeyPassphrase'] as const) {
        if (c[f] && c[f]!.length < 8) return 'SFTP passwords and key passphrases need at least 8 characters (shorter ones could not be kept out of the logs).';
      }
      return null;
    default:
      return null;
  }
}

/** secretProblem checks an encryption password the user chose (S21), named what in the problem, or null. */
export function secretProblem(secret: string, what = 'The encryption password'): string | null {
  if ([...secret].length < 16) return `${what} needs at least 16 characters.`;
  if (secret.trim() !== secret) return `${what} may not start or end with a space.`;
  if (/[\u0000-\u001f\u007f-\u009f]/.test(secret)) return `${what} may not contain control characters.`;
  return null;
}

export const EMPTY_SFTP: SftpRemote = { host: '', port: 22, user: '', path: '', hostKeys: [] };
export const EMPTY_S3: S3Remote = { provider: 'AWS', endpoint: '', region: 'us-east-1', bucket: '', prefix: '', storageClass: '', forcePathStyle: false, caCert: '' };
export const EMPTY_B2: B2Remote = { bucket: '', prefix: '' };

/** locationProblem checks a remote's location fields, or null. */
export function locationProblem(kind: DestKind, r: { sftp: SftpRemote; s3: S3Remote; b2: B2Remote }): string | null {
  switch (kind) {
    case 'sftp':
      if (!r.sftp.host.trim()) return 'Enter the SFTP host.';
      if (!Number.isInteger(r.sftp.port) || r.sftp.port < 1 || r.sftp.port > 65535) return 'The port must be 1 to 65535.';
      if (!r.sftp.user.trim()) return 'Enter the SFTP user.';
      if (r.sftp.hostKeys.length === 0) return 'Fetch the server’s host keys and confirm their fingerprints.';
      return null;
    case 's3':
      if (r.s3.provider !== 'AWS' && !/^https?:\/\/[^/{}\s]+$/.test(r.s3.endpoint.trim())) return 'Enter the endpoint URL (https://host[:port]).';
      if (!r.s3.bucket.trim()) return 'Enter the bucket.';
      return null;
    case 'b2':
      return r.b2.bucket.trim() ? null : 'Enter the bucket.';
    default:
      return null;
  }
}

/** remoteFor builds the request's remote of kind (undefined for local). */
export function remoteFor(kind: DestKind, r: { sftp: SftpRemote; s3: S3Remote; b2: B2Remote }): SftpRemote | S3Remote | B2Remote | undefined {
  switch (kind) {
    case 'sftp':
      return { ...r.sftp, host: r.sftp.host.trim(), user: r.sftp.user.trim(), path: r.sftp.path.trim() };
    case 's3':
      return { ...r.s3, endpoint: r.s3.endpoint.trim(), region: r.s3.region.trim(), bucket: r.s3.bucket.trim(), prefix: r.s3.prefix.trim(), caCert: r.s3.caCert.trim() };
    case 'b2':
      return { bucket: r.b2.bucket.trim(), prefix: r.b2.prefix.trim() };
    default:
      return undefined;
  }
}

/** sameHostKeys compares pinned host key lists (order ignored). */
export function sameHostKeys(a: HostKey[] | undefined, b: HostKey[] | undefined): boolean {
  const key = (k: HostKey) => `${k.type} ${k.key}`;
  const x = (a ?? []).map(key).sort();
  const y = (b ?? []).map(key).sort();
  return x.length === y.length && x.every((v, i) => v === y[i]);
}

/**
 * createNeedsPassword mirrors the server's S29 check of POST /destinations: a kind other than
 * local (its sources are linked by the same request), or accepting no encryption.
 */
export function createNeedsPassword(kind: DestKind, acceptUnencrypted: boolean): boolean {
  return isRemoteKind(kind) || acceptUnencrypted;
}

/**
 * updateNeedsPassword mirrors the server's S29 check of PUT /destinations/{id}: new credentials,
 * changed host keys or CA certificate, or a source linked to a destination that is not local.
 */
export function updateNeedsPassword(
  cur: Pick<Destination, 'kind' | 'sourceIds' | 'remote'>,
  change: { credentials?: CredentialsInput; hostKeys?: HostKey[]; caCert?: string; sourceIds: number[] },
): boolean {
  if (change.credentials && Object.keys(change.credentials).length > 0) return true;
  if (change.hostKeys && !sameHostKeys(change.hostKeys, cur.remote?.hostKeys)) return true;
  if (change.caCert !== undefined && change.caCert.trim() !== (cur.remote?.caCert ?? '').trim()) return true;
  if (isRemoteKind(cur.kind)) {
    return change.sourceIds.some((id) => !cur.sourceIds.includes(id));
  }
  return false;
}

/** KitState is a destination's recovery kit custody (S21, §5.2). */
export type KitState = 'none' | 'confirmed' | 'unconfirmed';

export function kitState(d: Pick<Destination, 'encryption'>): KitState {
  const e = d.encryption;
  if (!e || !e.origin) return 'none';
  return e.kitConfirmedAt ? 'confirmed' : 'unconfirmed';
}

/** sameLocation reports whether a stored destination is at the location a create names (§4.2), as the server's pending match does. */
function sameLocation(d: Destination, kind: DestKind, target: string, remote: SftpRemote | S3Remote | B2Remote | undefined): boolean {
  const r = d.remote ?? {};
  const low = (v: string | undefined) => (v ?? '').trim().toLowerCase();
  const eq = (a: string | undefined, b: string | undefined) => (a ?? '').trim() === (b ?? '').trim();
  switch (kind) {
    case 'local': {
      const clean = (p: string) => p.trim().replace(/\/+$/, '') || '/';
      return !!target.trim() && clean(d.target) === clean(target);
    }
    case 'sftp': {
      const x = remote as SftpRemote | undefined;
      return !!x && low(r.host) === low(x.host) && (r.port ?? 22) === (x.port ?? 22) && eq(r.path, x.path);
    }
    case 's3': {
      const x = remote as S3Remote | undefined;
      const ep = (v: string | undefined) => low(v).replace(/\/+$/, '');
      return !!x && ep(r.endpoint) === ep(x.endpoint) && (ep(r.endpoint) !== '' || (low(r.region) || 'us-east-1') === (low(x.region) || 'us-east-1')) && eq(r.bucket, x.bucket) && eq(r.prefix, x.prefix);
    }
    case 'b2': {
      const x = remote as B2Remote | undefined;
      return !!x && low(r.bucket) === low(x.bucket) && eq(r.prefix, x.prefix);
    }
  }
  return false;
}

/**
 * pendingAt returns the pending destination (a create that did not finish, §4.5) of this kind and
 * engine at this location, or null. A create of its location finishes it with the encryption
 * secret the server kept for it, so the wizard sends no secret and expects the repository there.
 */
export function pendingAt(
  list: Destination[] | undefined,
  kind: DestKind,
  engine: EngineName,
  target: string,
  remote: SftpRemote | S3Remote | B2Remote | undefined,
): Destination | null {
  return (list ?? []).find((d) => d.pending && d.kind === kind && d.engine === engine && sameLocation(d, kind, target, remote)) ?? null;
}

/** isEncrypted reports whether a destination's backups are encrypted (restic, or rclone crypt). */
export function isEncrypted(d: Pick<Destination, 'encryption'>): boolean {
  return !!d.encryption && d.encryption.mode !== 'none' && !!d.encryption.mode;
}

/** stateNumber reads a number from an engine state's stats under the first of keys that has one. */
export function stateNumber(state: EngineState | null | undefined, ...keys: string[]): number | null {
  const stats = state?.stats;
  if (!stats) return null;
  for (const k of keys) {
    const v = stats[k];
    if (typeof v === 'number' && Number.isFinite(v)) return v;
  }
  return null;
}

/** snapshotCount is the repository's snapshot count as the engine state last recorded it. */
export function snapshotCount(state: EngineState | null | undefined): number | null {
  return stateNumber(state, 'snapshotCount', 'snapshots');
}

/**
 * repositoryBytes is the repository's (or remote's) last known size (§15): engine_state's
 * stats.repositoryBytes only, so another figure there (a job's bytes) is never shown as the size.
 */
export function repositoryBytes(state: EngineState | null | undefined): number | null {
  return stateNumber(state, 'repositoryBytes');
}

/** Engine setting defaults (internal/destinations engine_settings.go). */
export const GiB = 1024 ** 3;
export const DEFAULT_TRANSFERS = 4;
export const MAX_TRANSFERS = 32;

/** engineSettingsDefaults fills the engine fields of settings for engine and kind (filecopy: none). */
export function engineSettingsDefaults(s: DestinationSettings, engine: EngineName, kind: DestKind): DestinationSettings {
  const base: DestinationSettings = {
    verify: { mode: s.verify.mode, samplePercent: s.verify.samplePercent },
    hardlinks: s.hardlinks,
    adoptExisting: s.adoptExisting,
    mtimeWindowSec: s.mtimeWindowSec,
    maxChangePercent: s.maxChangePercent,
    maxChangeFiles: s.maxChangeFiles,
  };
  if (engine === 'filecopy') return base;
  base.transfers = s.transfers ?? DEFAULT_TRANSFERS;
  if (engine === 'restic') {
    base.verify.sampleMaxBytes = s.verify.sampleMaxBytes ?? 4 * GiB;
    base.restic = {
      packSizeMiB: kind === 'local' ? 16 : 64,
      batchBytes: 64 * GiB,
      batchFiles: 20000,
      pruneEveryDays: 7,
      pruneMaxUnused: '10%',
      ...s.restic,
    };
  } else {
    base.verify.sampleMaxBytes = s.verify.sampleMaxBytes ?? 16 * GiB;
    base.rclone = { batchFiles: 1000, batchBytes: 64 * GiB, ...s.rclone };
  }
  return base;
}

/** SNAPSHOT_RETENTION are restic's snapshot retention fields with the server's ranges and help text (§6.5). */
export const SNAPSHOT_RETENTION: { key: keyof Retention & `snapshot${string}`; label: string; max: number; def: number; suffix: string }[] = [
  { key: 'snapshotDaily', label: 'Daily snapshots', max: 3650, def: 7, suffix: 'days (newest complete snapshot of each)' },
  { key: 'snapshotWeekly', label: 'Weekly snapshots', max: 520, def: 4, suffix: 'weeks' },
  { key: 'snapshotMonthly', label: 'Monthly snapshots', max: 120, def: 6, suffix: 'months' },
  { key: 'snapshotYearly', label: 'Yearly snapshots', max: 100, def: 0, suffix: 'years (0 keeps none)' },
];

/** DEFAULT_RETENTION_CRON is an engine destination's retention schedule (daily 04:30, the server's default). */
export const DEFAULT_RETENTION_CRON = '30 4 * * *';

export const RETENTION_PRESETS: CronPreset[] = [
  { label: 'Daily at 04:30', cron: DEFAULT_RETENTION_CRON },
  { label: 'Weekly (Sunday 04:30)', cron: '30 4 * * 0' },
];

/** PRUNE_MAX_UNUSED is prune's --max-unused as the server accepts it. */
export const PRUNE_MAX_UNUSED = /^(?:(?:100|[0-9]{1,2})(?:\.[0-9]{1,2})?%|[0-9]{1,12}[kKmMgGtT]?|unlimited)$/;
