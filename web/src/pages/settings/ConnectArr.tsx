import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Copy, Eye, FlaskConical, KeyRound, Pencil, Plus, RefreshCw, ShieldAlert, Trash2, X } from 'lucide-react';
import { useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { Link } from 'react-router';
import {
  ARR_NAMES,
  ARR_TYPES,
  DEFAULT_ARR_REFRESH,
  WEBHOOK_POLL_MS,
  WEBHOOK_TRIGGERS,
  arrSettings,
  createArr,
  getIndex,
  getWebhookInfo,
  isArrType,
  listUnmapped,
  refreshIntegration,
  testArr,
  updateArr,
  webhookKey,
  webhookPath,
  type ArrIntegrationInput,
  type ArrPathMapping,
  type ArrTestInput,
  type ArrTestResult,
  type ArrType,
  type IndexView,
  type RefreshSettings,
} from '@/api/arr';
import { errorMessage } from '@/api/client';
import { deleteIntegration } from '@/api/integrations';
import { updateSource } from '@/api/library';
import type { CronSchedule, Integration, Source } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CronInput } from '@/components/CronInput';
import { CheckboxField, FormRow, FormSection, NumberField, SecretField, TextField, inputClass } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Pagination } from '@/components/Pagination';
import { PathPicker } from '@/components/PathPicker';
import { Badge } from '@/components/StatusBadge';
import { DeletedIntegrations } from '@/components/tiers/DeletedIntegrations';
import { copyText } from '@/lib/clipboard';
import { describeCron, validateCron, type CronPreset } from '@/lib/cron';
import { formatBytes, formatNumber, formatRelative } from '@/lib/format';
import { keys, useIntegrations, useSources } from '@/lib/lookups';
import { ArrBackupFields, ArrBackupSummary, arrBackupSettings, arrBackupValue, validateArrBackup } from './ArrBackup';

/** ARR_REFRESH_PRESETS are the offered full-refresh schedules (the default first). */
export const ARR_REFRESH_PRESETS: CronPreset[] = [
  { label: 'Every 6 hours (at :15)', cron: '15 */6 * * *' },
  { label: 'Every 12 hours (at :15)', cron: '15 */12 * * *' },
  { label: 'Daily at 03:15', cron: '15 3 * * *' },
];

const ITEM_NOUN: Record<ArrType, string> = { radarr: 'movies', sonarr: 'series', lidarr: 'artists' };

/** ROOT_EXAMPLE is each app's usual root folder, for the path mapping examples. */
const ROOT_EXAMPLE: Record<ArrType, string> = { radarr: 'movies', sonarr: 'tv', lidarr: 'music' };

/**
 * WEBHOOK_GAPS says what an app does not send a webhook for (design D2, D8): the full refresh
 * catches those changes up.
 */
const WEBHOOK_GAPS: Partial<Record<ArrType, string>> = {
  lidarr:
    'Lidarr sends no webhook for a manual import unless “Replace existing files” is ticked, and none when a track file is deleted: the full refresh finds those changes and backs them up.',
};

export const indexKey = (id: number) => ['integrations', id, 'index'] as const;

/**
 * ArrConnections is the Sonarr, Radarr and Lidarr part of Settings → Connect: one card per
 * connection with its index status, and the form that edits it.
 */
