import { useMutation, useQueryClient } from '@tanstack/react-query';
import { DatabaseBackup, FlaskConical, Pencil, Plus, Server, Trash2, X } from 'lucide-react';
import { useState, type FormEvent, type ReactNode } from 'react';
import { Link, useNavigate } from 'react-router';
import { errorMessage } from '@/api/client';
import { createIntegration, deleteIntegration, plexBackup, testIntegration, updateIntegration } from '@/api/integrations';
import type { CronSchedule, Integration, IntegrationInput, IntegrationTestResult, PathMapping, PlexSettings } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CronInput } from '@/components/CronInput';
import { CheckboxField, FormRow, FormSection, SecretField, SelectField, TextField, inputClass } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { EmptyState, Page } from '@/components/Page';
import { PathField, PathPicker } from '@/components/PathPicker';
import { PlexSignInPanel, usePlexSignIn } from '@/components/plex/PlexSignIn';
import type { PlexSignInSelection } from '@/components/plex/plexSignInMachine';
import { Badge } from '@/components/StatusBadge';
import { DEFAULT_PLEX_BACKUP_CRON, PLEX_BACKUP_PRESETS, describeCron, overlapsWindow, validateCron } from '@/lib/cron';
import { keys, useDestinations, useIntegrations } from '@/lib/lookups';

/** Plex's default butler window (ButlerStartHour–ButlerEndHour) when the server does not say. */
export const DEFAULT_BUTLER = { start: 2, end: 5 };

function emptySettings(): PlexSettings {
  return { dataPath: '', pathMappings: [], backup: { destinationId: 0, cron: DEFAULT_PLEX_BACKUP_CRON, enabled: true } };
}

function withDefaults(s: Partial<PlexSettings> | null | undefined): PlexSettings {
  const d = emptySettings();
  return {
    dataPath: s?.dataPath ?? d.dataPath,
    pathMappings: s?.pathMappings ?? d.pathMappings,
    backup: { ...d.backup, ...s?.backup },
  };
}

