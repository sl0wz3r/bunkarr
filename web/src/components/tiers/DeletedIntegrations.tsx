import { useQuery, useQueryClient } from '@tanstack/react-query';
import { CheckCircle2 } from 'lucide-react';
import { useState } from 'react';
import { Link } from 'react-router';
import { confirmDeletedIntegration, listDeletedIntegrations, type DeletedIntegration } from '@/api/deletedIntegrations';
import { Button } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { ErrorNotice, Notice } from '@/components/Notice';
import { formatDateTime, formatNumber, formatRelative } from '@/lib/format';

/** Under ['integrations'], so invalidating the integrations (after a delete) refreshes it too. */
export const deletedIntegrationsKey = ['integrations', 'deleted'] as const;

/** useDeletedIntegrations lists the deleted *arr integrations whose removal is not confirmed. */
export function useDeletedIntegrations(enabled = true) {
  return useQuery({ queryKey: deletedIntegrationsKey, queryFn: listDeletedIntegrations, enabled });
}

const SHOWN_FOLDERS = 3;

/** foldersText is "/media/movies, /media/4k and 2 more". */
export function foldersText(folders: string[] | null | undefined): string {
  const list = folders ?? [];
  const shown = list.slice(0, SHOWN_FOLDERS).join(', ');
  return list.length > SHOWN_FOLDERS ? `${shown} and ${formatNumber(list.length - SHOWN_FOLDERS)} more` : shown;
}

/**
 * ConfirmRemovalDialog confirms DELETE /integrations/deleted/{key}: Bunkarr forgets the deleted
 * integration's folders, so the files in them that no other *arr manages are unmanaged and the
 * rules decide them. Nothing leaves the destinations (S15).
 */
export function ConfirmRemovalDialog({
  record,
  onClose,
  onConfirmed,
}: {
  record: DeletedIntegration;
  onClose: () => void;
  onConfirmed?: () => Promise<void> | void;
}) {
  const qc = useQueryClient();
  return (
    <ConfirmDialog
      title={`Confirm removal of ${record.name}`}
      confirmLabel="Confirm removal"
      danger
      onClose={onClose}
      onConfirm={async () => {
        await confirmDeletedIntegration(record.key);
        await qc.invalidateQueries({ queryKey: deletedIntegrationsKey });
        await onConfirmed?.();
      }}
    >
      <p>
        Confirm that <strong>{record.name}</strong> ({record.app}), deleted {formatRelative(record.deletedAt)}, is gone for good?
      </p>
      <p>
        Bunkarr then forgets its folders{(record.folders ?? []).length > 0 && ` (${foldersText(record.folders)})`}. Files in them that no other *arr manages
        stop being unknown: they count as not managed by an *arr, and your tier rules decide them from the next sync or preview. With no rules they stay
        full.
      </p>
      <p className="text-ink-muted">
        Nothing is removed from your destinations: a file whose tier drops keeps its copy until you release it. Until you confirm, these files stay full and
        their copies are held by the mass-change guard.
      </p>
    </ConfirmDialog>
  );
}

/** ConfirmRemovalButton opens ConfirmRemovalDialog for one record. */
export function ConfirmRemovalButton({ record, onConfirmed }: { record: DeletedIntegration; onConfirmed?: () => Promise<void> | void }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button small icon={CheckCircle2} aria-label={`Confirm removal of ${record.name}`} onClick={() => setOpen(true)}>
        Confirm removal…
      </Button>
      {open && <ConfirmRemovalDialog record={record} onClose={() => setOpen(false)} onConfirmed={onConfirmed} />}
    </>
  );
}

/**
 * DeletedIntegrations lists the deleted *arr integrations whose removal is not confirmed, each
 * with "Confirm removal" (Settings → Tiers, Settings → Connect). It renders nothing when there are
 * none.
 */
export function DeletedIntegrations({ onConfirmed }: { onConfirmed?: () => Promise<void> | void }) {
  const list = useDeletedIntegrations();
  if (list.error) return <ErrorNotice error={list.error} />;
  const records = list.data ?? [];
  if (records.length === 0) return null;
  return (
    <section aria-label="Deleted *arr integrations">
      <Notice tone="warning" title={records.length === 1 ? 'A deleted *arr integration needs your confirmation' : 'Deleted *arr integrations need your confirmation'}>
        <p>
          Files in their folders that no other *arr manages are unknown to the tier rules, so they stay full, and new copies of them are held by the
          mass-change guard. Confirm the removal once you no longer use the integration.
        </p>
        <ul className="mt-2 space-y-2">
          {records.map((d) => (
            <li key={d.key} className="flex flex-wrap items-start justify-between gap-2">
              <div className="min-w-[12rem]">
                <span className="font-medium">{d.name}</span> <span className="text-ink-muted">({d.app})</span>
                <span className="text-xs text-ink-muted" title={formatDateTime(d.deletedAt)}>
                  {' '}
                  · deleted {formatRelative(d.deletedAt)}
                </span>
                {(d.folders ?? []).length > 0 && <div className="break-all text-xs text-ink-muted">Folders: {foldersText(d.folders)}</div>}
                {(d.unmapped ?? []).length > 0 && (
                  <div className="break-all text-xs text-ink-muted">Root folders without a path mapping (every file no *arr manages stays full): {foldersText(d.unmapped)}</div>
                )}
              </div>
              <ConfirmRemovalButton record={d} onConfirmed={onConfirmed} />
            </li>
          ))}
        </ul>
      </Notice>
    </section>
  );
}

/**
 * DeletedIntegrationsHint names the deleted *arr integrations still waiting for a confirmation
 * where held copies are shown (a sync's job page), with the action. It renders nothing when there
 * are none or they cannot be read: it is a hint, the job page stands without it.
 */
export function DeletedIntegrationsHint() {
  const list = useDeletedIntegrations();
  const records = list.data ?? [];
  if (records.length === 0) return null;
  return (
    <div className="mt-2 border-t border-warn/30 pt-2">
      <p>
        {records.length === 1
          ? 'A deleted *arr integration keeps the files in its folders'
          : `${formatNumber(records.length)} deleted *arr integrations keep the files in their folders`}{' '}
        full (unknown) until you confirm the removal, and the guard can hold their copies on every sync until then (
        <Link to="/settings/tiers" className="text-accent hover:underline">
          Settings → Tiers
        </Link>
        ).
      </p>
      <div className="mt-1 flex flex-wrap gap-2">
        {records.map((d) => (
          <ConfirmRemovalButton key={d.key} record={d} />
        ))}
      </div>
    </div>
  );
}