export function ArrConnections() {
  const qc = useQueryClient();
  const integrations = useIntegrations();
  const [editing, setEditing] = useState<Integration | ArrType | null>(null);
  const [deleting, setDeleting] = useState<Integration | null>(null);
  const arrs = (integrations.data ?? []).filter((i) => isArrType(i.type));

  return (
    <section aria-labelledby="arr-heading" className="mb-8 max-w-5xl">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2 border-b border-line pb-2">
        <h2 id="arr-heading" className="text-lg">
          Sonarr, Radarr and Lidarr
        </h2>
        <div className="flex flex-wrap gap-1">
          {ARR_TYPES.map((t) => (
            <Button key={t} small variant="ghost" icon={Plus} onClick={() => setEditing(t)}>
              Add {ARR_NAMES[t]}
            </Button>
          ))}
        </div>
      </div>
      <ErrorNotice error={integrations.error} />
      {integrations.data && arrs.length === 0 && (
        <p className="text-sm text-ink-muted">
          Connect your *arr apps so Bunkarr knows their movies, series and artists: an import or upgrade is then backed up within a minute (webhooks), and
          manifests record what to download again.
        </p>
      )}
      <div className="grid gap-3 lg:grid-cols-2">
        {arrs.map((i) => (
          <ArrCard key={i.id} integration={i} onEdit={() => setEditing(i)} onDelete={() => setDeleting(i)} />
        ))}
      </div>
      <div className="mt-3">
        <DeletedIntegrations />
      </div>
      {editing && (
        <ArrForm
          integration={typeof editing === 'string' ? null : editing}
          type={typeof editing === 'string' ? editing : (editing.type as ArrType)}
          onClose={() => setEditing(null)}
        />
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${ARR_NAMES[deleting.type as ArrType]}`}
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
            Delete <strong>{deleting.name}</strong>? Its stored API key, webhook key, index and schedules are removed; webhooks it sends are refused from
            then on.
          </p>
          <p className="text-ink-muted">
            Files already backed up stay on your destinations. Files in its folders that no other *arr manages stay fully backed up until you confirm the
            removal (offered here after the delete, and in Settings → Tiers).
          </p>
        </ConfirmDialog>
      )}
    </section>
  );
}

function ArrCard({ integration: i, onEdit, onDelete }: { integration: Integration; onEdit: () => void; onDelete: () => void }) {
  const qc = useQueryClient();
  const type = i.type as ArrType;
  const s = arrSettings(i);
  const index = useQuery({ queryKey: indexKey(i.id), queryFn: () => getIndex(i.id), refetchInterval: 30_000 });
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);
  const [unmapped, setUnmapped] = useState(false);
  const [applyHeld, setApplyHeld] = useState(false);
  const queued = async (job: { id: number }, what: string) => {
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
  const refresher = useMutation({
    mutationFn: () => refreshIntegration(i.id),
    onSuccess: (job) => queued(job, 'Refresh queued.'),
    onError: (e) => setNotice({ tone: 'error', body: errorMessage(e) }),
  });
  return (
    <article aria-label={i.name} className="rounded border border-line bg-panel p-4">
      <div className="mb-2 flex items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="flex flex-wrap items-center gap-2 font-medium">
            {i.name}
            <Badge tone="info">{ARR_NAMES[type]}</Badge>
            {!i.enabled && <Badge>Disabled</Badge>}
            {i.hasApiKey ? <Badge tone="ok">Key stored</Badge> : <Badge tone="warn">No API key</Badge>}
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
        <dt className="text-ink-muted">Path mappings</dt>
        <dd className="text-xs">
          {s.pathMappings.length === 0 ? (
            <span className="text-warn">none: add one so Bunkarr finds the files</span>
          ) : (
            s.pathMappings.map((m) => (
              <div key={`${m.arr}>${m.local}`} className="break-all font-mono">
                {m.arr} → {m.local}
              </div>
            ))
          )}
        </dd>
        <dt className="text-ink-muted">Refresh</dt>
        <dd className="text-xs">
          {s.refresh.enabled ? describeCron(s.refresh.cron) : 'manual only'}, stale after {s.refresh.staleAfterHours} h
        </dd>
        <dt className="text-ink-muted">Index</dt>
        <dd className="text-xs">
          {index.data ? (
            <IndexStatusLine
              index={index.data}
              type={type}
              onUnmapped={() => setUnmapped(true)}
              onApplyHeld={i.enabled ? () => setApplyHeld(true) : undefined}
            />
          ) : (
            <ErrorNotice error={index.error} />
          )}
        </dd>
        <ArrBackupSummary integration={i} onNotice={setNotice} />
      </dl>
      <div className="mt-3">
        <Button small icon={RefreshCw} busy={refresher.isPending} disabled={!i.enabled} onClick={() => refresher.mutate()}>
          Refresh now
        </Button>
      </div>
      {unmapped && <UnmappedDialog integration={i} onClose={() => setUnmapped(false)} />}
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
            The last full refresh found far fewer {ITEM_NOUN[type]} or files in {ARR_NAMES[type]} than the index holds, so the refresh guard kept them
            in the index. Until a refresh applies that, the index stays stale and its facts count as unknown.
          </p>
          <p>
            Apply it only if {ARR_NAMES[type]} really lost them (you deleted them, or moved them to another app). If a share is not mounted in{' '}
            {ARR_NAMES[type]} or it answered with an empty list, fix that and use &quot;Refresh now&quot; instead.
          </p>
          <p className="text-ink-muted">This runs a full refresh that removes what {ARR_NAMES[type]} no longer lists. Files already backed up stay on your destinations.</p>
        </ConfirmDialog>
      )}
    </article>
  );
}

/** IndexStatusLine says whether the index is fresh, and what it could not map. */
export function IndexStatusLine({
  index,
  type,
  onUnmapped,
  onApplyHeld,
}: {
  index: IndexView;
  type: ArrType;
  onUnmapped?: () => void;
  /** Offers "Apply held changes" when the refresh guard held removals (a refresh with allowChanges). */
  onApplyHeld?: () => void;
}) {
  const st = index.stats ?? {};
  const off = (st.filesUnmapped ?? 0) + (st.filesMismatched ?? 0);
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
        {st.items !== undefined && (
          <span>
            {formatNumber(st.items)} {ITEM_NOUN[type]}, {formatNumber(st.files ?? 0)} files
          </span>
        )}
      </div>
      {!index.fresh && index.reason && <div className="text-warn">{index.reason}. Tier rules treat its facts as unknown.</div>}
      {index.status === 'failed' && index.error && <div className="text-danger">Last refresh failed: {index.error}</div>}
      {index.status === 'ok' && index.error && <div className="text-warn">{index.error}</div>}
      {st.guardHeld && (
        <div className="text-warn">
          The index keeps what {ARR_NAMES[type]} no longer lists, and stays stale, until a refresh applies the held changes.{' '}
          {onApplyHeld && (
            <Button small variant="ghost" icon={ShieldAlert} onClick={onApplyHeld}>
              Apply held changes
            </Button>
          )}
        </div>
      )}
      {off > 0 && (
        <div className="text-warn">
          {formatNumber(st.filesUnmapped ?? 0)} unmapped, {formatNumber(st.filesMismatched ?? 0)} mismatched file(s){' '}
          {onUnmapped && (
            <Button small variant="ghost" onClick={onUnmapped}>
              Show
            </Button>
          )}
        </div>
      )}
      {(st.inaccessibleRootFolders ?? []).length > 0 && (
        <div className="text-warn">Not accessible in {ARR_NAMES[type]}: {(st.inaccessibleRootFolders ?? []).join(', ')}</div>
      )}
      {st.recycleBin && st.recycleBin.sourceId !== null && !st.recycleBin.excluded && (
        <div className="text-warn">The recycle bin {st.recycleBin.path} is inside a source: edit the connection and test it to exclude it.</div>
      )}
      {st.fileDate && st.fileDate !== 'none' && <div className="text-warn">Change File Date is “{st.fileDate}”: set it to None in {ARR_NAMES[type]}.</div>}
    </div>
  );
}

const REASONS: Record<string, string> = {
  unmapped: 'No path mapping',
  'no-source': 'In no source',
  mismatched: 'No matching file in the catalog',
};

/** UnmappedDialog lists the *arr's files that do not apply to a catalog file. */
function UnmappedDialog({ integration, onClose }: { integration: Integration; onClose: () => void }) {
  const [page, setPage] = useState(1);
  const pageSize = 50;
  const list = useQuery({ queryKey: ['catalog', 'unmapped', integration.id, page], queryFn: () => listUnmapped(integration.id, page, pageSize) });
  return (
    <Modal title={`Unmapped files · ${integration.name}`} size="xl" onClose={onClose}>
      <p className="mb-3 text-sm text-ink-muted">
        These files supply no facts to tier rules and are not scan targets: add or fix a path mapping, or scan the source so the catalog knows them.
      </p>
      <ErrorNotice error={list.error} />
      {list.data && list.data.records.length === 0 && <p className="text-sm">Every file maps to a catalog file.</p>}
      <ul className="space-y-2 text-sm" aria-label="Unmapped files">
        {list.data?.records.map((f) => (
          <li key={`${f.path}#${f.reason}`} className="rounded border border-line p-2">
            <div className="break-all font-mono text-xs">{f.path}</div>
            {f.localPath && <div className="break-all font-mono text-xs text-ink-muted">→ {f.localPath}</div>}
            <div className="text-xs">
              {REASONS[f.reason] ?? f.reason} · {formatBytes(f.size)}
              {f.catalogSize !== undefined && <> (the catalog has {formatBytes(f.catalogSize)})</>}
            </div>
          </li>
        ))}
      </ul>
      {list.data && <Pagination page={page} pageSize={pageSize} total={list.data.totalRecords} onPage={setPage} />}
    </Modal>
  );
}

