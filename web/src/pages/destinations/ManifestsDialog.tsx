import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Eye, FileText, RefreshCw } from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { Link, useNavigate } from 'react-router';
import { errorMessage } from '@/api/client';
import { listManifests, manifestDownloadUrl, startManifestExport, type ManifestVersion } from '@/api/manifests';
import type { Destination } from '@/api/types';
import { Button } from '@/components/Button';
import { DataTable, type Column } from '@/components/DataTable';
import { DownloadLink, ManifestExportLinks } from '@/components/ManifestExport';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Badge } from '@/components/StatusBadge';
import { formatBytes, formatDateTime, formatNumber, formatRelative } from '@/lib/format';
import { keys } from '@/lib/lookups';

/** manifestsKey is the query key of a destination's manifest versions. */
export function manifestsKey(destinationId: number) {
  return ['destination', destinationId, 'manifests'] as const;
}

/**
 * ManifestsDialog is a destination's Manifests tab (design §16): its manifest versions with their
 * counts and integrity, verified downloads of each as JSON or CSV, "Export now", a preview (dry
 * run) and a manifest of the destination's current view built on the spot.
 */
export function ManifestsDialog({ destination, onClose }: { destination: Destination; onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const versions = useQuery({ queryKey: manifestsKey(destination.id), queryFn: () => listManifests(destination.id) });
  const [busy, setBusy] = useState<'export' | 'preview' | null>(null);
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);

  async function start(dryRun: boolean) {
    setNotice(null);
    setBusy(dryRun ? 'preview' : 'export');
    try {
      const job = await startManifestExport(destination.id, { dryRun });
      await qc.invalidateQueries({ queryKey: keys.jobs });
      if (dryRun) {
        navigate(`/activity/jobs/${job.id}`);
        return;
      }
      setNotice({
        tone: 'success',
        body: (
          <>
            Manifest export of {destination.name} queued.{' '}
            <Link className="text-accent hover:underline" to={`/activity/jobs/${job.id}`}>
              View job #{job.id}
            </Link>
          </>
        ),
      });
    } catch (e) {
      setNotice({ tone: 'error', body: `${destination.name}: ${errorMessage(e)}` });
    } finally {
      setBusy(null);
    }
  }

  const columns: Column<ManifestVersion>[] = [
    {
      key: 'created',
      header: 'Created',
      className: 'whitespace-nowrap',
      cell: (v) => <span title={formatRelative(v.createdAt)}>{formatDateTime(v.createdAt)}</span>,
    },
    { key: 'items', header: 'Items', className: 'text-right', cell: (v) => formatNumber(v.itemCount) },
    { key: 'files', header: 'Files', className: 'text-right', cell: (v) => formatNumber(v.fileCount) },
    { key: 'size', header: 'Listed size', className: 'whitespace-nowrap text-right', cell: (v) => formatBytes(v.bytes) },
    {
      key: 'integrity',
      header: 'Integrity',
      cell: (v) =>
        v.integrity === 'ok' ? (
          <Badge tone="ok">OK</Badge>
        ) : (
          <Badge tone="danger" title="The files were missing or did not match their checksums when read back">
            Damaged
          </Badge>
        ),
    },
    {
      key: 'download',
      header: 'Download',
      className: 'whitespace-nowrap',
      cell: (v) =>
        v.integrity === 'ok' ? (
          <span className="inline-flex gap-1">
            <DownloadLink small href={manifestDownloadUrl(v.id, 'json')} label={`Download the manifest of ${formatDateTime(v.createdAt)} as JSON`}>
              JSON
            </DownloadLink>
            <DownloadLink small href={manifestDownloadUrl(v.id, 'csv')} label={`Download the manifest of ${formatDateTime(v.createdAt)} as CSV`}>
              CSV
            </DownloadLink>
          </span>
        ) : (
          <span className="text-xs text-ink-muted">Not available</span>
        ),
    },
    {
      key: 'path',
      header: 'Path',
      cell: (v) => (
        <div>
          <code className="break-all text-xs">{v.path}</code>
          <div className="break-all font-mono text-xs text-ink-muted" title="sha256 of manifest.json">
            {v.checksum.replace(/^sha256:/, '').slice(0, 16)}…
          </div>
        </div>
      ),
    },
  ];

  return (
    <Modal
      title={`Manifests · ${destination.name}`}
      size="xl"
      onClose={onClose}
      footer={
        <>
          <Button icon={RefreshCw} onClick={() => void versions.refetch()}>
            Refresh
          </Button>
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        </>
      }
    >
      <p className="mb-3 text-xs text-ink-muted">
        A manifest lists every Sonarr, Radarr and Lidarr item and every library file, with what this destination holds of each, so a library can be
        acquired again after a disaster. Versions are written under <code>.bunkarr/manifests/</code> on this destination after full syncs (when
        *arr integrations exist) and whenever you export one; an unchanged manifest writes no new version. The JSON is canonical; the CSV is for
        spreadsheets. Downloads are checked against their checksums first.
      </p>
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Button variant="primary" icon={FileText} busy={busy === 'export'} disabled={!destination.enabled} onClick={() => void start(false)}>
          Export now
        </Button>
        <Button variant="ghost" icon={Eye} busy={busy === 'preview'} disabled={!destination.enabled} onClick={() => void start(true)}>
          Preview
        </Button>
        <ManifestExportLinks destinationId={destination.id} what="Current view" />
      </div>
      {!destination.enabled && <p className="mb-3 text-xs text-warn">The destination is disabled: enable it to write manifest versions.</p>}
      <ErrorNotice error={versions.error} />
      <DataTable
        columns={columns}
        rows={versions.data}
        rowKey={(v) => v.id}
        loading={versions.isPending}
        empty="No manifest versions on this destination yet."
        caption="Manifest versions"
      />
    </Modal>
  );
}
