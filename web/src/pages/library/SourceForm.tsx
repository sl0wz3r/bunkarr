import { useMutation, useQueryClient } from '@tanstack/react-query';
import { FlaskConical } from 'lucide-react';
import { useState, type FormEvent } from 'react';
import { createSource, testSource, updateSource } from '@/api/library';
import type { Source, SourceInput, SourceTestResult } from '@/api/types';
import { Button } from '@/components/Button';
import { CheckboxField, TextAreaField, TextField } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice, WarningList } from '@/components/Notice';
import { PathField } from '@/components/PathPicker';
import { Badge } from '@/components/StatusBadge';
import { formatNumber } from '@/lib/format';
import { keys } from '@/lib/lookups';

/** parseLines splits a text area into trimmed, non-empty lines. */
export function parseLines(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean);
}

/** SourceTestView shows the result of POST /sources/test. */
export function SourceTestView({ result }: { result: SourceTestResult }) {
  return (
    <div aria-label="Test result" role="group" className="mb-4">
      <Notice tone={result.ok ? 'success' : 'error'} reveal revealKey={result}>
        {result.message || (result.ok ? 'The folder is readable.' : 'The folder cannot be used.')}
      </Notice>
      <div className="mb-3 flex flex-wrap gap-2 text-xs">
        <Badge tone={result.exists ? 'ok' : 'danger'}>{result.exists ? 'Exists' : 'Missing'}</Badge>
        {result.exists && <Badge tone={result.isDir ? 'ok' : 'danger'}>{result.isDir ? 'Folder' : 'Not a folder'}</Badge>}
        {result.fsType && <Badge>{result.fsType}</Badge>}
        {result.fuse && (
          <Badge tone="warn" title="Unraid /mnt/user: inode numbers are only stable with the 'support Hard Links' tunable on">
            FUSE
          </Badge>
        )}
        {result.exists && <Badge>{formatNumber(result.entries)} entries</Badge>}
      </div>
      <WarningList warnings={result.warnings} />
    </div>
  );
}

/**
 * SourceForm adds or edits a source: a folder Bunkarr reads (never writes) and mirrors to
 * each destination linked to it under its destination folder.
 */
export function SourceForm({ source, onClose }: { source: Source | null; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(source?.name ?? '');
  const [path, setPath] = useState(source?.path ?? '');
  const [destFolder, setDestFolder] = useState(source?.destFolder ?? '');
  const [exclude, setExclude] = useState((source?.exclude ?? []).join('\n'));
  const [enabled, setEnabled] = useState(source?.enabled ?? true);
  const [test, setTest] = useState<{ path: string; result: SourceTestResult } | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  // Counts Save clicks, so a repeated form error is shown (scrolled into view) again.
  const [attempt, setAttempt] = useState(0);

  const tester = useMutation({ mutationFn: (p: string) => testSource(p), onSuccess: (result, p) => setTest({ path: p, result }) });
  const saver = useMutation({
    mutationFn: (body: SourceInput) => (source ? updateSource(source.id, body) : createSource(body)),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: keys.sources });
      await qc.invalidateQueries({ queryKey: keys.catalogStats });
      onClose();
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    setAttempt((n) => n + 1);
    if (!name.trim() || !path.trim()) {
      setFormError('Enter a name and a path.');
      return;
    }
    const body: SourceInput = { name: name.trim(), path: path.trim(), exclude: parseLines(exclude), enabled };
    if (destFolder.trim()) {
      body.destFolder = destFolder.trim();
    }
    // Keep the links a source has (PUT replaces them: leaving them out clears them).
    if (source?.plexIntegrationId != null) {
      body.plexIntegrationId = source.plexIntegrationId;
      body.plexSectionId = source.plexSectionId;
      body.plexPath = source.plexPath;
    }
    if (source?.arrIntegrationId != null) {
      body.arrIntegrationId = source.arrIntegrationId;
    }
    saver.mutate(body);
  }

  const shownTest = test && test.path === path.trim() ? test.result : null;
  return (
    <Modal
      title={source ? `Edit source · ${source.name}` : 'Add source'}
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button icon={FlaskConical} busy={tester.isPending} disabled={!path.trim()} onClick={() => tester.mutate(path.trim())}>
            Test
          </Button>
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="source-form" busy={saver.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="source-form" onSubmit={submit} noValidate>
        {formError && (
          <Notice key={attempt} tone="error">
            {formError}
          </Notice>
        )}
        <ErrorNotice error={saver.error ?? tester.error} />
        {shownTest && <SourceTestView result={shownTest} />}
        <TextField label="Name" value={name} onChange={setName} autoFocus={!source} placeholder="Movies" />
        <PathField
          label="Path"
          value={path}
          onChange={setPath}
          placeholder="/media/movies"
          help="The folder as Bunkarr sees it inside the container. Bunkarr only reads it and never follows symlinks. Mount the media parent read-only as one volume so hardlinks can be detected; on Unraid prefer a pool or disk path (/mnt/cache/…, /mnt/diskN/…) over /mnt/user."
        />
        <TextField
          label="Destination folder"
          value={destFolder}
          onChange={setDestFolder}
          mono
          placeholder={source ? '' : 'Derived from the name'}
          help="The folder under each destination that mirrors this source. It cannot change once a destination holds files for it. Switching from rsync? Use the name of the existing folder so matching files are adopted instead of copied again."
        />
        <TextAreaField
          label="Exclude"
          value={exclude}
          onChange={setExclude}
          mono
          rows={3}
          placeholder={'*.partial\n.recycle/**'}
          help="Glob patterns, one per line, matched against the relative path and the file name."
        />
        <CheckboxField label="Enabled" text="Include this source in scans and syncs" checked={enabled} onChange={setEnabled} />
        {source?.plexPath && (
          <p className="text-xs text-ink-muted">
            Imported from Plex: <code>{source.plexPath}</code>
          </p>
        )}
      </form>
    </Modal>
  );
}