function MappingsEditor({
  app,
  root = 'movies',
  value,
  onChange,
}: {
  app: string;
  /** root is the example root folder's name ("music": /music → /media/music). */
  root?: string;
  value: ArrPathMapping[];
  onChange: (v: ArrPathMapping[]) => void;
}) {
  const set = (i: number, patch: Partial<ArrPathMapping>) => onChange(value.map((m, j) => (j === i ? { ...m, ...patch } : m)));
  return (
    <FormRow
      label="Path mappings"
      group
      help={`${app} reports its paths as it sees them inside its container. Map each of its root folders to where Bunkarr sees the same folder, for example /${root} → /media/${root}. Test checks every root folder.`}
    >
      <div className="space-y-2">
        {value.map((m, i) => (
          <div key={i} className="flex flex-col gap-2 rounded border border-line p-2 sm:flex-row sm:items-center sm:border-0 sm:p-0">
            <input
              aria-label={`${app} path ${i + 1}`}
              className={`${inputClass} font-mono sm:w-2/5`}
              value={m.arr}
              placeholder={`/${root}`}
              spellCheck={false}
              onChange={(e) => set(i, { arr: e.target.value })}
            />
            <span className="hidden text-ink-muted sm:inline" aria-hidden="true">
              →
            </span>
            <div className="min-w-0 flex-1">
              <PathPicker label={`Bunkarr path ${i + 1}`} value={m.local} placeholder={`/media/${root}`} onChange={(local) => set(i, { local })} />
            </div>
            <IconButton label={`Remove mapping ${i + 1}`} icon={X} onClick={() => onChange(value.filter((_, j) => j !== i))} />
          </div>
        ))}
        <Button small icon={Plus} onClick={() => onChange([...value, { arr: '', local: '' }])}>
          Add mapping
        </Button>
      </div>
    </FormRow>
  );
}

