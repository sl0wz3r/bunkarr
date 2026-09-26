import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { RefreshCw, Search } from 'lucide-react';
import { useEffect, useState } from 'react';
import { Link } from 'react-router';
import { ApiError } from '@/api/client';
import {
  listPreviewItems,
  TIERS,
  type Tier,
  type TierCount,
  type TierDestinationPreview,
  type TierField,
  type TierItemState,
  type TierPreview,
  type TierPreviewItem,
} from '@/api/tiers';
import { Button } from '@/components/Button';
import { DataTable, type Column } from '@/components/DataTable';
import { inputClass } from '@/components/Form';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Stat, StatGrid } from '@/components/Page';
import { Pagination } from '@/components/Pagination';
import { Badge, type Tone } from '@/components/StatusBadge';
import { formatBytes, formatDateTime, formatNumber } from '@/lib/format';
import { ConfirmRemovalButton, useDeletedIntegrations } from './DeletedIntegrations';
import { ReasonList, TierBadge, UnknownPromotedBadge } from './TierBadge';
import { decisionRuleText, positionText, TIER_LABELS } from './tierText';

export const PREVIEW_PAGE_SIZE = 50;
const SEARCH_DELAY_MS = 300;

export const STATE_LABELS: Record<TierItemState, { label: string; tone: Tone; help: string }> = {
  stored: { label: 'Stored', tone: 'ok', help: 'Full, and the destination holds an up-to-date copy.' },
  'to-copy': { label: 'To copy', tone: 'info', help: 'Full, and the next sync copies it.' },
  kept: { label: 'Kept', tone: 'warn', help: 'No longer full, but the copy the destination holds is kept until you release it.' },
  'not-copied': { label: 'Not copied', tone: 'muted', help: 'Not full and never copied: only listed (manifest) or left out (skip).' },
};

/** countText is "1,234 files · 5.6 GiB". */
function countText(c: TierCount | undefined): string {
  const files = c?.files ?? 0;
  return `${formatNumber(files)} ${files === 1 ? 'file' : 'files'} · ${formatBytes(c?.bytes ?? 0)}`;
}

/**
 * PreviewPanel shows "what gets backed up and why": per destination the stored, full, manifest,
 * skip, unknown-promoted, to-copy, kept and moved-to-non-full counts with bytes, the counts per
 * rule and the config backups (always full); banners for unknown sources (a deleted *arr
 * integration's with "Confirm removal") and stale references; and the preview's files with their
 * tier, deciding rule and reasons.
 */
