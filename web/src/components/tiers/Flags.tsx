import { useQueryClient } from '@tanstack/react-query';
import { Trash2 } from 'lucide-react';
import { useState } from 'react';
import { Link } from 'react-router';
import { addFlag, deleteFlag, type ArrItemFacts, type ExternalIds, type FlagTarget, type ItemFlag } from '@/api/tiers';
import { Button, IconButton } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { DataTable, type Column } from '@/components/DataTable';
import { inputClass } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice } from '@/components/Notice';
import { Badge } from '@/components/StatusBadge';
import { formatRelative } from '@/lib/format';

export const flagsKey = ['tiers', 'flags'] as const;

/** externalIdsText is "tmdb 603, imdb tt0133093". */
export function externalIdsText(ids: ExternalIds | null | undefined): string {
  if (!ids) return '';
  return (['tmdb', 'tvdb', 'imdb', 'mbid', 'tvmaze'] as const)
    .filter((k) => ids[k] !== undefined && ids[k] !== '' && ids[k] !== 0)
    .map((k) => `${k} ${String(ids[k])}`)
    .join(', ');
}

/** flagTargetText says what a flag covers: an *arr item by its ids, or a file or folder of a source. */
export function flagTargetText(f: ItemFlag, sourceName: (id: number) => string): string {
  if (f.kind === 'arr') {
    const ids = externalIdsText(f.externalIds);
    return `${f.arrKind ?? 'item'} ${ids ? `(${ids})` : `#${f.arrId ?? '?'}`}`;
  }
  const where = f.sourceId != null ? sourceName(f.sourceId) : 'a deleted source';
  return f.relPath ? `${where}: ${f.relPath}` : `${where} (the whole source)`;
}

/**
 * FlagsTable lists the irreplaceable flags (GET /tiers/flags): what each covers, whether it
 * resolves now (and why not) and its note, with "Remove".
 */
export function FlagsTable({ flags, sourceName, loading }: { flags: ItemFlag[] | undefined; sourceName: (id: number) => string; loading?: boolean }) {
  const qc = useQueryClient();
  const [removing, setRemoving] = useState<ItemFlag | null>(null);
  const columns: Column<ItemFlag>[] = [
    { key: 'id', header: 'Flag', cell: (f) => `#${f.id}` },
    {
      key: 'target',
      header: 'Covers',
      className: 'w-[45%]',
      cell: (f) => (
        <div className="min-w-[12rem]">
          <span className="break-all">{flagTargetText(f, sourceName)}</span>
          {f.kind === 'arr' && f.lastRelPath && (
            <div className="break-all text-xs text-ink-muted">
              Last folder: {f.lastSourceId != null ? `${sourceName(f.lastSourceId)}: ` : ''}
              {f.lastRelPath}
            </div>
          )}
          {f.note && <div className="text-xs text-ink-muted">{f.note}</div>}
        </div>
      ),
    },
    {
      key: 'state',
      header: 'State',
      cell: (f) =>
        f.resolved ? (
          <Badge tone="ok">Resolves</Badge>
        ) : (
          <span className="flex flex-col items-start gap-1">
            <Badge tone="warn">Not resolved</Badge>
            {f.reason && <span className="text-xs text-warn">{f.reason}</span>}
          </span>
        ),
    },
    { key: 'created', header: 'Added', className: 'whitespace-nowrap', cell: (f) => formatRelative(f.createdAt) },
    {
      key: 'actions',
      header: <span className="sr-only">Actions</span>,
      className: 'text-right',
      cell: (f) => <IconButton label={`Remove flag #${f.id}`} icon={Trash2} className="hover:text-danger" onClick={() => setRemoving(f)} />,
    },
  ];
  return (
    <>
      <DataTable
        columns={columns}
        rows={flags}
        rowKey={(f) => f.id}
        loading={loading}
        caption="Irreplaceable flags"
        empty={
          <>
            No file is flagged irreplaceable. Flag one from its page in the <Link to="/library" className="text-accent hover:underline">Library</Link>.
          </>
        }
      />
      {removing && (
        <RemoveFlagDialog
          flag={removing}
          onClose={() => setRemoving(null)}
          onRemoved={async () => {
            await qc.invalidateQueries({ queryKey: flagsKey });
          }}
        />
      )}
    </>
  );
}

