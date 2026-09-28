import { useMutation } from '@tanstack/react-query';
import { KeyRound, ShieldCheck } from 'lucide-react';
import { useState } from 'react';
import { fetchHostKeys } from '@/api/destinations';
import type { B2Remote, CredentialField, CredentialsInput, DestKind, HostKey, HostKeyInfo, S3Remote, SftpRemote } from '@/api/types';
import { Button } from '@/components/Button';
import { Checkbox, FormRow, NumberField, SecretField, SelectField, TextAreaField, TextField } from '@/components/Form';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Badge } from '@/components/StatusBadge';
import { CREDENTIAL_FIELDS, PROVIDER_PRESETS, fillEndpoint, presetOf, sameHostKeys } from '@/lib/destinationKinds';

// The Connection step of the add wizard and of Edit (docs/design/phase4.md §4.2, §4.3, §4.6, §15
// step 3): each remote kind's location fields, the write-only storage credentials, and for SFTP
// the host keys the user confirms before anything is sent to the server.

/** CredentialFields edits the write-only credentials of kind: password inputs that say "Stored" per hasCredentials. */
export function CredentialFields({
  kind,
  values,
  onChange,
  stored,
}: {
  kind: Exclude<DestKind, 'local'>;
  values: CredentialsInput;
  onChange: (v: CredentialsInput) => void;
  stored: Partial<Record<CredentialField, boolean>> | null | undefined;
}) {
  return (
    <>
      {CREDENTIAL_FIELDS[kind].map((f) => (
        <SecretField
          key={f.field}
          label={f.label}
          value={values[f.field] ?? ''}
          onChange={(v) => onChange({ ...values, [f.field]: v })}
          stored={!!stored?.[f.field]}
          multiline={f.multiline}
          placeholder={f.multiline ? '-----BEGIN OPENSSH PRIVATE KEY-----' : undefined}
          help={
            <>
              {f.help && <>{f.help} </>}
              {!stored?.[f.field]
                ? 'Stored encrypted and never shown again.'
                : kind === 'sftp'
                  ? // The server keeps one SFTP login (key, passphrase, password), not three fields (§4.3).
                    'Stored encrypted; never shown again. Leave empty to keep it. Typing a new private key or password replaces the stored login (key, passphrase and password); the stored passphrase stays only if it opens a new encrypted key.'
                  : 'Stored encrypted; never shown again. Leave empty to keep it, type to replace it.'}
            </>
          }
        />
      ))}
    </>
  );
}

/**
 * HostKeysField fetches the keys an SFTP server presents (POST /destinations/sftp/hostkeys) and
 * pins every one of them once the user confirms the fingerprints: a known_hosts with only one key
 * type fails when the server negotiates another (§4.6). pinned are the keys in the request.
 */