/** Settings → Plex: Plex servers (URL, token, data path, path mappings) and their DB backup. */
export function Plex() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const integrations = useIntegrations();
  const destinations = useDestinations();
  const [editing, setEditing] = useState<Integration | 'new' | null>(null);
  const [deleting, setDeleting] = useState<Integration | null>(null);
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);
  const [busy, setBusy] = useState<number | null>(null);

  const servers = (integrations.data ?? []).filter((i) => i.type === 'plex');
  const destName = (id: number) => destinations.data?.find((d) => d.id === id)?.name ?? `destination #${id}`;

  async function backupNow(i: Integration) {
    const dest = i.settings?.backup?.destinationId;
    if (!dest) return;
    setNotice(null);
    setBusy(i.id);
    try {
      const job = await plexBackup(i.id, { destinationId: dest, dryRun: false });
      await qc.invalidateQueries({ queryKey: keys.jobs });
      navigate(`/activity/jobs/${job.id}`);
    } catch (e) {
      setNotice({ tone: 'error', body: `${i.name}: ${errorMessage(e)}` });
    } finally {
      setBusy(null);
    }
  }

  return (
    <Page
      title="Plex"
      actions={
        <Button variant="ghost" icon={Plus} onClick={() => setEditing('new')}>
          Add Plex server
        </Button>
      }
    >
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      <ErrorNotice error={integrations.error} />
      {integrations.data && servers.length === 0 ? (
        <EmptyState icon={Server} title="No Plex server yet">
          <p>
            Connect Plex to import its libraries as sources (Library → Import from Plex) and to back up its database and preferences to a destination.
          </p>
          <div className="mt-4">
            <Button variant="primary" icon={Plus} onClick={() => setEditing('new')}>
              Add Plex server
            </Button>
          </div>
        </EmptyState>
      ) : (
        <div className="grid max-w-5xl gap-3 lg:grid-cols-2">
          {servers.map((i) => {
            const s = withDefaults(i.settings);
            return (
              <article key={i.id} aria-label={i.name} className="rounded border border-line bg-panel p-4">
                <div className="mb-2 flex items-start justify-between gap-2">
                  <div className="min-w-0">
                    <h2 className="flex flex-wrap items-center gap-2 font-medium">
                      {i.name}
                      {!i.enabled && <Badge>Disabled</Badge>}
                      {i.hasApiKey ? <Badge tone="ok">Token stored</Badge> : <Badge tone="warn">No token</Badge>}
                    </h2>
                    <div className="break-all font-mono text-xs text-ink-muted">{i.url}</div>
                  </div>
                  <div className="flex shrink-0">
                    <IconButton label={`Edit ${i.name}`} icon={Pencil} onClick={() => setEditing(i)} />
                    <IconButton label={`Delete ${i.name}`} icon={Trash2} className="hover:text-danger" onClick={() => setDeleting(i)} />
                  </div>
                </div>
                <dl className="grid grid-cols-[8rem_1fr] gap-x-3 gap-y-1 text-sm">
                  <dt className="text-ink-muted">Data path</dt>
                  <dd className="break-all font-mono text-xs">{s.dataPath || <span className="font-sans text-warn">not set</span>}</dd>
                  <dt className="text-ink-muted">Path mappings</dt>
                  <dd className="text-xs">
                    {s.pathMappings.length === 0
                      ? 'none'
                      : s.pathMappings.map((m) => (
                          <div key={`${m.plex}>${m.local}`} className="break-all font-mono">
                            {m.plex} → {m.local}
                          </div>
                        ))}
                  </dd>
                  <dt className="text-ink-muted">DB backup</dt>
                  <dd className="text-xs">
                    {s.backup.destinationId ? (
                      <>
                        to {destName(s.backup.destinationId)}, {s.backup.enabled ? describeCron(s.backup.cron) : 'manual only'}
                      </>
                    ) : (
                      'no destination chosen'
                    )}
                  </dd>
                </dl>
                <div className="mt-3">
                  <Button
                    small
                    icon={DatabaseBackup}
                    busy={busy === i.id}
                    disabled={!s.backup.destinationId || !s.dataPath}
                    title={!s.backup.destinationId ? 'Choose a backup destination first' : !s.dataPath ? 'Set the data path first' : undefined}
                    onClick={() => void backupNow(i)}
                  >
                    Back up now
                  </Button>
                </div>
              </article>
            );
          })}
        </div>
      )}
      {editing && <PlexForm integration={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
      {deleting && (
        <ConfirmDialog
          title="Delete Plex server"
          confirmLabel="Delete"
          danger
          onClose={() => setDeleting(null)}
          onConfirm={async () => {
            await deleteIntegration(deleting.id);
            await qc.invalidateQueries({ queryKey: keys.integrations });
            await qc.invalidateQueries({ queryKey: keys.schedules });
          }}
        >
          <p>
            Delete <strong>{deleting.name}</strong>? Its stored token and backup schedule are removed.
          </p>
          <p className="text-ink-muted">Sources imported from it and database backups already on your destinations stay.</p>
        </ConfirmDialog>
      )}
    </Page>
  );
}

function MappingsEditor({ value, onChange }: { value: PathMapping[]; onChange: (v: PathMapping[]) => void }) {
  const set = (i: number, patch: Partial<PathMapping>) => onChange(value.map((m, j) => (j === i ? { ...m, ...patch } : m)));
  return (
    <FormRow
      label="Path mappings"
      group
      help="Plex sees your media at its own paths (inside its container). Map each Plex folder to where Bunkarr sees the same folder, for example /data/movies → /media/movies. Import from Plex uses these."
    >
      <div className="space-y-2">
        {value.map((m, i) => (
          <div key={i} className="flex flex-col gap-2 rounded border border-line p-2 sm:flex-row sm:items-center sm:border-0 sm:p-0">
            <input
              aria-label={`Plex path ${i + 1}`}
              className={`${inputClass} font-mono sm:w-2/5`}
              value={m.plex}
              placeholder="/data/movies"
              spellCheck={false}
              onChange={(e) => set(i, { plex: e.target.value })}
            />
            <span className="hidden text-ink-muted sm:inline" aria-hidden="true">
              →
            </span>
            <div className="min-w-0 flex-1">
              <PathPicker label={`Bunkarr path ${i + 1}`} value={m.local} placeholder="/media/movies" onChange={(local) => set(i, { local })} />
            </div>
            <IconButton label={`Remove mapping ${i + 1}`} icon={X} onClick={() => onChange(value.filter((_, j) => j !== i))} />
          </div>
        ))}
        <Button small icon={Plus} onClick={() => onChange([...value, { plex: '', local: '' }])}>
          Add mapping
        </Button>
      </div>
    </FormRow>
  );
}

/** selectionKey identifies a sign-in selection for comparing test results. */
function selectionKey(s: PlexSignInSelection | null): string {
  return s ? `${s.signInId}/${s.serverId}/${s.useAccountToken ? 'account' : 'server'}` : '';
}

/**
 * PlexForm adds or edits a Plex server. The token is write-only: it is never shown again. "Sign in
 * with Plex" picks a server and a tested connection; the token then stays on Bunkarr's server
 * (plexSignIn), and closing the form without saving forgets the sign-in there.
 */
function PlexForm({ integration, onClose }: { integration: Integration | null; onClose: () => void }) {
  const qc = useQueryClient();
  const destinations = useDestinations();
  const signIn = usePlexSignIn();
  const [picked, setPicked] = useState<PlexSignInSelection | null>(null);
  // A selection speaks only for the sign-in and the server it came from (Start over, expiry, a
  // typed token, another server or the account-token box changing end it).
  const selection =
    picked && signIn.state.step === 'serverChosen' && signIn.state.signInId === picked.signInId && signIn.state.serverId === picked.serverId ? picked : null;
  const initial = withDefaults(integration?.settings);
  const [name, setName] = useState(integration?.name ?? 'Plex');
  const [url, setUrl] = useState(integration?.url ?? '');
  const [token, setToken] = useState('');
  const [enabled, setEnabled] = useState(integration?.enabled ?? true);
  const [dataPath, setDataPath] = useState(initial.dataPath);
  const [mappings, setMappings] = useState<PathMapping[]>(initial.pathMappings);
  const [destinationId, setDestinationId] = useState(initial.backup.destinationId);
  const [schedule, setSchedule] = useState<CronSchedule>({ cron: initial.backup.cron, enabled: initial.backup.enabled });
  // The last test and the URL and token (or sign-in selection) it tested.
  const [test, setTest] = useState<{ url: string; token: string; signIn: string; result: IntegrationTestResult } | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  // Counts Save clicks, so a repeated form error is shown (scrolled into view) again.
  const [attempt, setAttempt] = useState(0);

  const tester = useMutation({
    mutationFn: (v: { url: string; token: string; signIn: string; ref: PlexSignInSelection | null }) =>
      testIntegration({
        type: 'plex',
        url: v.url,
        apiKey: v.ref ? undefined : v.token || undefined,
        id: integration?.id,
        plexSignIn: v.ref ? { id: v.ref.signInId, serverId: v.ref.serverId, useAccountToken: v.ref.useAccountToken || undefined } : undefined,
      }),
    onMutate: () => setTest(null),
    onSuccess: (result, v) => setTest({ url: v.url, token: v.token, signIn: v.signIn, result }),
  });
  const saver = useMutation({
    mutationFn: (body: IntegrationInput) => (integration ? updateIntegration(integration.id, body) : createIntegration(body)),
    onSuccess: async (_, body) => {
      if (body.plexSignIn) {
        // The server consumed the sign-in with the save: nothing to forget on close.
        signIn.markSaved();
      }
      await qc.invalidateQueries({ queryKey: keys.integrations });
      await qc.invalidateQueries({ queryKey: keys.schedules });
      onClose();
    },
  });

  // A result, or an error, speaks only for the URL and token (or sign-in selection) it tested, and
  // a new test replaces it.
  const tested = (v: { url: string; token: string; signIn: string } | undefined) =>
    v?.url === url.trim() && v.token === token.trim() && v.signIn === selectionKey(selection);
  const current = test && tested(test) ? test.result : null;
  const testError = tested(tester.variables) ? tester.error : null;
  const butler = {
    start: current?.butlerStartHour ?? DEFAULT_BUTLER.start,
    end: current?.butlerEndHour ?? DEFAULT_BUTLER.end,
  };
  const overlap = destinationId > 0 && schedule.enabled && overlapsWindow(schedule.cron, butler.start, butler.end);

  function submit(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    setAttempt((n) => n + 1);
    if (!name.trim() || !url.trim()) {
      setFormError('Enter a name and the server URL.');
      return;
    }
    if (!integration && !token.trim() && !selection) {
      setFormError('Enter the Plex token.');
      return;
    }
    // The server checks any non-empty backup cron, even without a destination: an invalid one
    // that is not in use (no destination, so no schedule field) is dropped. "Manual only" drops
    // it in CronInput.
    const cron = destinationId > 0 || !validateCron(schedule.cron) ? schedule.cron.trim() : '';
    const cronError = cron || (destinationId > 0 && schedule.enabled) ? validateCron(cron) : null;
    if (cronError) {
      setFormError(`The backup schedule is invalid: ${cronError}`);
      return;
    }
    if (destinationId > 0 && schedule.enabled && !dataPath.trim()) {
      setFormError('Set the data path: scheduled database backups read the Plex database from it. Or choose "Manual only" for the schedule.');
      return;
    }
    const pathMappings = mappings.map((m) => ({ plex: m.plex.trim(), local: m.local.trim() })).filter((m) => m.plex || m.local);
    if (pathMappings.some((m) => !m.plex || !m.local)) {
      setFormError('Every path mapping needs both a Plex path and a Bunkarr path.');
      return;
    }
    const body: IntegrationInput = {
      type: 'plex',
      name: name.trim(),
      url: url.trim(),
      enabled,
      settings: {
        dataPath: dataPath.trim(),
        pathMappings,
        backup: { destinationId, cron, enabled: destinationId > 0 && schedule.enabled },
      },
    };
    if (selection) {
      body.plexSignIn = { id: selection.signInId, serverId: selection.serverId, useAccountToken: selection.useAccountToken || undefined };
    } else if (token.trim()) {
      body.apiKey = token.trim();
    }
    saver.mutate(body);
  }

  function takeSelection(sel: PlexSignInSelection) {
    setPicked(sel);
    setUrl(sel.uri);
    setToken('');
    if (!integration && (!name.trim() || name.trim() === 'Plex')) {
      setName(sel.serverName);
    }
  }

  // Typing a token (or "Use a token instead") ends the sign-in: the typed token is what is saved.
  function tokenInstead(v: string) {
    if (selection) {
      setPicked(null);
      signIn.reset();
    }
    setToken(v);
  }

  const destOptions = [{ value: '0', label: 'None (no database backup)' }, ...(destinations.data ?? []).map((d) => ({ value: String(d.id), label: d.name }))];
  return (
    <Modal
      title={integration ? `Edit Plex server · ${integration.name}` : 'Add Plex server'}
      size="xl"
      onClose={onClose}
      footer={
        <>
          <Button
            icon={FlaskConical}
            busy={tester.isPending}
            disabled={!url.trim()}
            onClick={() => tester.mutate({ url: url.trim(), token: token.trim(), signIn: selectionKey(selection), ref: selection })}
          >
            Test
          </Button>
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="plex-form" busy={saver.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="plex-form" onSubmit={submit} noValidate>
        {formError && (
          <Notice key={attempt} tone="error">
            {formError}
          </Notice>
        )}
        <ErrorNotice error={saver.error ?? testError} />
        {current && (
          <Notice tone={current.ok ? 'success' : 'error'} reveal revealKey={current}>
            {current.message || (current.ok ? 'Connected.' : 'Cannot connect.')}
            {current.ok && (current.version || current.machineIdentifier) && (
              <div className="mt-1 text-xs text-ink-muted">
                {current.version && <>Version {current.version}</>}
                {current.machineIdentifier && <> · Server {current.machineIdentifier}</>}
              </div>
            )}
          </Notice>
        )}
        <FormSection
          title="Sign in with Plex"
          description={
            integration
              ? 'Sign in to replace the stored token with a new one for this server, or to pick another of its connections.'
              : 'Bunkarr lists your servers from plex.tv and tests their connections from where it runs.'
          }
        >
          <PlexSignInPanel controller={signIn} onSelect={takeSelection} onClear={() => setPicked(null)} selection={selection} disabled={saver.isPending} />
        </FormSection>
        <FormSection title="Server" description={selection ? undefined : 'Or enter the details manually.'}>
          <TextField label="Name" value={name} onChange={setName} autoFocus={!integration} />
          <TextField label="URL" type="url" value={url} onChange={setUrl} mono placeholder="http://plex:32400" help="How Bunkarr reaches Plex, for example http://192.168.1.10:32400." />
          <SecretField
            label="Token"
            value={token}
            onChange={tokenInstead}
            stored={!!integration?.hasApiKey && !selection}
            reenter={!!integration && url.trim() !== integration.url}
            placeholder={selection ? `From Plex sign-in — ${selection.serverName} (${selection.owned ? 'owner' : 'shared'})` : 'X-Plex-Token'}
            help={
              selection ? (
                <span className="inline-flex flex-wrap items-center gap-2">
                  Token: from Plex sign-in — {selection.serverName} ({selection.owned ? 'owner' : 'shared'}
                  {selection.useAccountToken ? ', your account token' : ''}). It stays on Bunkarr&apos;s server.
                  <Button small variant="ghost" onClick={() => tokenInstead('')}>
                    Use a token instead
                  </Button>
                </span>
              ) : (
                'Your Plex token (X-Plex-Token). It is stored encrypted and never shown again; Bunkarr sends it only in a request header.'
              )
            }
          />
          <CheckboxField label="Enabled" checked={enabled} onChange={setEnabled} text="Use this server" />
        </FormSection>
        <FormSection title="Paths">
          <PathField
            label="Data path"
            value={dataPath}
            onChange={setDataPath}
            placeholder="/plex"
            help={
              <>
                The &quot;Plex Media Server&quot; folder mounted into Bunkarr, read-only is enough (for example
                <code> …/Library/Application Support/Plex Media Server:/plex:ro</code>). Bunkarr must be able to read Preferences.xml, so its PUID must match
                Plex&apos;s user.
              </>
            }
          />
          <MappingsEditor value={mappings} onChange={setMappings} />
        </FormSection>
        <FormSection
          title="Database backup"
          description="Backs up the Plex database (online, consistent copy while Plex runs), the blobs database and Preferences.xml, verifies the copy and keeps versions on the destination."
        >
          <SelectField label="Destination" value={String(destinationId)} onChange={(v) => setDestinationId(Number(v))} options={destOptions} />
          {destinationId > 0 && (
            <CronInput
              label="Schedule"
              value={schedule}
              onChange={setSchedule}
              presets={PLEX_BACKUP_PRESETS}
              warning={
                overlap
                  ? `This runs during Plex's maintenance window (${String(butler.start).padStart(2, '0')}:00–${String(butler.end).padStart(2, '0')}:00${current?.butlerStartHour == null ? ', Plex default' : ''}). Plex may optimize its database then, which makes the backup slow and Plex's WAL file grow; pick a time outside it.`
                  : undefined
              }
            />
          )}
          {destinationId > 0 && (
            <p className="text-xs text-ink-muted sm:ml-[12rem]">
              Versions kept are set per destination (Destinations → Retention). Backups are listed under the destination&apos;s snapshots.{' '}
              <Link to="/destinations" className="text-accent hover:underline" onClick={onClose}>
                Destinations
              </Link>
            </p>
          )}
        </FormSection>
      </form>
    </Modal>
  );
}
