import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query';
import { ArrowLeft, ScanSearch, Search } from 'lucide-react';
import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { Link, useParams, useSearchParams } from 'react-router';
import { errorMessage } from '@/api/client';
import { getSource, listFiles, scanSource } from '@/api/library';
import type { CatalogFile, FileFilter } from '@/api/types';
import { Button } from '@/components/Button';
import { DataTable, type Column } from '@/components/DataTable';
import { inputClass } from '@/components/Form';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Page, Stat, StatGrid } from '@/components/Page';
import { Pagination } from '@/components/Pagination';
import { Badge, ScanStatusBadge } from '@/components/StatusBadge';
import { formatBytes, formatDateTime, formatNumber, formatRelative } from '@/lib/format';
import { keys } from '@/lib/lookups';

export const FILES_PAGE_SIZE = 50;
const SEARCH_DELAY_MS = 300;

const FILTERS: { value: FileFilter; label: string }[] = [
  { value: 'all', label: 'Files in the source' },
  { value: 'hardlinked', label: 'Hardlinked' },
  { value: 'deleted', label: 'Deleted from the source' },
];

/** Library → source file browser (/library/sources/:id): the catalog of one source, paged. */
export function SourceFiles() {
  const sourceId = Number(useParams().id);
  const validId = Number.isInteger(sourceId) && sourceId > 0;
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const page = Math.max(1, Number(params.get('page')) || 1);
  const filter = (params.get('filter') as FileFilter | null) ?? 'all';
  const search = params.get('search') ?? '';
  const [typed, setTyped] = useState(search);
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);

  const update = useCallback(
    (next: Record<string, string>) =>
      setParams(
        (prev) => {
          const p = new URLSearchParams(prev);
          for (const [k, v] of Object.entries(next)) {
            if (v) p.set(k, v);
            else p.delete(k);
          }
          return p;
        },
        { replace: true },
      ),
    [setParams],
  );

  // Search as you type, after a short pause.
  useEffect(() => {
    if (typed.trim() === search) return;
    const t = setTimeout(() => update({ search: typed.trim(), page: '' }), SEARCH_DELAY_MS);
    return () => clearTimeout(t);
  }, [typed, search, update]);

  const source = useQuery({ queryKey: ['source', sourceId], queryFn: () => getSource(sourceId), enabled: validId });
  const files = useQuery({
    queryKey: ['source', sourceId, 'files', page, filter, search],
    queryFn: () => listFiles(sourceId, { page, pageSize: FILES_PAGE_SIZE, search, filter }),
    enabled: validId,
    placeholderData: keepPreviousData,
  });

  async function scan() {
    setNotice(null);
    try {
      const job = await scanSource(sourceId);
      await qc.invalidateQueries({ queryKey: keys.jobs });
      setNotice({
        tone: 'success',
        body: (
          <>
            Scan queued.{' '}
            <Link className="text-accent hover:underline" to={`/activity/jobs/${job.id}`}>
              View job #{job.id}
            </Link>
          </>
        ),
      });
    } catch (e) {
      setNotice({ tone: 'error', body: errorMessage(e) });
    }
  }

  const columns: Column<CatalogFile>[] = [
    {
      key: 'path',
      header: 'Path',
      className: 'w-[55%]',
      // A live file links to its item view (facts, tier per destination, flags); a deleted one has none.
      cell: (f) =>
        f.deleted ? (
          <span className="break-all font-mono text-xs text-ink-muted line-through">{f.relPath}</span>
        ) : (
          <Link to={`/library/files/${f.id}`} className="break-all font-mono text-xs hover:text-accent hover:underline">
            {f.relPath}
          </Link>
        ),
    },
    { key: 'size', header: 'Size', className: 'whitespace-nowrap text-right', cell: (f) => formatBytes(f.size) },
    { key: 'mtime', header: 'Modified', className: 'whitespace-nowrap', cell: (f) => <span title={formatDateTime(f.mtime)}>{formatRelative(f.mtime)}</span> },
    {
      key: 'links',
      header: 'Links',
      cell: (f) =>
        f.hardlinkGroup ? (
          <Badge tone="info" title={`Hardlink group ${f.hardlinkGroup}: the content is stored once`}>
            {f.nlink} names
          </Badge>
        ) : f.nlink > 1 ? (
          <Badge title="Other names are outside this source">{f.nlink} names</Badge>
        ) : null,
    },
    {
      key: 'state',
      header: 'State',
      cell: (f) =>
        f.deleted ? (
          <Badge tone="warn" title="Gone from the source; destinations keep it in retention">
            Deleted
          </Badge>
        ) : null,
    },
  ];

  const s = source.data;
  return (
    <Page
      title={s ? s.name : 'Source'}
      actions={
        <>
          <Link to="/library" className="inline-flex items-center gap-2 rounded px-3 py-1.5 text-sm hover:bg-panel-2">
            <ArrowLeft className="h-4 w-4" aria-hidden="true" /> Library
          </Link>
          <Button variant="ghost" icon={ScanSearch} disabled={!validId} onClick={() => void scan()}>
            Scan
          </Button>
        </>
      }
    >
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      {!validId && <Notice tone="error">There is no source with this id.</Notice>}
      <ErrorNotice error={source.error} />
      {s && (
        <>
          <p className="mb-3 break-all font-mono text-xs text-ink-muted">
            {s.path} → <span className="text-ink">{s.destFolder}/</span>
          </p>
          <StatGrid label="Source statistics">
            <Stat label="Files" value={formatNumber(s.stats?.files)} />
            <Stat label="Size" value={formatBytes(s.stats?.bytes)} />
            <Stat label="Unique size" value={formatBytes(s.stats?.uniqueBytes)} hint="Hardlinked files counted once" />
            <Stat label="Hardlink groups" value={formatNumber(s.stats?.hardlinkGroups)} />
            <Stat label="Skipped" value={formatNumber(s.stats?.skipped)} hint="Symlinks and other non-regular files" />
            <Stat
              label="Last scan"
              value={
                <span className="flex items-center gap-2">
                  <ScanStatusBadge status={s.lastScanStatus} />
                  <span className="text-xs text-ink-muted">{s.lastScanAt ? formatRelative(s.lastScanAt) : ''}</span>
                </span>
              }
            />
          </StatGrid>
        </>
      )}
      <div className="mb-3 flex flex-wrap items-end gap-3">
        <label className="min-w-[14rem] flex-1 text-sm">
          <span className="mb-1 block text-ink-muted">Search</span>
          <span className="relative block">
            <Search className="pointer-events-none absolute left-2 top-2.5 h-4 w-4 text-ink-muted" aria-hidden="true" />
            <input type="search" className={`${inputClass} pl-8`} value={typed} placeholder="Part of a path" onChange={(e) => setTyped(e.target.value)} />
          </span>
        </label>
        <label className="text-sm">
          <span className="mb-1 block text-ink-muted">Show</span>
          <select className={`${inputClass} w-56`} value={filter} onChange={(e) => update({ filter: e.target.value === 'all' ? '' : e.target.value, page: '' })}>
            {FILTERS.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
        </label>
      </div>
      <ErrorNotice error={files.error} />
      <DataTable
        columns={columns}
        rows={files.data?.records}
        rowKey={(f) => f.id}
        loading={files.isPending}
        caption="Catalog files"
        empty={search || filter !== 'all' ? 'No files match.' : 'No files cataloged yet. Run a scan.'}
      />
      {files.data && files.data.totalRecords > 0 && (
        <Pagination page={page} pageSize={FILES_PAGE_SIZE} total={files.data.totalRecords} onPage={(p) => update({ page: p > 1 ? String(p) : '' })} />
      )}
    </Page>
  );
}
