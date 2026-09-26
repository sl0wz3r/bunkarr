import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Database, Download, Pencil, Plus, RefreshCw, ScanSearch, Trash2 } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { Link } from 'react-router';
import { errorMessage } from '@/api/client';
import { catalogStats, deleteSource, scanSource } from '@/api/library';
import type { Source } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { DataTable, type Column } from '@/components/DataTable';
import { ManifestExportLinks } from '@/components/ManifestExport';
import { ErrorNotice, Notice } from '@/components/Notice';
import { EmptyState, Page, Stat, StatGrid } from '@/components/Page';
import { Badge, ScanStatusBadge } from '@/components/StatusBadge';
import { formatBytes, formatDateTime, formatNumber, formatRelative } from '@/lib/format';
import { keys, useSources } from '@/lib/lookups';
import { PlexImport } from './PlexImport';
import { SourceForm } from './SourceForm';

/** Library: the sources Bunkarr backs up, their catalog figures, add/edit/delete, scan, Plex import. */
export function Library() {
  const qc = useQueryClient();
  const sources = useSources();
  const stats = useQuery({ queryKey: keys.catalogStats, queryFn: catalogStats });
  const [editing, setEditing] = useState<Source | 'new' | null>(null);
  const [deleting, setDeleting] = useState<Source | null>(null);
  const [importing, setImporting] = useState(false);
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);

  async function scan(s: Source) {
    setNotice(null);
    try {
      const job = await scanSource(s.id);
      await qc.invalidateQueries({ queryKey: keys.jobs });
      setNotice({
        tone: 'success',
        body: (
          <>
            Scan of {s.name} queued.{' '}
            <Link className="text-accent hover:underline" to={`/activity/jobs/${job.id}`}>
              View job #{job.id}
            </Link>
          </>
        ),
      });
    } catch (e) {
      setNotice({ tone: 'error', body: `Scan of ${s.name}: ${errorMessage(e)}` });
    }
  }

  const columns: Column<Source>[] = [
    {
      key: 'name',
      header: 'Source',
      cell: (s) => (
        <div className="min-w-[10rem]">
          <Link to={`/library/sources/${s.id}`} className="font-medium hover:text-accent hover:underline">
            {s.name}
          </Link>
          {!s.enabled && (
            <span className="ml-2">
              <Badge>Disabled</Badge>
            </span>
          )}
          {s.plexPath && (
            <span className="ml-2">
              <Badge tone="info" title={`Plex path: ${s.plexPath}`}>
                Plex
              </Badge>
            </span>
          )}
          <div className="break-all font-mono text-xs text-ink-muted">{s.path}</div>
        </div>
      ),
    },
    { key: 'folder', header: 'Destination folder', cell: (s) => <code className="text-xs">{s.destFolder}</code> },
    { key: 'files', header: 'Files', className: 'text-right', cell: (s) => formatNumber(s.stats?.files) },
    { key: 'size', header: 'Size', className: 'whitespace-nowrap text-right', cell: (s) => formatBytes(s.stats?.bytes) },
    {
      key: 'unique',
      header: 'Unique',
      className: 'whitespace-nowrap text-right',
      cell: (s) => <span title="Size with each hardlinked file counted once: what a backup stores">{formatBytes(s.stats?.uniqueBytes)}</span>,
    },
    { key: 'groups', header: 'Hardlink groups', className: 'text-right', cell: (s) => formatNumber(s.stats?.hardlinkGroups) },
    {
      key: 'skipped',
      header: 'Skipped',
      className: 'text-right',
      cell: (s) => <span title="Symlinks and other non-regular files are reported, never followed or copied">{formatNumber(s.stats?.skipped)}</span>,
    },
    {
      key: 'scan',
      header: 'Last scan',
      className: 'whitespace-nowrap',
      cell: (s) => (
        <div>
          <ScanStatusBadge status={s.lastScanStatus} />
          {s.lastScanAt && (
            <div className="mt-1 text-xs text-ink-muted" title={formatDateTime(s.lastScanAt)}>
              {formatRelative(s.lastScanAt)}
            </div>
          )}
        </div>
      ),
    },
    {
      key: 'actions',
      header: <span className="sr-only">Actions</span>,
      className: 'whitespace-nowrap text-right',
      cell: (s) => (
        <>
          <IconButton label={`Scan ${s.name}`} icon={ScanSearch} onClick={() => void scan(s)} />
          <IconButton label={`Edit ${s.name}`} icon={Pencil} onClick={() => setEditing(s)} />
          <IconButton label={`Delete ${s.name}`} icon={Trash2} className="hover:text-danger" onClick={() => setDeleting(s)} />
        </>
      ),
    },
  ];

  const st = stats.data;
  const list = sources.data;
  return (
    <Page
      title="Library"
      actions={
        <>
          <Button variant="ghost" icon={Plus} onClick={() => setEditing('new')}>
            Add source
          </Button>
          <Button variant="ghost" icon={Download} onClick={() => setImporting(true)}>
            Import from Plex
          </Button>
          <ManifestExportLinks />
          <Button
            variant="ghost"
            icon={RefreshCw}
            onClick={() => {
              void sources.refetch();
              void stats.refetch();
            }}
          >
            Refresh
          </Button>
        </>
      }
    >
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      <ErrorNotice error={sources.error} />
      {st && (list?.length ?? 0) > 0 && (
        <StatGrid label="Catalog">
          <Stat label="Sources" value={formatNumber(st.sources)} />
          <Stat label="Files" value={formatNumber(st.files)} />
          <Stat label="Size" value={formatBytes(st.bytes)} />
          <Stat label="Unique size" value={formatBytes(st.uniqueBytes)} hint="Hardlinked files counted once" />
          <Stat label="Hardlinked files" value={formatNumber(st.hardlinkedFiles)} />
          <Stat label="Hardlink groups" value={formatNumber(st.hardlinkGroups)} />
        </StatGrid>
      )}
      {list && list.length === 0 ? (
        <EmptyState icon={Database} title="No sources yet">
          <p>
            A source is a media folder Bunkarr reads (never writes) and mirrors to your destinations. Add one by path, or import your Plex libraries: Bunkarr maps
            Plex's folders to its own paths.
          </p>
          <div className="mt-4 flex justify-center gap-2">
            <Button variant="primary" icon={Plus} onClick={() => setEditing('new')}>
              Add source
            </Button>
            <Button icon={Download} onClick={() => setImporting(true)}>
              Import from Plex
            </Button>
          </div>
        </EmptyState>
      ) : (
        <DataTable columns={columns} rows={list} rowKey={(s) => s.id} loading={sources.isPending} caption="Sources" />
      )}

      {editing && <SourceForm source={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
      {importing && <PlexImport onClose={() => setImporting(false)} />}
      {deleting && (
        <ConfirmDialog
          title="Delete source"
          confirmLabel="Delete"
          danger
          onClose={() => setDeleting(null)}
          onConfirm={async () => {
            await deleteSource(deleting.id);
            await qc.invalidateQueries({ queryKey: keys.sources });
            await qc.invalidateQueries({ queryKey: keys.catalogStats });
            await qc.invalidateQueries({ queryKey: keys.destinations });
          }}
        >
          <p>
            Delete <strong>{deleting.name}</strong> and its catalog from Bunkarr?
          </p>
          <p className="text-ink-muted">
            Nothing is deleted from <code>{deleting.path}</code> and nothing is deleted from your destinations: files already backed up stay where they are.
          </p>
        </ConfirmDialog>
      )}
    </Page>
  );
}
