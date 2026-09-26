import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { FlaskConical, Library, Pencil, Plus, RefreshCw, ShieldAlert, Trash2 } from 'lucide-react';
import { useState, type FormEvent, type ReactNode } from 'react';
import { Link } from 'react-router';
import { getIndex, refreshIntegration, type IndexView } from '@/api/arr';
import { errorMessage } from '@/api/client';
import { deleteIntegration } from '@/api/integrations';
import {
  DEFAULT_PLEX_INDEX,
  DEFAULT_REFRESH,
  HAS_KEY,
  NEEDS_PLEX,
  PROVIDER_NAMES,
  PROVIDER_TYPES,
  createProvider,
  isProviderType,
  plexIndex,
  providerSettings,
  savePlexIndex,
  testProvider,
  updateProvider,
  type ProviderCacheStats,
  type ProviderIntegrationInput,
  type ProviderTestInput,
  type ProviderTestResult,
  type ProviderType,
} from '@/api/providers';
import type { CronSchedule, Integration, PlexIndexSettings } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CronInput } from '@/components/CronInput';
import { CheckboxField, FormSection, NumberField, SecretField, SelectField, TextField } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Badge } from '@/components/StatusBadge';
import { describeCron, validateCron, type CronPreset } from '@/lib/cron';
import { formatNumber, formatRelative } from '@/lib/format';
import { keys, useIntegrations } from '@/lib/lookups';
import { indexKey } from './ConnectArr';

/** REFRESH_PRESETS are the offered refresh schedules of each type (its default first). */
export const REFRESH_PRESETS: Record<ProviderType | 'plex', CronPreset[]> = {
  tautulli: [
    { label: 'Daily at 02:00', cron: '0 2 * * *' },
    { label: 'Every 6 hours', cron: '0 */6 * * *' },
  ],
  seerr: [
    { label: 'Daily at 02:30', cron: '30 2 * * *' },
    { label: 'Every 6 hours (at :30)', cron: '30 */6 * * *' },
  ],
  maintainerr: [
    { label: 'Every 6 hours (at :45)', cron: '45 */6 * * *' },
    { label: 'Hourly (at :45)', cron: '45 * * * *' },
    { label: 'Daily at 03:45', cron: '45 3 * * *' },
  ],
  plex: [
    { label: 'Daily at 01:00', cron: '0 1 * * *' },
    { label: 'Every 6 hours', cron: '0 */6 * * *' },
  ],
};

const URL_EXAMPLE: Record<ProviderType, string> = { tautulli: 'http://tautulli:8181', seerr: 'http://seerr:5055', maintainerr: 'http://maintainerr:6246' };

const KEY_HELP: Record<ProviderType, string> = {
  tautulli: 'Tautulli → Settings → Web Interface → API key (Tautulli 2.18 or newer).',
  seerr: 'Seerr → Settings → General → API Key.',
  maintainerr: '',
};

const WHAT: Record<ProviderType, string> = {
  tautulli: 'Play counts and last-watched dates for the tier rules (tautulli.playCount, tautulli.lastWatched).',
  seerr: 'Who requested what, for the tier rules (seerr.requested, seerr.requestedBy).',
  maintainerr: 'What Maintainerr is about to delete, for the tier rules (maintainerr.pendingDelete).',
};

export const MAINTAINERR_NOTICE = 'Maintainerr has no API authentication; Bunkarr only reads from it.';

/** guardHeld: the last refresh kept the old rows (the refresh guard, S10). */
function guardHeld(index: IndexView): boolean {
  return index.status === 'ok' && !!index.error && index.error.startsWith('Refresh guard');
}

/** cacheSummary describes a cache's counts. */
function cacheSummary(type: ProviderType | 'plex', st: ProviderCacheStats): string | null {
  switch (type) {
    case 'tautulli':
      return st.plays === undefined ? null : `${formatNumber(st.plays)} plays of ${formatNumber(st.ratingKeys ?? 0)} items`;
    case 'seerr':
      return st.requests === undefined ? null : `${formatNumber(st.requests)} requests (${formatNumber(st.counted ?? 0)} counted)`;
    case 'maintainerr':
      return st.pending === undefined ? null : `${formatNumber(st.pending)} pending deletion, ${formatNumber(st.undecided ?? 0)} undecided`;
    case 'plex':
      return st.items === undefined ? null : `${formatNumber(Number(st.sections ?? 0))} libraries, ${formatNumber(st.items)} items, ${formatNumber(st.files ?? 0)} files`;
  }
}

