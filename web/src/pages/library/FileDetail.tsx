import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ArrowLeft, ShieldCheck, Trash2 } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { Link, useParams } from 'react-router';
import { coversPath } from '@/api/deletedIntegrations';
import { getCatalogFile, getTierFields, type FileDetail as Detail, type FileFacts, type FileTierAtDestination, type TierField } from '@/api/tiers';
import { Button } from '@/components/Button';
import { DataTable, type Column } from '@/components/DataTable';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Page } from '@/components/Page';
import { Badge } from '@/components/StatusBadge';
import { ConfirmRemovalButton, useDeletedIntegrations } from '@/components/tiers/DeletedIntegrations';
import { flagsKey, MarkIrreplaceableDialog, RemoveFlagDialog } from '@/components/tiers/Flags';
import { ReasonList, TierBadge, UnknownPromotedBadge } from '@/components/tiers/TierBadge';
import { decisionRuleText, fieldMap, valueText } from '@/components/tiers/tierText';
import { formatBytes, formatDateTime, formatNumber, formatRelative } from '@/lib/format';
import { useSources } from '@/lib/lookups';

export const fileDetailKey = (id: number) => ['catalog', 'file', id] as const;

const SOURCE_NAMES: Record<string, string> = {
  arr: 'Sonarr, Radarr, Lidarr',
  plex: 'Plex',
  tautulli: 'Tautulli',
  seerr: 'Seerr',
  maintainerr: 'Maintainerr',
};

/** Unknown marks a fact that could not be decided, with why. */
function Unknown({ why }: { why?: string }) {
  return (
    <span className="text-warn">
      Unknown{why ? `: ${why}` : ''}
    </span>
  );
}

/** notRead is the text of a fact nothing supplies (its provider is not configured or built). */
const notRead = <span className="text-ink-muted">Not read (no integration supplies it)</span>;

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <tr className="border-b border-line/60 last:border-b-0">
      <th scope="row" className="w-44 px-3 py-2 text-left align-top font-normal text-ink-muted">
        {label}
      </th>
      <td className="min-w-0 break-words px-3 py-2 align-top">{children}</td>
    </tr>
  );
}

function list(values: string[] | null | undefined): string {
  return values && values.length > 0 ? values.join(', ') : 'none';
}

/**
 * FactsTable lists what the tier rules see for a file: the *arr item (tags, profile, root folder,
 * monitored, genres), the Plex section, plays and last watched, requests, Maintainerr's status and
 * the flags. An unknown fact is marked, with the reason.
 */