export function PreviewPanel({
  preview,
  fields,
  outdated,
  onRerun,
  onRemovalConfirmed,
}: {
  preview: TierPreview;
  fields: Map<string, TierField>;
  /** The rules changed since this preview was computed. */
  outdated: boolean;
  onRerun: () => void;
  /** Called after the removal of a deleted *arr integration is confirmed (the preview is out of date). */
  onRemovalConfirmed?: () => Promise<void> | void;
}) {
  const dests = preview.destinations ?? [];
  const saved = typeof preview.revision === 'number';
  const unknownSources = preview.unknownSources ?? [];
  // An unknown source of a deleted *arr integration is confirmed here (integration ids are never reused).
  const deleted = useDeletedIntegrations(unknownSources.length > 0);
  const deletedOf = (integrationId: number) => (deleted.data ?? []).find((d) => d.integrationId === integrationId);
  return (
    <section aria-label="Preview" className="mb-8">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2 border-b border-line pb-2">
        <h2 className="text-lg">What gets backed up and why</h2>
        <span className="text-xs text-ink-muted">
          {saved ? `Saved rules (revision ${preview.revision})` : 'Draft (not saved)'} · computed {formatDateTime(preview.createdAt)} · nothing was written
        </span>
      </div>
      {outdated && (
        <Notice tone="warning" title="The rules changed since this preview">
          <Button small icon={RefreshCw} onClick={onRerun}>
            Preview again
          </Button>
        </Notice>
      )}
      {unknownSources.length > 0 && (
        <Notice tone="warning" title="Some facts are unknown">
          <p>Conditions that read them are unknown. Unknown never lowers protection: a file a more protective rule might match stays full.</p>
          <ul className="mt-1 list-disc pl-5">
            {unknownSources.map((u) => {
              const d = deletedOf(u.integrationId);
              return (
                <li key={`${u.integrationId}-${u.reason}`}>
                  {u.name}: {u.reason}
                  {d && (
                    <span className="ml-2 inline-block align-middle">
                      <ConfirmRemovalButton record={d} onConfirmed={onRemovalConfirmed} />
                    </span>
                  )}
                </li>
              );
            })}
          </ul>
        </Notice>
      )}
      {(preview.staleReferences ?? []).length > 0 && (
        <Notice tone="warning" title="Values no index knows">
          <p>A renamed or deleted tag, profile, folder, section or user makes its condition false everywhere.</p>
          <ul className="mt-1 list-disc pl-5">
            {preview.staleReferences!.map((w) => (
              <li key={`${w.ruleIndex}-${w.conditionIndex}-${w.message}`}>
                {positionText(w.ruleIndex, w.conditionIndex)}: {w.message}
              </li>
            ))}
          </ul>
        </Notice>
      )}
      {dests.length === 0 && <p className="mb-4 text-sm text-ink-muted">No enabled destination: there is nothing to preview.</p>}
      {dests.map((d) => (
        <DestinationPreview key={d.destinationId} d={d} />
      ))}
      {dests.length > 0 && <PreviewItems key={preview.id} preview={preview} fields={fields} onRerun={onRerun} />}
    </section>
  );
}