/** CacheStatusLine says whether a cache is fresh, what it holds, and what makes its facts partial. */
export function CacheStatusLine({ index, type, onApplyHeld }: { index: IndexView; type: ProviderType | 'plex'; onApplyHeld?: () => void }) {
  const st = (index.stats ?? {}) as ProviderCacheStats;
  const summary = cacheSummary(type, st);
  const held = guardHeld(index);
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-2">
        {index.status === 'never' && !index.refreshedAt ? (
          <Badge>Not refreshed yet</Badge>
        ) : index.fresh ? (
          <Badge tone="ok">Fresh</Badge>
        ) : (
          <Badge tone="warn">Stale</Badge>
        )}
        {index.refreshedAt && <span>refreshed {formatRelative(index.refreshedAt)}</span>}
        {summary && <span>{summary}</span>}
      </div>
      {!index.fresh && index.reason && <div className="text-warn">{index.reason}. Tier rules treat its facts as unknown.</div>}
      {index.status === 'failed' && index.error && <div className="text-danger">Last refresh failed: {index.error}</div>}
      {held && (
        <div className="text-warn">
          {index.error} The old data is kept and ages into unknown.{' '}
          {onApplyHeld && (
            <Button small variant="ghost" icon={ShieldAlert} onClick={onApplyHeld}>
              Apply held changes
            </Button>
          )}
        </div>
      )}
      {type === 'tautulli' && ((st.sectionsWithoutHistory ?? []).length > 0 || (st.usersWithoutHistory ?? 0) > 0) && (
        <div className="text-warn">
          Tautulli does not keep the history of {(st.sectionsWithoutHistory ?? []).length} libraries and {st.usersWithoutHistory ?? 0} users: play counts
          there are lower bounds.
        </div>
      )}
      {type === 'maintainerr' && st.plexIndexFresh === false && (
        <div className="text-warn">The linked Plex library index was not fresh during the refresh: season and episode members are undecided.</div>
      )}
      {type === 'plex' && (st.filesUnmapped ?? 0) > 0 && (
        <div className="text-warn">{formatNumber(st.filesUnmapped ?? 0)} Plex files map to no source: check the Plex path mappings.</div>
      )}
    </div>
  );
}

/**
 * ProviderConnections is the Plex library index, Tautulli, Seerr and Maintainerr part of Settings →
 * Connect: one card per connection with its cache status, and the forms that edit them.
 */