/**
 * ArrForm adds or edits a Sonarr, Radarr or Lidarr connection. The API key is write-only. Settings
 * this form does not edit (the backup ones) are sent back as they were.
 */
function ArrForm({ integration, type, onClose }: { integration: Integration | null; type: ArrType; onClose: () => void }) {
  const qc = useQueryClient();
  const app = ARR_NAMES[type];
  const initial = arrSettings(integration);
  const [name, setName] = useState(integration?.name ?? app);
  const [url, setUrl] = useState(integration?.url ?? '');
  const [apiKey, setApiKey] = useState('');
  const [enabled, setEnabled] = useState(integration?.enabled ?? true);
  const [mappings, setMappings] = useState<ArrPathMapping[]>(initial.pathMappings.length ? initial.pathMappings : [{ arr: '', local: '' }]);
  const [schedule, setSchedule] = useState<CronSchedule>({ cron: initial.refresh.cron, enabled: initial.refresh.enabled });
  const [staleAfter, setStaleAfter] = useState(initial.refresh.staleAfterHours);
  const [backup, setBackup] = useState(() => arrBackupValue(integration));
  // The last Test result with the inputs it tested: it speaks only for those (a changed URL, key,
  // mapping or backup folder hides it until the next Test).
  const [test, setTest] = useState<{ key: string; result: ArrTestResult } | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState(0);

  const cleanMappings = () => mappings.map((m) => ({ arr: m.arr.trim(), local: m.local.trim() })).filter((m) => m.arr || m.local);
  const testInput = (): ArrTestInput => ({
    type,
    url: url.trim(),
    apiKey: apiKey.trim() || undefined,
    id: integration?.id,
    settings: { pathMappings: cleanMappings(), ...(backup.backupFolder.trim() ? { backupFolder: backup.backupFolder.trim() } : {}) },
  });
  const inputKey = JSON.stringify(testInput());

  const tester = useMutation({
    mutationFn: (v: { key: string; input: ArrTestInput }) => testArr(v.input),
    onMutate: () => setTest(null),
    onSuccess: (result, v) => setTest({ key: v.key, result }),
  });
  const current = test?.key === inputKey ? test.result : null;
  const testError = tester.variables?.key === inputKey ? tester.error : null;
  const saver = useMutation({
    mutationFn: (body: ArrIntegrationInput) => (integration ? updateArr(integration.id, body) : createArr(body)),
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
    if (!integration && !apiKey.trim()) {
      setFormError(`Enter ${app}'s API key (${app} → Settings → General).`);
      return;
    }
    const pathMappings = cleanMappings();
    if (pathMappings.some((m) => !m.arr || !m.local)) {
      setFormError(`Every path mapping needs both a ${app} path and a Bunkarr path.`);
      return;
    }
    if (!Number.isInteger(staleAfter) || staleAfter < 1 || staleAfter > 720) {
      setFormError('"Stale after" must be 1 to 720 hours.');
      return;
    }
    const cron = schedule.cron.trim() || DEFAULT_ARR_REFRESH.cron;
    const cronError = validateCron(cron);
    if (cronError) {
      setFormError(`The refresh schedule is invalid: ${cronError}`);
      return;
    }
    const refresh: RefreshSettings = { cron, enabled: schedule.enabled, staleAfterHours: staleAfter };
    const backupError = validateArrBackup(backup);
    if (backupError) {
      setFormError(backupError);
      return;
    }
    const body: ArrIntegrationInput = {
      type,
      name: name.trim(),
      url: url.trim(),
      enabled,
      settings: { ...initial, pathMappings, refresh, ...arrBackupSettings(backup) },
    };
    if (apiKey.trim()) {
      body.apiKey = apiKey.trim();
    }
    saver.mutate(body);
  }

  return (
    <Modal
      title={integration ? `Edit ${app} · ${integration.name}` : `Add ${app}`}
      size="xl"
      onClose={onClose}
      footer={
        <>
          <Button icon={FlaskConical} busy={tester.isPending} disabled={!url.trim()} onClick={() => tester.mutate({ key: inputKey, input: testInput() })}>
            Test
          </Button>
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="arr-form" busy={saver.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="arr-form" onSubmit={submit} noValidate>
        {formError && (
          <Notice key={attempt} tone="error">
            {formError}
          </Notice>
        )}
        <ErrorNotice error={saver.error ?? testError} />
        {current && <ArrTestView result={current} app={app} />}
        <FormSection title={app}>
          <TextField label="Name" value={name} onChange={setName} autoFocus={!integration} />
          <TextField
            label="URL"
            type="url"
            value={url}
            onChange={setUrl}
            mono
            placeholder={type === 'radarr' ? 'http://radarr:7878' : type === 'sonarr' ? 'http://sonarr:8989' : 'http://lidarr:8686'}
            help="How Bunkarr reaches it, including any URL base (for example http://192.168.1.10:7878/radarr)."
          />
          <SecretField
            label="API key"
            value={apiKey}
            onChange={setApiKey}
            stored={!!integration?.hasApiKey}
            reenter={!!integration && url.trim() !== integration.url}
            placeholder={`${app} → Settings → General → API Key`}
            help="Stored encrypted and never shown again; sent only in a request header to this URL. Bunkarr only reads from the app (and asks it to make its own backups)."
          />
          <CheckboxField label="Enabled" checked={enabled} onChange={setEnabled} text={`Use this ${app}`} />
        </FormSection>
        <FormSection title="Paths">
          <MappingsEditor app={app} root={ROOT_EXAMPLE[type]} value={mappings} onChange={setMappings} />
        </FormSection>
        <FormSection
          title="Index"
          description={`Bunkarr keeps an index of ${app}'s ${ITEM_NOUN[type]} and files. A full refresh also catches up on changes that sent no webhook, and queues the syncs they need.`}
        >
          <CronInput label="Full refresh" value={schedule} onChange={setSchedule} presets={ARR_REFRESH_PRESETS} />
          <NumberField
            label="Stale after"
            value={staleAfter}
            onChange={setStaleAfter}
            min={1}
            max={720}
            suffix="hours"
            help="Tier rules treat the index's facts as unknown (so the files stay fully backed up) when its last complete refresh is older than this."
          />
          {integration && <IndexSection integration={integration} type={type} />}
        </FormSection>
        <ArrBackupFields type={type} value={backup} onChange={setBackup} test={current} />
        {integration ? (
          <WebhookPanel integration={integration} type={type} />
        ) : (
          <FormSection title="Webhook">
            <p className="text-sm text-ink-muted">Save first: the webhook URL and its key are shown here once {app} is connected.</p>
          </FormSection>
        )}
      </form>
    </Modal>
  );
}

function IndexSection({ integration, type }: { integration: Integration; type: ArrType }) {
  const index = useQuery({ queryKey: indexKey(integration.id), queryFn: () => getIndex(integration.id) });
  return (
    <FormRow label="Status">
      <div className="pt-2 text-xs">{index.data ? <IndexStatusLine index={index.data} type={type} /> : <ErrorNotice error={index.error} />}</div>
    </FormRow>
  );
}

/** ArrTestView shows a Test result: the connection, each root folder as Bunkarr sees it, and warnings. */
export function ArrTestView({ result, app }: { result: ArrTestResult; app: string }) {
  const sources = useSources();
  const sourceName = (id: number | null) => (id === null ? '' : (sources.data?.find((s) => s.id === id)?.name ?? `source #${id}`));
  return (
    <div className="mb-4 space-y-2">
      <Notice tone={result.ok ? 'success' : 'error'} reveal revealKey={result}>
        {result.message}
      </Notice>
      {result.rootFolders && result.rootFolders.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs" aria-label="Root folders">
            <thead className="text-ink-muted">
              <tr>
                <th className="py-1 pr-2 font-medium">{app} root folder</th>
                <th className="py-1 pr-2 font-medium">Bunkarr path</th>
                <th className="py-1 pr-2 font-medium">Source</th>
              </tr>
            </thead>
            <tbody>
              {result.rootFolders.map((rf) => (
                <tr key={rf.path} className="border-t border-line align-top">
                  <td className="py-1 pr-2 font-mono break-all">
                    {rf.path}
                    {!rf.accessible && (
                      <span className="ml-1">
                        <Badge tone="warn">Not accessible</Badge>
                      </span>
                    )}
                  </td>
                  <td className="py-1 pr-2 font-mono break-all">{rf.localPath ?? '—'}</td>
                  <td className="py-1 pr-2">
                    {rf.sourceId !== null ? sourceName(rf.sourceId) : '—'}
                    {rf.reason && <div className="text-warn">{rf.reason}</div>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {result.recycleBin && <RecycleBinWarning bin={result.recycleBin} app={app} sources={sources.data ?? []} />}
      {result.fileDate && result.fileDate !== 'none' && (
        <Notice tone="warning">
          {app} changes each file&apos;s date (Change File Date: {result.fileDate}): every version of a title then has the same modification time, so an
          equal-size replacement looks unchanged. Set it to None in {app} → Settings → Media Management.
        </Notice>
      )}
    </div>
  );
}

/** RecycleBinWarning offers to exclude the *arr's recycle bin from the source that contains it. */
function RecycleBinWarning({ bin, app, sources }: { bin: NonNullable<ArrTestResult['recycleBin']>; app: string; sources: Source[] }) {
  const qc = useQueryClient();
  const [done, setDone] = useState(bin.excluded);
  const src = sources.find((s) => s.id === bin.sourceId);
  const pattern = `/${bin.relPath}/`;
  const exclude = useMutation({
    mutationFn: async () => {
      if (!src) {
        throw new Error('The source is not loaded yet.');
      }
      await updateSource(src.id, {
        name: src.name,
        path: src.path,
        destFolder: src.destFolder,
        exclude: [...src.exclude, pattern],
        enabled: src.enabled,
        plexIntegrationId: src.plexIntegrationId,
        plexSectionId: src.plexSectionId,
        plexPath: src.plexPath,
        arrIntegrationId: src.arrIntegrationId,
      });
    },
    onSuccess: async () => {
      setDone(true);
      await qc.invalidateQueries({ queryKey: keys.sources });
    },
  });
  if (bin.sourceId === null || !bin.relPath) {
    return null;
  }
  if (done) {
    return <Notice tone="success">{app}&apos;s recycle bin is excluded from {src?.name ?? 'its source'}.</Notice>;
  }
  return (
    <Notice tone="warning">
      {app}&apos;s recycle bin {bin.path} is inside {src ? <strong>{src.name}</strong> : 'a source'}, which does not exclude it: every upgraded file would be
      backed up twice.
      <div className="mt-2">
        <Button small busy={exclude.isPending} disabled={!src} onClick={() => exclude.mutate()}>
          Exclude {pattern} from {src?.name ?? 'the source'}
        </Button>
      </div>
      <ErrorNotice error={exclude.error} className="mt-2" />
    </Notice>
  );
}

/**
 * CopyButton copies text (lib/clipboard falls back to execCommand over plain http) and says what
 * happened in a live region: "Copied", or to select the text and copy it by hand.
 */
export function CopyButton({ text, label }: { text: string; label: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  useEffect(() => {
    if (state !== 'copied') return;
    const t = window.setTimeout(() => setState('idle'), 1500);
    return () => window.clearTimeout(t);
  }, [state]);
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <Button
        small
        variant="ghost"
        icon={Copy}
        aria-label={label}
        onClick={() => {
          void copyText(text).then((ok) => setState(ok ? 'copied' : 'failed'));
        }}
      >
        {state === 'copied' ? 'Copied' : 'Copy'}
      </Button>
      <span role="status" className={state === 'failed' ? 'text-xs text-warn' : 'sr-only'}>
        {state === 'copied' ? 'Copied.' : state === 'failed' ? 'Your browser did not allow copying: select the text and copy it by hand.' : ''}
      </span>
    </span>
  );
}

/**
 * webhookBaseProblem says why base is not an address an *arr can send webhooks to: not an http(s)
 * URL, or a loopback name (inside the *arr's container, localhost is the *arr itself).
 */
export function webhookBaseProblem(base: string, app: string): string | null {
  let u: URL;
  try {
    u = new URL(base.trim());
  } catch {
    return `Enter the address ${app} reaches Bunkarr at, for example http://bunkarr:8787.`;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') {
    return `Enter an http:// or https:// address, for example http://bunkarr:8787.`;
  }
  const host = u.hostname.replace(/^\[|\]$/g, '').toLowerCase();
  if (host === 'localhost' || host.endsWith('.localhost') || /^127\./.test(host) || host === '::1' || host === '0.0.0.0') {
    return `${u.hostname} is a loopback address: in ${app} (another container, or another machine) it does not reach Bunkarr. Enter the name or IP address ${app} reaches Bunkarr at, for example http://bunkarr:${u.port || '8787'}.`;
  }
  return null;
}

/**
 * WebhookPanel shows what to paste into the app's webhook connection: the URL, the auth (Basic
 * first, the webhook key as the password), the key on request, and the triggers to tick; plus
 * the webhook activity when the server reports it (GET /integrations/{id}/webhook).
 */
export function WebhookPanel({ integration, type }: { integration: Integration; type: ArrType }) {
  const app = ARR_NAMES[type];
  // Polled while the panel is open: the user presses Test in the *arr and comes back to see it.
  const info = useQuery({
    queryKey: ['integrations', integration.id, 'webhook'],
    queryFn: () => getWebhookInfo(integration.id),
    refetchInterval: (q) => (q.state.data === null ? false : WEBHOOK_POLL_MS),
  });
  const [checking, setChecking] = useState(false);
  const [key, setKey] = useState<string | null>(null);
  const [rotating, setRotating] = useState(false);
  const reveal = useMutation({ mutationFn: () => webhookKey(integration.id, false), onSuccess: setKey });
  // How the *arr reaches Bunkarr: this page's address unless the user changes it (the *arr runs
  // elsewhere, or this page was opened through localhost or a proxy).
  const [base, setBase] = useState(() => window.location.origin);
  const baseProblem = webhookBaseProblem(base, app);
  const url = `${base.trim().replace(/\/+$/, '')}${info.data?.path ?? webhookPath(type, integration.id)}`;
  const data = info.data;
  return (
    <FormSection title="Webhook" description={`In ${app}: Settings → Connect → + → Webhook. A webhook makes Bunkarr back up an import within a minute.`}>
      <TextField
        label="Bunkarr address"
        value={base}
        onChange={setBase}
        mono
        placeholder="http://bunkarr:8787"
        help={
          <>
            How {app} reaches Bunkarr; this page&apos;s address by default. It only builds the URL below, nothing is saved.
            {baseProblem && (
              <span className="mt-1 block text-warn" data-testid="webhook-base-problem">
                {baseProblem}
              </span>
            )}
          </>
        }
      />
      <FormRow label="URL">
        <div className="flex items-center gap-2">
          <code className="min-w-0 flex-1 break-all rounded border border-line bg-page px-2 py-1 text-xs" data-testid="webhook-url">
            {url}
          </code>
          <CopyButton text={url} label="Copy the webhook URL" />
        </div>
        <div className="mt-1 text-xs text-ink-muted">Method POST. The URL names this connection; the key authenticates it.</div>
      </FormRow>
      <FormRow label="Authentication" group>
        <ol className="list-decimal space-y-1 pl-4 pt-2 text-sm">
          <li>
            <strong>Username</strong> anything (for example <code>bunkarr</code>), <strong>Password</strong> the webhook key. Recommended: {app} keeps the
            password private.
          </li>
          <li>
            Or a header <code>X-Api-Key</code> with the webhook key.
          </li>
        </ol>
        <p className="mt-1 text-xs text-ink-muted">
          The webhook key works only for this webhook. Bunkarr&apos;s own API key is refused here, so {app} and its backups never hold an admin key.
        </p>
      </FormRow>
      <FormRow label="Webhook key">
        {key ? (
          <div className="flex items-center gap-2">
            <input aria-label="Webhook key" readOnly className={`${inputClass} font-mono`} value={key} onFocus={(e) => e.currentTarget.select()} />
            <CopyButton text={key} label="Copy the webhook key" />
          </div>
        ) : (
          <div className="flex flex-wrap gap-2 pt-1">
            <Button small icon={Eye} busy={reveal.isPending} onClick={() => reveal.mutate()}>
              Show key
            </Button>
          </div>
        )}
        <div className="mt-2">
          <Button small variant="ghost" icon={KeyRound} onClick={() => setRotating(true)}>
            Regenerate key
          </Button>
        </div>
        <ErrorNotice error={reveal.error} className="mt-2" />
      </FormRow>
      <FormRow label="Triggers" group>
        <ul className="grid gap-x-4 pt-2 text-sm sm:grid-cols-2" aria-label="Triggers to tick">
          {WEBHOOK_TRIGGERS[type].map((t) => (
            <li key={t}>{t}</li>
          ))}
        </ul>
        {WEBHOOK_GAPS[type] && <p className="mt-1 text-xs text-ink-muted">{WEBHOOK_GAPS[type]}</p>}
      </FormRow>
      {data && (
        <FormRow label="Activity">
          <div className="space-y-1 pt-2 text-sm">
            <div className="flex flex-wrap items-center gap-2">
              <span>Last Test received: {data.lastTestAt ? formatRelative(data.lastTestAt) : 'never (press Test in the webhook settings)'}</span>
              <Button
                small
                variant="ghost"
                icon={RefreshCw}
                busy={checking}
                onClick={() => {
                  setChecking(true);
                  void info.refetch().finally(() => setChecking(false));
                }}
              >
                Check again
              </Button>
            </div>
            <div>
              Last event: {data.lastEventAt ? formatRelative(data.lastEventAt) : 'none yet'} · {formatNumber(data.last24h)} in the last 24 h
            </div>
            {data.warnings.map((w) => (
              <Notice key={w} tone="warning">
                {w}
              </Notice>
            ))}
            {data.recent.length > 0 && (
              <ul className="space-y-1 text-xs" aria-label="Recent webhook events">
                {data.recent.map((ev) => (
                  <li key={ev.id} className="flex flex-wrap gap-2">
                    <span className="font-medium">{ev.eventType}</span>
                    {ev.summary?.title && <span>{ev.summary.title}</span>}
                    <span className="text-ink-muted">{formatRelative(ev.receivedAt)}</span>
                    {ev.outcome && <Badge>{ev.outcome}</Badge>}
                    {ev.jobId && (
                      <Link className="text-accent hover:underline" to={`/activity/jobs/${ev.jobId}`}>
                        job #{ev.jobId}
                      </Link>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </div>
        </FormRow>
      )}
      {rotating && (
        <ConfirmDialog
          title="Regenerate the webhook key"
          confirmLabel="Regenerate"
          danger
          onClose={() => setRotating(false)}
          onConfirm={async () => {
            setKey(await webhookKey(integration.id, true));
          }}
        >
          <p>
            The current key stops working at once: until you paste the new key into {app}&apos;s webhook, its events are refused (the next full refresh
            catches up).
          </p>
        </ConfirmDialog>
      )}
    </FormSection>
  );
}