/** RemoveFlagDialog confirms DELETE /tiers/flags/{id}. */
export function RemoveFlagDialog({ flag, onClose, onRemoved }: { flag: Pick<ItemFlag, 'id'>; onClose: () => void; onRemoved: () => Promise<void> | void }) {
  return (
    <ConfirmDialog
      title="Remove flag"
      confirmLabel="Remove flag"
      danger
      onClose={onClose}
      onConfirm={async () => {
        await deleteFlag(flag.id);
        await onRemoved();
      }}
    >
      <p>
        Remove irreplaceable flag #{flag.id}? The files it covers then take the tier the rules give them. Files already backed up stay until you release
        them, and retained copies it held back can expire.
      </p>
    </ConfirmDialog>
  );
}

type FlagChoice = 'item' | 'file' | 'folder';

/**
 * MarkIrreplaceableDialog flags a file irreplaceable (POST /tiers/flags): its *arr item by default
 * when it has one (the flag follows the item's external ids, in any integration), else the file,
 * or its folder. A flagged file is full at every destination and its retained copies never expire.
 */
export function MarkIrreplaceableDialog({
  sourceId,
  relPath,
  item,
  onClose,
  onDone,
}: {
  sourceId: number;
  relPath: string;
  item?: ArrItemFacts;
  onClose: () => void;
  onDone: (f: ItemFlag) => Promise<void> | void;
}) {
  const folder = relPath.includes('/') ? relPath.slice(0, relPath.lastIndexOf('/')) : '';
  const itemIds = item ? externalIdsText(item.externalIds) : '';
  const [choice, setChoice] = useState<FlagChoice>(item && itemIds ? 'item' : 'file');
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function save() {
    setBusy(true);
    setError(null);
    let target: FlagTarget;
    if (choice === 'item' && item) {
      target = { integrationId: item.integrationId, kind: item.kind, arrId: item.arrId };
    } else {
      target = { sourceId, relPath: choice === 'folder' ? folder : relPath };
    }
    try {
      const f = await addFlag({ flag: 'irreplaceable', target, note: note.trim() });
      await onDone(f);
      onClose();
    } catch (e) {
      setError(e);
      setBusy(false);
    }
  }

  const option = (value: FlagChoice, label: string, help: string, disabled = false) => (
    <label className={`flex items-start gap-2 ${disabled ? 'opacity-60' : 'cursor-pointer'}`}>
      <input type="radio" name="flag-target" className="mt-1" checked={choice === value} disabled={disabled} onChange={() => setChoice(value)} />
      <span>
        {label}
        <span className="block text-xs text-ink-muted">{help}</span>
      </span>
    </label>
  );

  return (
    <Modal
      title="Mark irreplaceable"
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" busy={busy} onClick={() => void save()}>
            Mark irreplaceable
          </Button>
        </>
      }
    >
      <ErrorNotice error={error} />
      <p className="mb-3 text-sm">An irreplaceable file is always full, at every destination, whatever the rules say; its retained copies never expire.</p>
      <fieldset className="mb-4 space-y-2 text-sm">
        <legend className="mb-1 font-medium">Flag</legend>
        {item &&
          option(
            'item',
            `The ${item.kind} “${item.title}${item.year ? ` (${item.year})` : ''}”`,
            itemIds
              ? `Every file of it, in any ${item.app} integration, found by ${itemIds}; it follows a re-add under a new id and falls back to its last folder.`
              : 'The item has no external ids, so it cannot be flagged; flag the file or its folder.',
            !itemIds,
          )}
        {option('file', 'This file', `${relPath}; the flag follows a rename Bunkarr pairs as a move.`)}
        {option('folder', folder ? 'Its folder' : 'The whole source', folder ? `Every file under ${folder}/.` : 'Every file of this source.')}
      </fieldset>
      <label className="block text-sm">
        <span className="mb-1 block font-medium">Note</span>
        <input className={inputClass} value={note} maxLength={500} placeholder="Why it is irreplaceable (optional)" onChange={(e) => setNote(e.target.value)} />
      </label>
    </Modal>
  );
}