export function ProviderConnections() {
  const qc = useQueryClient();
  const integrations = useIntegrations();
  const [editing, setEditing] = useState<Integration | ProviderType | null>(null);
  const [indexing, setIndexing] = useState<Integration | null>(null);
  const [deleting, setDeleting] = useState<Integration | null>(null);
  const all = integrations.data ?? [];
  const providers = all.filter((i) => isProviderType(i.type));
  const plexes = all.filter((i) => i.type === 'plex');

  return (
    <section aria-labelledby="providers-heading" className="mb-8 max-w-5xl">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2 border-b border-line pb-2">
        <h2 id="providers-heading" className="text-lg">
          Tautulli, Seerr and Maintainerr
        </h2>
        <div className="flex flex-wrap gap-1">
          {PROVIDER_TYPES.map((t) => (
            <Button key={t} small variant="ghost" icon={Plus} onClick={() => setEditing(t)}>
              Add {PROVIDER_NAMES[t]}
            </Button>
          ))}
        </div>
      </div>
      <ErrorNotice error={integrations.error} />
      {integrations.data && providers.length === 0 && (
        <p className="mb-3 text-sm text-ink-muted">
          Bunkarr only reads from these apps: play history (Tautulli), requests (Seerr) and pending deletions (Maintainerr) become facts for the tier
          rules. Tautulli and Maintainerr are matched through the Plex server they are linked to, so that server&apos;s library index is turned on.
        </p>
      )}
      <div className="grid gap-3 lg:grid-cols-2">
        {plexes.map((p) => (
          <PlexIndexCard key={`plex-${p.id}`} integration={p} onEdit={() => setIndexing(p)} />
        ))}
        {providers.map((i) => (
          <ProviderCard key={i.id} integration={i} plexes={plexes} onEdit={() => setEditing(i)} onDelete={() => setDeleting(i)} />
        ))}
      </div>
      {editing && (
        <ProviderForm
          integration={typeof editing === 'string' ? null : editing}
          type={typeof editing === 'string' ? editing : (editing.type as ProviderType)}
          plexes={plexes}
          onClose={() => setEditing(null)}
        />
      )}
      {indexing && <PlexIndexForm integration={indexing} onClose={() => setIndexing(null)} />}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${PROVIDER_NAMES[deleting.type as ProviderType]}`}
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
            Delete <strong>{deleting.name}</strong>? Its stored data and schedule are removed; tier rules that use it then treat its facts as unknown, so
            the files they cover stay fully backed up.
          </p>
        </ConfirmDialog>
      )}
    </section>
  );
}

/** useQueued returns a notice setter for a queued job, with a link to it. */
function useQueued(setNotice: (n: { tone: 'success' | 'error'; body: ReactNode }) => void) {
  const qc = useQueryClient();
  return async (job: { id: number }, what: string) => {
    setNotice({
      tone: 'success',
      body: (
        <>
          {what}{' '}
          <Link className="text-accent hover:underline" to={`/activity/jobs/${job.id}`}>
            Open the job
          </Link>
        </>
      ),
    });
    await qc.invalidateQueries({ queryKey: keys.jobs });
  };
}

function ProviderCard({ integration: i, plexes, onEdit, onDelete }: { integration: Integration; plexes: Integration[]; onEdit: () => void; onDelete: () => void }) {
  const type = i.type as ProviderType;
  const s = providerSettings(i, type);
  const index = useQuery({ queryKey: indexKey(i.id), queryFn: () => getIndex(i.id), refetchInterval: 30_000 });
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);
  const [applyHeld, setApplyHeld] = useState(false);
  const queued = useQueued(setNotice);
  const refresher = useMutation({
    mutationFn: () => refreshIntegration(i.id),
    onSuccess: (job) => queued(job, 'Refresh queued.'),
    onError: (e) => setNotice({ tone: 'error', body: errorMessage(e) }),
  });
  const linked = plexes.find((p) => p.id === s.plexIntegrationId);
  return (
    <article aria-label={i.name} className="rounded border border-line bg-panel p-4">
      <div className="mb-2 flex items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="flex flex-wrap items-center gap-2 font-medium">
            {i.name}
            <Badge tone="info">{PROVIDER_NAMES[type]}</Badge>
            {!i.enabled && <Badge>Disabled</Badge>}
            {HAS_KEY[type] ? (
              i.hasApiKey ? (
                <Badge tone="ok">Key stored</Badge>
              ) : (
                <Badge tone="warn">No API key</Badge>
              )
            ) : (
              <Badge tone="warn" title={MAINTAINERR_NOTICE}>
                No authentication
              </Badge>
            )}
          </h3>
          <div className="break-all font-mono text-xs text-ink-muted">{i.url}</div>
        </div>
        <div className="flex shrink-0">
          <IconButton label={`Edit ${i.name}`} icon={Pencil} onClick={onEdit} />
          <IconButton label={`Delete ${i.name}`} icon={Trash2} className="hover:text-danger" onClick={onDelete} />
        </div>
      </div>
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      <dl className="grid grid-cols-[8rem_1fr] gap-x-3 gap-y-1 text-sm">
        <dt className="text-ink-muted">Plex server</dt>
        <dd className="text-xs">
          {linked ? (
            <>
              {linked.name}
              {!plexIndex(linked).enabled && NEEDS_PLEX[type] && <span className="ml-2 text-warn">its library index is off</span>}
            </>
          ) : NEEDS_PLEX[type] ? (
            <span className="text-warn">not linked: edit the connection and choose the Plex server</span>
          ) : (
            <span className="text-ink-muted">not linked (optional)</span>
          )}
        </dd>
        <dt className="text-ink-muted">Refresh</dt>
        <dd className="text-xs">
          {s.refresh.enabled ? describeCron(s.refresh.cron) : 'manual only'}, stale after {s.refresh.staleAfterHours} h
        </dd>
        <dt className="text-ink-muted">Data</dt>
        <dd className="text-xs">
          {index.data ? (
            <CacheStatusLine index={index.data} type={type} onApplyHeld={i.enabled ? () => setApplyHeld(true) : undefined} />
          ) : (
            <ErrorNotice error={index.error} />
          )}
        </dd>
      </dl>
      <div className="mt-3">
        <Button small icon={RefreshCw} busy={refresher.isPending} disabled={!i.enabled} onClick={() => refresher.mutate()}>
          Refresh now
        </Button>
      </div>
      {applyHeld && (
        <ConfirmDialog
          title={`Apply held changes · ${i.name}`}
          confirmLabel="Apply held changes"
          danger
          onClose={() => setApplyHeld(false)}
          onConfirm={async () => {
            const job = await refreshIntegration(i.id, { allowChanges: true });
            await queued(job, 'Refresh with the held changes queued.');
          }}
        >
          <p>
            The last refresh got far fewer rows from {PROVIDER_NAMES[type]} than Bunkarr holds (or none), so the refresh guard kept the old ones. Apply it
            only if {PROVIDER_NAMES[type]}&apos;s history or requests really shrank; if it was starting up, use &quot;Refresh now&quot; instead.
          </p>
        </ConfirmDialog>
      )}
    </article>
  );
}

function PlexIndexCard({ integration: p, onEdit }: { integration: Integration; onEdit: () => void }) {
  const ix = plexIndex(p);
  const index = useQuery({ queryKey: indexKey(p.id), queryFn: () => getIndex(p.id), refetchInterval: 30_000, enabled: ix.enabled });
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);
  const queued = useQueued(setNotice);
  const refresher = useMutation({
    mutationFn: () => refreshIntegration(p.id),
    onSuccess: (job) => queued(job, 'Refresh queued.'),
    onError: (e) => setNotice({ tone: 'error', body: errorMessage(e) }),
  });
  return (
    <article aria-label={`Library index · ${p.name}`} className="rounded border border-line bg-panel p-4">
      <div className="mb-2 flex items-start justify-between gap-2">
        <h3 className="flex flex-wrap items-center gap-2 font-medium">
          <Library className="h-4 w-4 text-ink-muted" aria-hidden="true" />
          {p.name}
          <Badge tone="info">Plex library index</Badge>
          {ix.enabled ? <Badge tone="ok">On</Badge> : <Badge>Off</Badge>}
        </h3>
        <IconButton label={`Library index settings · ${p.name}`} icon={Pencil} onClick={onEdit} />
      </div>
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      {ix.enabled ? (
        <>
          <dl className="grid grid-cols-[8rem_1fr] gap-x-3 gap-y-1 text-sm">
            <dt className="text-ink-muted">Refresh</dt>
            <dd className="text-xs">
              {describeCron(ix.cron)}, stale after {ix.staleAfterHours} h
            </dd>
            <dt className="text-ink-muted">Index</dt>
            <dd className="text-xs">{index.data ? <CacheStatusLine index={index.data} type="plex" /> : <ErrorNotice error={index.error} />}</dd>
          </dl>
          <div className="mt-3">
            <Button small icon={RefreshCw} busy={refresher.isPending} disabled={!p.enabled} onClick={() => refresher.mutate()}>
              Refresh now
            </Button>
          </div>
        </>
      ) : (
        <p className="text-sm text-ink-muted">
          Off. Turn it on to link Tautulli or Maintainerr to this server, or to use its libraries (plex.section) and added dates in the tier rules.
        </p>
      )}
    </article>
  );
}

/** PlexIndexForm turns a Plex server's library index on or off and sets its schedule. */
function PlexIndexForm({ integration, onClose }: { integration: Integration; onClose: () => void }) {
  const qc = useQueryClient();
  const ix = plexIndex(integration);
  const [schedule, setSchedule] = useState<CronSchedule>({ cron: ix.cron, enabled: true });
  const [enabled, setEnabled] = useState(ix.enabled);
  const [staleAfter, setStaleAfter] = useState(ix.staleAfterHours);
  const [formError, setFormError] = useState<string | null>(null);
  const saver = useMutation({
    mutationFn: (index: PlexIndexSettings) => savePlexIndex(integration, index),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: keys.integrations });
      await qc.invalidateQueries({ queryKey: keys.schedules });
      await qc.invalidateQueries({ queryKey: indexKey(integration.id) });
      onClose();
    },
  });
  function submit(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    const cron = schedule.cron.trim() || DEFAULT_PLEX_INDEX.cron;
    const cronError = validateCron(cron);
    if (cronError) {
      setFormError(`The refresh schedule is invalid: ${cronError}`);
      return;
    }
    if (!Number.isInteger(staleAfter) || staleAfter < 1 || staleAfter > 720) {
      setFormError('"Stale after" must be 1 to 720 hours.');
      return;
    }
    saver.mutate({ enabled, cron, staleAfterHours: staleAfter });
  }
  return (
    <Modal
      title={`Library index · ${integration.name}`}
      onClose={onClose}
      footer={
        <>
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="plex-index-form" busy={saver.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="plex-index-form" onSubmit={submit} noValidate>
        {formError && <Notice tone="error">{formError}</Notice>}
        <ErrorNotice error={saver.error} />
        <FormSection
          title="Plex library index"
          description="Bunkarr reads this server's libraries, items and files (never their genres), so Tautulli's plays and Maintainerr's collections, which name Plex items, can be matched to your files."
        >
          <CheckboxField label="Index" checked={enabled} onChange={setEnabled} text="Keep an index of this server's libraries" />
          <CronInput label="Refresh" value={schedule} onChange={setSchedule} presets={REFRESH_PRESETS.plex} allowManual={false} />
          <NumberField
            label="Stale after"
            value={staleAfter}
            onChange={setStaleAfter}
            min={1}
            max={720}
            suffix="hours"
            help="Facts that need the index (Tautulli, Maintainerr, plex.section) count as unknown when its last complete refresh is older than this."
          />
        </FormSection>
      </form>
    </Modal>
  );
}

/** ProviderTestView shows a Test result, with whether the app works with the linked Plex server. */
export function ProviderTestView({ result, type }: { result: ProviderTestResult; type: ProviderType }) {
  return (
    <Notice tone={result.ok ? 'success' : 'error'} reveal revealKey={result}>
      <div>{result.message || (result.ok ? `Connected to ${PROVIDER_NAMES[type]}.` : 'Test failed.')}</div>
      {result.plexMatches === true && <div>It works with the linked Plex server.</div>}
      {result.plexMatches === false && <div>It does not work with the linked Plex server: choose the server it watches.</div>}
    </Notice>
  );
}

/**
 * ProviderForm adds or edits a Tautulli, Seerr or Maintainerr connection. The API key is
 * write-only (Maintainerr has none). Linking a Plex server whose library index is off offers to
 * turn the index on (Tautulli and Maintainerr are matched through it, design §6.3).
 */
function ProviderForm({ integration, type, plexes, onClose }: { integration: Integration | null; type: ProviderType; plexes: Integration[]; onClose: () => void }) {
  const qc = useQueryClient();
  const app = PROVIDER_NAMES[type];
  const initial = providerSettings(integration, type);
  const [name, setName] = useState(integration?.name ?? app);
  const [url, setUrl] = useState(integration?.url ?? '');
  const [apiKey, setApiKey] = useState('');
  const [enabled, setEnabled] = useState(integration?.enabled ?? true);
  const defaultPlex = initial.plexIntegrationId || (NEEDS_PLEX[type] && plexes.length === 1 ? plexes[0].id : 0);
  const [plexId, setPlexId] = useState(String(defaultPlex));
  const [schedule, setSchedule] = useState<CronSchedule>({ cron: initial.refresh.cron, enabled: initial.refresh.enabled });
  const [staleAfter, setStaleAfter] = useState(initial.refresh.staleAfterHours);
  const [turnOnIndex, setTurnOnIndex] = useState(true);
  const [test, setTest] = useState<{ key: string; result: ProviderTestResult } | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);

  const linked = plexes.find((p) => String(p.id) === plexId) ?? null;
  const indexOff = !!linked && !plexIndex(linked).enabled;
  const testInput = (): ProviderTestInput => ({
    type,
    url: url.trim(),
    apiKey: HAS_KEY[type] ? apiKey.trim() || undefined : undefined,
    id: integration?.id,
    settings: { plexIntegrationId: Number(plexId) || 0 },
  });
  const inputKey = JSON.stringify(testInput());
  const tester = useMutation({
    mutationFn: (v: { key: string; input: ProviderTestInput }) => testProvider(v.input),
    onMutate: () => setTest(null),
    onSuccess: (result, v) => setTest({ key: v.key, result }),
  });
  const current = test?.key === inputKey ? test.result : null;
  const testError = tester.variables?.key === inputKey ? tester.error : null;
  // Once saved, Save updates that integration: a retry after the library index failed to turn on
  // must not add the connection a second time.
  const [savedId, setSavedId] = useState<number | null>(integration?.id ?? null);
  const saver = useMutation({
    mutationFn: async (v: { body: ProviderIntegrationInput; index: Integration | null }) => {
      // The connection first: a save the server refuses must not turn on the Plex library index
      // (and queue its full refresh).
      const saved = savedId ? await updateProvider(savedId, v.body) : await createProvider(v.body);
      setSavedId(saved.id);
      if (v.index) {
        try {
          await savePlexIndex(v.index, { ...plexIndex(v.index), enabled: true });
        } catch (e) {
          await qc.invalidateQueries({ queryKey: keys.integrations });
          throw new Error(
            `${app} was saved, but turning on the library index of ${v.index.name} failed (${errorMessage(e)}). Save again to retry, or turn it on in its card.`,
          );
        }
      }
      return saved;
    },
    onSuccess: async (saved) => {
      await qc.invalidateQueries({ queryKey: keys.integrations });
      await qc.invalidateQueries({ queryKey: keys.schedules });
      await qc.invalidateQueries({ queryKey: indexKey(saved.id) });
      onClose();
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    setAttempt((n) => n + 1);
    if (!name.trim() || !url.trim()) {
      setFormError(`Enter a name and ${app}'s URL.`);
      return;
    }
    if (HAS_KEY[type] && !integration && !apiKey.trim()) {
      setFormError(`Enter ${app}'s API key (${KEY_HELP[type]})`);
      return;
    }
    if (NEEDS_PLEX[type] && !linked) {
      setFormError(`Choose the Plex server ${app} ${type === 'tautulli' ? 'watches' : 'manages'}.`);
      return;
    }
    if (!Number.isInteger(staleAfter) || staleAfter < 1 || staleAfter > 720) {
      setFormError('"Stale after" must be 1 to 720 hours.');
      return;
    }
    const cron = schedule.cron.trim() || DEFAULT_REFRESH[type].cron;
    const cronError = validateCron(cron);
    if (cronError) {
      setFormError(`The refresh schedule is invalid: ${cronError}`);
      return;
    }
    const body: ProviderIntegrationInput = {
      type,
      name: name.trim(),
      url: url.trim(),
      enabled,
      settings: { ...initial, plexIntegrationId: linked?.id ?? 0, refresh: { cron, enabled: schedule.enabled, staleAfterHours: staleAfter } },
    };
    if (HAS_KEY[type] && apiKey.trim()) {
      body.apiKey = apiKey.trim();
    }
    saver.mutate({ body, index: indexOff && turnOnIndex && type !== 'seerr' ? linked : null });
  }

  const plexOptions = [
    ...(NEEDS_PLEX[type] ? (linked ? [] : [{ value: '0', label: 'Choose a Plex server' }]) : [{ value: '0', label: 'None' }]),
    ...plexes.map((p) => ({ value: String(p.id), label: p.name })),
  ];

  return (
    <Modal
      title={integration ? `Edit ${app} · ${integration.name}` : `Add ${app}`}
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button icon={FlaskConical} busy={tester.isPending} disabled={!url.trim()} onClick={() => tester.mutate({ key: inputKey, input: testInput() })}>
            Test
          </Button>
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="provider-form" busy={saver.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="provider-form" onSubmit={submit} noValidate>
        {formError && (
          <Notice key={attempt} tone="error">
            {formError}
          </Notice>
        )}
        <ErrorNotice error={saver.error ?? testError} />
        {current && <ProviderTestView result={current} type={type} />}
        <FormSection title={app} description={WHAT[type]}>
          <TextField label="Name" value={name} onChange={setName} autoFocus={!integration} />
          <TextField label="URL" type="url" value={url} onChange={setUrl} mono placeholder={URL_EXAMPLE[type]} help="How Bunkarr reaches it, including any URL base." />
          {HAS_KEY[type] ? (
            <SecretField
              label="API key"
              value={apiKey}
              onChange={setApiKey}
              stored={!!integration?.hasApiKey}
              reenter={!!integration && url.trim() !== integration.url}
              placeholder={KEY_HELP[type]}
              help="Stored encrypted and never shown again; sent only in a request header to this URL. Bunkarr only reads from the app."
            />
          ) : (
            <Notice tone="warning">{MAINTAINERR_NOTICE} Anyone who can reach Maintainerr can add items to a deleting collection, so use its facts with care.</Notice>
          )}
          <CheckboxField label="Enabled" checked={enabled} onChange={setEnabled} text={`Use this ${app}`} />
        </FormSection>
        <FormSection
          title="Plex server"
          description={
            type === 'seerr'
              ? 'Optional: the Plex server Seerr uses. Its items then serve as a fallback when a file has no TMDB or TVDB id.'
              : `The Plex server ${app} ${type === 'tautulli' ? 'watches' : 'manages'}. Its rating keys only mean something on that server, so ${app}'s data is matched to your files through its library index.`
          }
        >
          {plexes.length === 0 ? (
            <p className="text-sm text-warn">Connect a Plex server first (Settings → Plex).</p>
          ) : (
            <SelectField label="Plex server" value={plexId} onChange={setPlexId} options={plexOptions} />
          )}
          {indexOff && type !== 'seerr' && (
            <CheckboxField
              label="Library index"
              checked={turnOnIndex}
              onChange={setTurnOnIndex}
              text={`Turn on the library index of ${linked?.name} when saving`}
              help={`Without it ${app}'s facts stay unknown (and the files they cover stay fully backed up).`}
            />
          )}
        </FormSection>
        <FormSection title="Refresh" description={`Bunkarr reads ${app} completely on this schedule; a failed read keeps the previous data, which then ages into unknown.`}>
          <CronInput label="Refresh" value={schedule} onChange={setSchedule} presets={REFRESH_PRESETS[type]} />
          <NumberField
            label="Stale after"
            value={staleAfter}
            onChange={setStaleAfter}
            min={1}
            max={720}
            suffix="hours"
            help="Tier rules treat its facts as unknown (so the files stay fully backed up) when its last complete refresh is older than this."
          />
          {integration && <CacheSection integration={integration} type={type} />}
        </FormSection>
      </form>
    </Modal>
  );
}

function CacheSection({ integration, type }: { integration: Integration; type: ProviderType }) {
  const index = useQuery({ queryKey: indexKey(integration.id), queryFn: () => getIndex(integration.id) });
  return (
    <div className="pt-2 text-xs">
      <div className="mb-1 text-sm text-ink-muted">Status</div>
      {index.data ? <CacheStatusLine index={index.data} type={type} /> : <ErrorNotice error={index.error} />}
    </div>
  );
}