function DestinationPreview({ d }: { d: TierDestinationPreview }) {
  const rules = d.byRule ?? [];
  const backups = d.configBackups ?? [];
  return (
    <div className="mb-6">
      <h3 className="mb-2 font-medium">{d.name}</h3>
      <StatGrid label={`Preview for ${d.name}`}>
        <Stat label="Stored" value={countText(d.stored)} hint="What the destination's live records hold now" />
        <Stat label="Full" value={countText(d.full)} hint={`Copied. Hardlinked content counted once: ${formatBytes(d.full.uniqueBytes)}`} />
        <Stat label="Manifest" value={countText(d.manifest)} hint="Only listed in the destination's manifests" muted={d.manifest.files === 0} />
        <Stat label="Skip" value={countText(d.skip)} hint="Neither copied nor listed" muted={d.skip.files === 0} />
        <Stat label="Unknown" value={countText(d.unknownPromoted)} hint="Full only because a fact is unknown" muted={d.unknownPromoted.files === 0} />
        <Stat label="To copy" value={countText(d.toCopy)} hint="Full files without an up-to-date copy: the next sync copies them" muted={d.toCopy.files === 0} />
        <Stat label="Kept" value={countText(d.kept)} hint="No longer full, but backed up: kept until you release them" muted={d.kept.files === 0} />
        <Stat
          label="Moved to a non-full location"
          value={countText(d.movedToNonFull)}
          hint="Backed-up content that reappeared in another source under a non-full tier"
          muted={d.movedToNonFull.files === 0}
        />
      </StatGrid>
      <div className="grid gap-4 lg:grid-cols-[2fr_1fr]">
        {rules.length > 0 && (
          <DataTable
            caption={`Files by rule at ${d.name}`}
            rows={rules}
            rowKey={(r) => `${r.ruleId}-${r.name}`}
            columns={[
              { key: 'rule', header: 'Deciding rule', cell: (r) => (r.ruleId === 0 ? <span className="text-ink-muted">{r.name} (built-in)</span> : r.name) },
              { key: 'action', header: 'Tier', cell: (r) => <TierBadge tier={r.action} /> },
              { key: 'files', header: 'Files', className: 'text-right', cell: (r) => formatNumber(r.files) },
              { key: 'bytes', header: 'Size', className: 'whitespace-nowrap text-right', cell: (r) => formatBytes(r.bytes) },
            ]}
          />
        )}
        {backups.length > 0 && (
          <div className="rounded border border-line bg-panel p-3 text-sm">
            <h4 className="mb-1 text-xs font-medium uppercase tracking-wide text-ink-muted">Config backups (always full)</h4>
            <ul className="space-y-1">
              {backups.map((b) => (
                <li key={`${b.kind}-${b.integrationId}`} className="flex justify-between gap-2">
                  <span>
                    {b.kind === 'plexdb' ? 'Plex DB' : '*arr'} · {b.name}
                  </span>
                  <span className="text-ink-muted">{b.lastBytes > 0 ? formatBytes(b.lastBytes) : 'no version yet'}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </div>
  );
}

interface ItemFilter {
  destinationId: number | '';
  tier: Tier | '';
  ruleId: string;
  state: TierItemState | '';
  search: string;
  page: number;
}

/** PreviewItems is the preview's file table with its filters (destination, tier, rule, state, search). */
function PreviewItems({ preview, fields, onRerun }: { preview: TierPreview; fields: Map<string, TierField>; onRerun: () => void }) {
  const dests = preview.destinations ?? [];
  const [filter, setFilter] = useState<ItemFilter>({ destinationId: dests.length === 1 ? dests[0].destinationId : '', tier: '', ruleId: '', state: '', search: '', page: 1 });
  const [typed, setTyped] = useState('');
  useEffect(() => {
    if (typed.trim() === filter.search) return;
    const t = setTimeout(() => setFilter((f) => ({ ...f, search: typed.trim(), page: 1 })), SEARCH_DELAY_MS);
    return () => clearTimeout(t);
  }, [typed, filter.search]);

  const items = useQuery({
    queryKey: ['tiers', 'preview', preview.id, 'items', filter],
    queryFn: () =>
      listPreviewItems(preview.id, {
        destinationId: filter.destinationId === '' ? undefined : filter.destinationId,
        tier: filter.tier,
        ruleId: filter.ruleId === '' ? undefined : Number(filter.ruleId),
        state: filter.state,
        search: filter.search,
        page: filter.page,
        pageSize: PREVIEW_PAGE_SIZE,
      }),
    placeholderData: keepPreviousData,
    retry: false,
  });
  const expired = items.error instanceof ApiError && items.error.status === 404;

  // The rules the chosen destination (or any) counts, for the rule filter. The built-in decisions
  // (irreplaceable, no rule matched) share ruleId 0, which is all the filter can send: their one
  // option names each of them.
  const ruleNames = new Map<string, string[]>();
  for (const d of dests) {
    if (filter.destinationId !== '' && d.destinationId !== filter.destinationId) continue;
    for (const r of d.byRule ?? []) {
      const names = ruleNames.get(String(r.ruleId)) ?? [];
      if (!names.includes(r.name)) names.push(r.name);
      ruleNames.set(String(r.ruleId), names);
    }
  }
  const rules = [...ruleNames].map(([id, names]) => [id, id === '0' ? `${names.join(' or ')} (built-in)` : names.join(' / ')] as const);
  const destName = new Map(dests.map((d) => [d.destinationId, d.name]));
  const set = (next: Partial<ItemFilter>) => setFilter((f) => ({ ...f, ...next, page: next.page ?? 1 }));

  const columns: Column<TierPreviewItem>[] = [
    {
      key: 'path',
      header: 'File',
      className: 'w-[40%]',
      cell: (i) => (
        <div className="min-w-[12rem]">
          <Link to={`/library/files/${i.fileId}`} className="break-all font-mono text-xs hover:text-accent hover:underline">
            {i.relPath}
          </Link>
          <div className="text-xs text-ink-muted">
            {i.sourceName}
            {filter.destinationId === '' && ` → ${destName.get(i.destinationId) ?? `destination #${i.destinationId}`}`}
          </div>
        </div>
      ),
    },
    { key: 'size', header: 'Size', className: 'whitespace-nowrap text-right', cell: (i) => formatBytes(i.size) },
    {
      key: 'tier',
      header: 'Tier',
      cell: (i) => (
        <div className="flex flex-col items-start gap-1">
          <TierBadge tier={i.tier} />
          {i.unknownPromoted && <UnknownPromotedBadge />}
        </div>
      ),
    },
    {
      key: 'why',
      header: 'Why',
      className: 'w-[35%]',
      cell: (i) => (
        <div className="min-w-[12rem]">
          <div className="text-xs">{decisionRuleText(i)}</div>
          <ReasonList reasons={i.reasons} unknown={i.unknown} fields={fields} follows={i.follows} />
        </div>
      ),
    },
    {
      key: 'state',
      header: 'State',
      cell: (i) => (
        <Badge tone={STATE_LABELS[i.state]?.tone ?? 'muted'} title={STATE_LABELS[i.state]?.help}>
          {STATE_LABELS[i.state]?.label ?? i.state}
        </Badge>
      ),
    },
  ];

  return (
    <div>
      <h3 className="mb-2 font-medium">Files</h3>
      <div className="mb-3 flex flex-wrap items-end gap-3">
        <label className="text-sm">
          <span className="mb-1 block text-ink-muted">Destination</span>
          <select
            className={`${inputClass} w-44`}
            value={String(filter.destinationId)}
            onChange={(e) => set({ destinationId: e.target.value === '' ? '' : Number(e.target.value), ruleId: '' })}
          >
            {dests.length > 1 && <option value="">All destinations</option>}
            {dests.map((d) => (
              <option key={d.destinationId} value={d.destinationId}>
                {d.name}
              </option>
            ))}
          </select>
        </label>
        <label className="text-sm">
          <span className="mb-1 block text-ink-muted">Tier</span>
          <select className={`${inputClass} w-36`} value={filter.tier} onChange={(e) => set({ tier: e.target.value as Tier | '' })}>
            <option value="">All tiers</option>
            {TIERS.map((t) => (
              <option key={t} value={t}>
                {TIER_LABELS[t]}
              </option>
            ))}
          </select>
        </label>
        <label className="text-sm">
          <span className="mb-1 block text-ink-muted">Rule</span>
          <select className={`${inputClass} w-44`} value={filter.ruleId} onChange={(e) => set({ ruleId: e.target.value })}>
            <option value="">All rules</option>
            {rules.map(([id, name]) => (
              <option key={id} value={id}>
                {name}
              </option>
            ))}
          </select>
        </label>
        <label className="text-sm">
          <span className="mb-1 block text-ink-muted">State</span>
          <select className={`${inputClass} w-36`} value={filter.state} onChange={(e) => set({ state: e.target.value as TierItemState | '' })}>
            <option value="">All states</option>
            {(Object.keys(STATE_LABELS) as TierItemState[]).map((s) => (
              <option key={s} value={s}>
                {STATE_LABELS[s].label}
              </option>
            ))}
          </select>
        </label>
        <label className="min-w-[12rem] flex-1 text-sm">
          <span className="mb-1 block text-ink-muted">Search</span>
          <span className="relative block">
            <Search className="pointer-events-none absolute left-2 top-2.5 h-4 w-4 text-ink-muted" aria-hidden="true" />
            <input type="search" className={`${inputClass} pl-8`} value={typed} placeholder="Part of a path" onChange={(e) => setTyped(e.target.value)} />
          </span>
        </label>
      </div>
      {expired ? (
        <Notice tone="warning" title="This preview expired">
          <p>Previews are kept for 10 minutes.</p>
          <Button small className="mt-2" icon={RefreshCw} onClick={onRerun}>
            Preview again
          </Button>
        </Notice>
      ) : (
        <ErrorNotice error={items.error} />
      )}
      {!expired && (
        <>
          <DataTable columns={columns} rows={items.data?.records} rowKey={(i) => `${i.destinationId}-${i.fileId}`} loading={items.isPending} caption="Preview files" empty="No files match." />
          {items.data && items.data.totalRecords > 0 && (
            <Pagination page={filter.page} pageSize={PREVIEW_PAGE_SIZE} total={items.data.totalRecords} onPage={(page) => set({ page })} />
          )}
        </>
      )}
    </div>
  );
}
