import { FileDown, Loader2 } from 'lucide-react';
import { useRef, useState, type MouseEvent, type ReactNode } from 'react';
import { errorMessage } from '@/api/client';
import { fetchManifestFile, manifestExportUrl, type ManifestFormat } from '@/api/manifests';

/** saveBlob hands blob to the browser as a download named filename. */
export function saveBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.rel = 'noopener';
  a.style.display = 'none';
  document.body.appendChild(a);
  try {
    a.click();
  } finally {
    a.remove();
    // Some browsers read the object URL after click() returns.
    window.setTimeout(() => URL.revokeObjectURL(url), 60_000);
  }
}

/**
 * DownloadLink is a link styled like a ghost button that saves what it points to. A click fetches
 * the file first: an error answer (429 too many exports, 409 a damaged version, 500) is shown
 * here instead of being saved as the file, and the link waits while a download is in flight.
 */
export function DownloadLink({ href, children, label, small }: { href: string; children: ReactNode; label?: string; small?: boolean }) {
  const size = small ? 'px-2 py-1 text-xs gap-1' : 'px-3 py-1.5 text-sm gap-2';
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inFlight = useRef(false);

  async function download(e: MouseEvent<HTMLAnchorElement>) {
    // A modified click (new tab, save link as) stays the browser's.
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
      return;
    }
    e.preventDefault();
    if (inFlight.current) {
      return;
    }
    inFlight.current = true;
    setBusy(true);
    setError(null);
    try {
      const file = await fetchManifestFile(href);
      saveBlob(file.blob, file.filename);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  }

  return (
    <span className="inline-flex flex-col items-start">
      <a
        href={href}
        download
        aria-label={label}
        title={label}
        aria-disabled={busy || undefined}
        aria-busy={busy || undefined}
        onClick={(e) => void download(e)}
        className={`inline-flex items-center justify-center whitespace-nowrap rounded ${size} text-ink hover:bg-panel-2 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent ${busy ? 'cursor-progress opacity-60' : ''}`}
      >
        {busy ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" /> : <FileDown className="h-4 w-4" aria-hidden="true" />}
        {children}
      </a>
      {error && (
        <span role="alert" className="max-w-xs whitespace-normal px-2 text-xs text-danger">
          {error}
        </span>
      )}
    </span>
  );
}

/**
 * ManifestExportLinks offer a manifest built on the spot (design §11.2), as JSON (canonical) and
 * as CSV (for a spreadsheet): of every enabled source, or of one destination's view.
 */
export function ManifestExportLinks({ destinationId, what = 'Export manifest' }: { destinationId?: number; what?: string }) {
  const formats: ManifestFormat[] = ['json', 'csv'];
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {formats.map((f) => (
        <DownloadLink key={f} href={manifestExportUrl(f, destinationId)} label={`${what} as ${f.toUpperCase()}`}>
          {what} ({f.toUpperCase()})
        </DownloadLink>
      ))}
    </span>
  );
}