export function HostKeysField({ host, port, pinned, onPin }: { host: string; port: number; pinned: HostKey[]; onPin: (keys: HostKey[]) => void }) {
  const [presented, setPresented] = useState<{ at: string; keys: HostKeyInfo[] } | null>(null);
  const [confirmed, setConfirmed] = useState(false);
  const target = `${host.trim()}:${port}`;
  const fetcher = useMutation({
    mutationFn: () => fetchHostKeys(host.trim(), port),
    onSuccess: (keys) => {
      setPresented({ at: target, keys });
      setConfirmed(false);
    },
  });
  // Keys speak only for the host and port they came from.
  const current = presented && presented.at === target ? presented.keys : null;
  const changed = !!current && current.length > 0 && !sameHostKeys(current, pinned);
  return (
    <FormRow
      label="Host keys"
      group
      help="Bunkarr connects only to a server that shows one of these keys, so nobody in between can pretend to be your server. Compare the fingerprints with the server's own (ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub)."
    >
      <div className="space-y-2 pt-1">
        {pinned.length > 0 ? (
          <ul aria-label="Pinned host keys" className="space-y-1 text-xs">
            {pinned.map((k) => (
              <li key={`${k.type} ${k.key}`} className="flex flex-wrap items-center gap-2">
                <Badge tone="ok">Pinned</Badge>
                <span className="font-mono">{k.type}</span>
                <span className="break-all font-mono text-ink-muted">{fingerprintOf(k, current)}</span>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-xs text-warn">No host key pinned yet: fetch the server&apos;s keys and confirm their fingerprints.</p>
        )}
        <Button small icon={KeyRound} busy={fetcher.isPending} disabled={!host.trim()} onClick={() => fetcher.mutate()}>
          Fetch host keys
        </Button>
        <ErrorNotice error={fetcher.error} />
        {current && current.length === 0 && <Notice tone="warning">The server presented no host key Bunkarr can pin.</Notice>}
        {current && current.length > 0 && (
          <div role="group" aria-label="Presented host keys" className="rounded border border-line p-2">
            <p className="mb-1 text-xs text-ink-muted">{target} presents:</p>
            <ul className="mb-2 space-y-1 text-xs">
              {current.map((k) => (
                <li key={k.key} className="flex flex-wrap gap-2">
                  <span className="font-mono">{k.type}</span>
                  <span className="break-all font-mono">{k.fingerprint}</span>
                </li>
              ))}
            </ul>
            {changed ? (
              <>
                <Checkbox
                  label="These fingerprints are my server's"
                  help={pinned.length > 0 ? 'The pinned keys are replaced by these. Needs your password.' : 'Every presented key is pinned.'}
                  checked={confirmed}
                  onChange={setConfirmed}
                />
                <div className="mt-2">
                  <Button
                    small
                    variant="primary"
                    icon={ShieldCheck}
                    disabled={!confirmed}
                    onClick={() => onPin(current.map((k) => ({ type: k.type, key: k.key })))}
                  >
                    Pin these keys
                  </Button>
                </div>
              </>
            ) : (
              <p className="text-xs text-accent">These are the pinned keys.</p>
            )}
          </div>
        )}
      </div>
    </FormRow>
  );
}

/** fingerprintOf shows a pinned key's fingerprint when a fetch presented it, else a shortened key. */
function fingerprintOf(k: HostKey, presented: HostKeyInfo[] | null): string {
  const f = presented?.find((p) => p.type === k.type && p.key === k.key)?.fingerprint;
  return f ?? `${k.key.slice(0, 24)}…`;
}

/** SftpFields edits an SFTP location; editing a stored destination leaves only the host keys editable. */
export function SftpFields({ value, onChange, fixed }: { value: SftpRemote; onChange: (v: SftpRemote) => void; fixed?: boolean }) {
  const set = (patch: Partial<SftpRemote>) => onChange({ ...value, ...patch });
  const locked = fixed ? 'The location cannot change after the destination is created.' : undefined;
  return (
    <>
      <TextField label="Host" value={value.host} onChange={(host) => set({ host, hostKeys: host === value.host ? value.hostKeys : [] })} mono placeholder="backup.example.net" disabled={fixed} help={locked} />
      <NumberField label="Port" value={value.port} onChange={(port) => set({ port, hostKeys: port === value.port ? value.hostKeys : [] })} min={1} max={65535} disabled={fixed} />
      <TextField label="User" value={value.user} onChange={(user) => set({ user })} mono placeholder="bunkarr" disabled={fixed} />
      <TextField
        label="Path"
        value={value.path}
        onChange={(path) => set({ path })}
        mono
        placeholder="/backups/bunkarr"
        disabled={fixed}
        help={fixed ? undefined : 'An absolute path, or one relative to the user’s login folder. Empty: the login folder itself.'}
      />
    </>
  );
}

/** S3Fields edits an S3 location; the provider presets fill the endpoint pattern. */
export function S3Fields({ value, onChange, fixed }: { value: S3Remote; onChange: (v: S3Remote) => void; fixed?: boolean }) {
  const set = (patch: Partial<S3Remote>) => onChange({ ...value, ...patch });
  const preset = presetOf(value.provider);
  function choose(provider: string) {
    const p = presetOf(provider);
    onChange({ ...value, provider: p.provider, region: p.region, endpoint: fillEndpoint(p, p.region), forcePathStyle: p.forcePathStyle, storageClass: '' });
  }
  return (
    <>
      <SelectField
        label="Provider"
        value={value.provider}
        onChange={choose}
        disabled={fixed}
        options={PROVIDER_PRESETS.map((p) => ({ value: p.provider, label: p.label }))}
        help={fixed ? 'The location cannot change after the destination is created.' : preset.help}
      />
      <TextField
        label="Endpoint"
        value={value.endpoint}
        onChange={(endpoint) => set({ endpoint })}
        mono
        disabled={fixed}
        placeholder={value.provider === 'AWS' ? 'empty: the region’s endpoint' : 'https://…'}
        help={fixed ? undefined : 'https only (http is accepted for a private address, with a warning). Bunkarr never turns certificate checks off.'}
      />
      <TextField
        label="Region"
        value={value.region}
        onChange={(region) => set({ region, endpoint: preset.endpoint.includes('{region}') && !fixed ? fillEndpoint(preset, region) : value.endpoint })}
        mono
        disabled={fixed}
        placeholder={preset.region || 'us-east-1'}
      />
      <TextField label="Bucket" value={value.bucket} onChange={(bucket) => set({ bucket })} mono disabled={fixed} placeholder="my-backups" />
      <TextField label="Prefix" value={value.prefix} onChange={(prefix) => set({ prefix })} mono disabled={fixed} placeholder="bunkarr" help={fixed ? undefined : 'A folder inside the bucket (optional).'} />
      <SelectField
        label="Storage class"
        value={value.storageClass}
        onChange={(storageClass) => set({ storageClass })}
        disabled={fixed}
        options={preset.storageClasses.map((c) => ({ value: c, label: c || 'Provider default' }))}
        help="Archive classes whose objects need a restore request are not offered: every job reads what it checks."
      />
      <FormRow label="Path-style" group>
        <div className="pt-2">
          <Checkbox
            label="Address the bucket in the path (path-style)"
            help="For MinIO and servers without per-bucket host names."
            checked={value.forcePathStyle}
            disabled={fixed}
            onChange={(forcePathStyle) => set({ forcePathStyle })}
          />
        </div>
      </FormRow>
      <TextAreaField
        label="CA certificate"
        value={value.caCert}
        onChange={(caCert) => set({ caCert })}
        mono
        rows={3}
        placeholder="-----BEGIN CERTIFICATE-----"
        help="Only for a self-signed endpoint: its CA certificate (PEM). Changing it needs your password."
      />
    </>
  );
}

/** B2Fields edits a Backblaze B2 location. */
export function B2Fields({ value, onChange, fixed }: { value: B2Remote; onChange: (v: B2Remote) => void; fixed?: boolean }) {
  const set = (patch: Partial<B2Remote>) => onChange({ ...value, ...patch });
  return (
    <>
      <TextField label="Bucket" value={value.bucket} onChange={(bucket) => set({ bucket })} mono disabled={fixed} placeholder="my-backups" help={fixed ? 'The location cannot change after the destination is created.' : undefined} />
      <TextField label="Prefix" value={value.prefix} onChange={(prefix) => set({ prefix })} mono disabled={fixed} placeholder="bunkarr" help={fixed ? undefined : 'A folder inside the bucket (optional).'} />
      {!fixed && (
        <p className="-mt-2 mb-4 text-xs text-ink-muted sm:ml-[12rem]">
          Use an application key restricted to this bucket: whoever reads Bunkarr&apos;s /config could otherwise erase every bucket of the account.
        </p>
      )}
    </>
  );
}