function FactsTable({ detail, fields, onRemoveFlag }: { detail: Detail; fields: Map<string, TierField>; onRemoveFlag: (id: number) => void }) {
  const f: FileFacts = detail.facts;
  const arr = f.arr;
  const item = arr.item;
  return (
    <div className="relative mb-6 overflow-x-auto rounded border border-line bg-panel">
      <table className="w-full text-sm">
        <caption className="sr-only">Facts</caption>
        <tbody>
          <Row label="Source">
            <Link to={`/library/sources/${detail.source.id}`} className="hover:text-accent hover:underline">
              {detail.source.name}
            </Link>
          </Row>
          <Row label="*arr item">
            {arr.state === 'unknown' ? (
              <Unknown why={arr.why} />
            ) : arr.state === 'unmanaged' ? (
              <span>Not managed: outside every *arr root folder</span>
            ) : item ? (
              <span>
                {item.title}
                {item.year ? ` (${item.year})` : ''} <span className="text-ink-muted">· {item.app} {item.kind} #{item.arrId}</span>
                {arr.byFolder && <span className="block text-xs text-ink-muted">Attributed by folder (an extra): episode-level facts are unknown.</span>}
              </span>
            ) : (
              <Unknown why={arr.why} />
            )}
          </Row>
          {item && (
            <>
              <Row label="Tags">{list(item.tags)}</Row>
              <Row label="Quality profile">{item.qualityProfile || '—'}</Row>
              <Row label="Root folder">
                <span className="break-all font-mono text-xs">{item.rootFolder || '—'}</span>
              </Row>
              <Row label="Monitored">{item.monitored ? 'Yes' : 'No'}</Row>
              <Row label="Genres">{list(item.genres)}</Row>
              {(arr.quality || arr.dateAdded) && (
                <Row label="*arr file">
                  {[arr.quality, arr.dateAdded ? `added ${formatDateTime(arr.dateAdded)}` : ''].filter(Boolean).join(' · ')}
                </Row>
              )}
            </>
          )}
          <Row label="Plex section">
            {!f.plex ? (
              notRead
            ) : f.plex.known ? (
              f.plex.section ? (
                `${fields.get('plex.section')?.suggestions.find((x) => x.value === f.plex!.section)?.label ?? f.plex.section}${f.plex.addedAt ? ` · added ${formatDateTime(f.plex.addedAt)}` : ''}`
              ) : (
                'Not in a Plex library'
              )
            ) : (
              <Unknown why={f.plex.why} />
            )}
          </Row>
          <Row label="Plays">
            {!f.watch ? (
              notRead
            ) : f.watch.known ? (
              <>
                {formatNumber(f.watch.plays)}
                {f.watch.lowerBound && ' or more (history is off for a user or the library)'}
                {' · last watched '}
                {f.watch.lastWatched ? formatRelative(f.watch.lastWatched) : 'never'}
              </>
            ) : (
              <Unknown why={f.watch.why} />
            )}
          </Row>
          <Row label="Requested by">
            {!f.requests ? (
              notRead
            ) : f.requests.requested === 'unknown' ? (
              <Unknown why={f.requests.why} />
            ) : f.requests.requested === 'true' ? (
              (f.requests.users ?? []).length > 0 ? (
                // Seerr users by the editor's labels when it has them, else by id only.
                valueText('seerr.requestedBy', f.requests.users, fields.get('seerr.requestedBy'))
              ) : (
                'Requested'
              )
            ) : (
              'Not requested'
            )}
          </Row>
          <Row label="Maintainerr">
            {!f.maintainerr ? (
              notRead
            ) : f.maintainerr.pending === 'unknown' ? (
              <Unknown why={f.maintainerr.why} />
            ) : f.maintainerr.pending === 'true' ? (
              <span className="text-warn">Pending deletion{f.maintainerr.deleteAfter ? ` after ${formatDateTime(f.maintainerr.deleteAfter)}` : ''}</span>
            ) : (
              'Not pending deletion'
            )}
          </Row>
          <Row label="Flags">
            {(f.flags ?? []).length === 0 ? (
              'None'
            ) : (
              <span className="flex flex-wrap items-center gap-2">
                {f.flags!.map((id) => (
                  <span key={id} className="inline-flex items-center gap-1">
                    <Badge tone="ok">Irreplaceable (flag #{id})</Badge>
                    <Button small variant="ghost" icon={Trash2} aria-label={`Remove flag #${id}`} onClick={() => onRemoveFlag(id)}>
                      Remove
                    </Button>
                  </span>
                ))}
              </span>
            )}
          </Row>
          {f.follows && (
            <Row label="Follows">
              <span className="break-all font-mono text-xs">{f.follows}</span>
              <span className="block text-xs text-ink-muted">A sidecar takes the decision of its media file.</span>
            </Row>
          )}
        </tbody>
      </table>
    </div>
  );
}

function recordState(state: string): ReactNode {
  const tone = state === 'present' ? 'ok' : state === 'missing' ? 'danger' : 'info';
  return <Badge tone={tone}>{state.replace(/_/g, ' ')}</Badge>;
}

/**
 * Library → item view (/library/files/:id, phase2-3.md §16): a catalog file's facts (unknown ones
 * marked with the reason), its tier at each destination with the reasons, the destination
 * records, and "Mark irreplaceable" (the *arr item by default when there is one, else the path).
 */
export function FileDetail() {
  const fileId = Number(useParams().id);
  const validId = Number.isInteger(fileId) && fileId > 0;
  const qc = useQueryClient();
  const detail = useQuery({ queryKey: fileDetailKey(fileId), queryFn: () => getCatalogFile(fileId), enabled: validId, retry: false });
  const fields = useQuery({ queryKey: ['tiers', 'fields'], queryFn: getTierFields });
  const [marking, setMarking] = useState(false);
  const [removing, setRemoving] = useState<number | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const fmap: Map<string, TierField> = fieldMap(fields.data);

  async function refresh() {
    await Promise.all([qc.invalidateQueries({ queryKey: fileDetailKey(fileId) }), qc.invalidateQueries({ queryKey: flagsKey })]);
  }

  const d = detail.data;
  const unknown = d?.facts.unknown ?? [];
  // A file whose *arr facts are unknown may be in a deleted *arr integration's folder: its removal
  // is confirmed here too (the server decides; this only offers the action where it can matter).
  const arrUnknown = d?.facts.arr.state === 'unknown';
  const deleted = useDeletedIntegrations(arrUnknown);
  const sources = useSources();
  const sourcePath = sources.data?.find((s) => s.id === d?.source.id)?.path;
  const localPath = sourcePath && d ? `${sourcePath.replace(/\/+$/, '')}/${d.file.relPath}` : null;
  const deletedHere = arrUnknown && localPath ? (deleted.data ?? []).filter((r) => coversPath(r, localPath)) : [];

  const tierColumns: Column<FileTierAtDestination>[] = [
    { key: 'dest', header: 'Destination', cell: (t) => <span className="font-medium">{t.destinationName}</span> },
    {
      key: 'tier',
      header: 'Tier',
      cell: (t) => (
        <div className="flex flex-col items-start gap-1">
          <TierBadge tier={t.tier} />
          {t.unknownPromoted && <UnknownPromotedBadge />}
        </div>
      ),
    },
    {
      key: 'why',
      header: 'Why',
      className: 'w-[55%]',
      cell: (t) => (
        <div className="min-w-[14rem]">
          <div className="text-xs">{decisionRuleText(t)}</div>
          <ReasonList reasons={t.reasons} unknown={t.unknown} fields={fmap} follows={t.follows} />
        </div>
      ),
    },
  ];

  const recordColumns: Column<FileTierAtDestination>[] = [
    { key: 'dest', header: 'Destination', cell: (t) => t.destinationName },
    {
      key: 'state',
      header: 'Record',
      cell: (t) =>
        t.record ? (
          recordState(t.record.state)
        ) : (
          <span className="text-ink-muted">{t.tier === 'full' ? 'Not copied yet' : 'Not copied (not full here)'}</span>
        ),
    },
    { key: 'path', header: 'Path at the destination', cell: (t) => (t.record ? <span className="break-all font-mono text-xs">{t.record.relPath}</span> : '—') },
    { key: 'size', header: 'Size', className: 'whitespace-nowrap text-right', cell: (t) => (t.record ? formatBytes(t.record.size) : '—') },
    { key: 'copied', header: 'Copied', className: 'whitespace-nowrap', cell: (t) => (t.record?.copiedAt ? <span title={formatDateTime(t.record.copiedAt)}>{formatRelative(t.record.copiedAt)}</span> : '—') },
    {
      key: 'verified',
      header: 'Verified',
      className: 'whitespace-nowrap',
      cell: (t) => (t.record?.verifiedAt ? <span title={formatDateTime(t.record.verifiedAt)}>{formatRelative(t.record.verifiedAt)}</span> : '—'),
    },
  ];

  return (
    <Page
      title={d ? <span className="break-all">{d.file.relPath.split('/').pop()}</span> : 'File'}
      actions={
        <>
          <Link
            to={d ? `/library/sources/${d.source.id}` : '/library'}
            className="inline-flex items-center gap-2 rounded px-3 py-1.5 text-sm hover:bg-panel-2"
          >
            <ArrowLeft className="h-4 w-4" aria-hidden="true" /> {d ? d.source.name : 'Library'}
          </Link>
          {d && (
            <Button variant="ghost" icon={ShieldCheck} onClick={() => setMarking(true)}>
              Mark irreplaceable
            </Button>
          )}
        </>
      }
    >
      {!validId && <Notice tone="error">There is no file with this id.</Notice>}
      <ErrorNotice error={detail.error} />
      {notice && <Notice tone="success">{notice}</Notice>}
      {validId && detail.isPending && <p className="text-ink-muted">Loading…</p>}
      {d && (
        <>
          <p className="mb-1 break-all font-mono text-xs">{d.file.relPath}</p>
          <p className="mb-4 text-xs text-ink-muted">
            {formatBytes(d.file.size)} · modified {formatDateTime(d.file.mtime)}
            {d.file.hardlinkGroup && ' · hardlinked (the most protective tier of its names applies)'}
          </p>
          {unknown.length > 0 && (
            <Notice tone="warning" title="Some facts are unknown">
              <ul className="list-disc pl-5">
                {unknown.map((u) => (
                  <li key={`${u.source}-${u.reason}`}>
                    {SOURCE_NAMES[u.source] ?? u.source}: {u.reason}
                  </li>
                ))}
              </ul>
              <p className="mt-1">Conditions that read them are unknown; unknown never lowers protection.</p>
              {deletedHere.length > 0 && (
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <span>
                    {deletedHere.length === 1 ? 'A deleted *arr integration keeps' : 'Deleted *arr integrations keep'} this file unknown (so full) until you
                    confirm the removal:
                  </span>
                  {deletedHere.map((r) => (
                    <ConfirmRemovalButton key={r.key} record={r} onConfirmed={refresh} />
                  ))}
                </div>
              )}
            </Notice>
          )}
          <h2 className="mb-2 text-lg">Facts</h2>
          <FactsTable detail={d} fields={fmap} onRemoveFlag={setRemoving} />

          <h2 className="mb-2 text-lg">Tier per destination</h2>
          <div className="mb-6">
            <DataTable
              columns={tierColumns}
              rows={d.tiers ?? []}
              rowKey={(t) => t.destinationId}
              caption="Tier per destination"
              empty="No destination backs up this file's source."
            />
          </div>

          <h2 className="mb-2 text-lg">Destination records</h2>
          <DataTable columns={recordColumns} rows={d.tiers ?? []} rowKey={(t) => t.destinationId} caption="Destination records" empty="No destination backs up this file's source." />
        </>
      )}
      {d && marking && (
        <MarkIrreplaceableDialog
          sourceId={d.source.id}
          relPath={d.file.relPath}
          item={d.facts.arr.state === 'item' ? d.facts.arr.item : undefined}
          onClose={() => setMarking(false)}
          onDone={async (f) => {
            setNotice(`Flagged irreplaceable (flag #${f.id}). The next sync of each destination copies it in full.`);
            await refresh();
          }}
        />
      )}
      {removing !== null && (
        <RemoveFlagDialog
          flag={{ id: removing }}
          onClose={() => setRemoving(null)}
          onRemoved={async () => {
            setNotice(`Flag #${removing} removed.`);
            await refresh();
          }}
        />
      )}
    </Page>
  );
}
