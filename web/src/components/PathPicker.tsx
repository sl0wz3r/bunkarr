import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { ArrowUp, Folder, FolderOpen } from 'lucide-react';
import { useId, useState, type ReactNode } from 'react';
import { browse } from '@/api/library';
import { Button } from './Button';
import { FormRow, inputClass } from './Form';
import { Modal } from './Modal';
import { ErrorNotice } from './Notice';

/**
 * PathPicker is a path input with a "Browse" button that opens a folder browser over
 * GET /filesystem (read-only: it lists directories and never creates anything).
 */
export function PathPicker({
  id,
  label,
  value,
  onChange,
  placeholder,
  disabled,
}: {
  id?: string;
  label: string;
  value: string;
  onChange: (path: string) => void;
  placeholder?: string;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  return (
    <div className="flex gap-2">
      <input
        id={id}
        aria-label={id ? undefined : label}
        className={`${inputClass} font-mono`}
        value={value}
        placeholder={placeholder}
        disabled={disabled}
        autoComplete="off"
        spellCheck={false}
        onChange={(e) => onChange(e.target.value)}
      />
      <Button icon={FolderOpen} aria-label={`Browse for ${label}`} title="Browse" disabled={disabled} onClick={() => setOpen(true)}>
        <span className="hidden sm:inline">Browse</span>
      </Button>
      {open && (
        <FolderBrowser
          title={`Choose ${label.toLowerCase()}`}
          start={value}
          onClose={() => setOpen(false)}
          onSelect={(p) => {
            onChange(p);
            setOpen(false);
          }}
        />
      )}
    </div>
  );
}

/** PathField is a PathPicker in a labelled form row. */
export function PathField({
  label,
  value,
  onChange,
  help,
  placeholder,
  disabled,
  after,
}: {
  label: string;
  value: string;
  onChange: (path: string) => void;
  help?: ReactNode;
  placeholder?: string;
  disabled?: boolean;
  after?: ReactNode;
}) {
  const id = useId();
  return (
    <FormRow label={label} htmlFor={id} help={help}>
      <PathPicker id={id} label={label} value={value} onChange={onChange} placeholder={placeholder} disabled={disabled} />
      {after}
    </FormRow>
  );
}

function FolderBrowser({ title, start, onSelect, onClose }: { title: string; start: string; onSelect: (path: string) => void; onClose: () => void }) {
  const [path, setPath] = useState(start.trim());
  // Keep showing the previous folder while the next one loads, so the dialog does not jump.
  const listing = useQuery({ queryKey: ['filesystem', path], queryFn: () => browse(path), retry: false, placeholderData: keepPreviousData });
  const loading = listing.isPending || listing.isPlaceholderData;
  const current = listing.isPlaceholderData ? path : (listing.data?.path ?? path);
  const dirs = listing.data?.directories ?? [];

  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={!listing.data || loading} onClick={() => onSelect(current)}>
            Select this folder
          </Button>
        </>
      }
    >
      <div className="mb-3 flex items-center gap-2">
        <Button icon={ArrowUp} small disabled={loading || !listing.data?.parent} onClick={() => setPath(listing.data?.parent ?? '')}>
          Up
        </Button>
        <code className="min-w-0 flex-1 truncate rounded bg-page px-2 py-1 text-xs" title={current}>
          {current || '/'}
        </code>
      </div>
      {listing.error ? (
        <>
          <ErrorNotice error={listing.error} />
          {path !== '' && (
            <Button small onClick={() => setPath('')}>
              Start from the top
            </Button>
          )}
        </>
      ) : listing.isPending ? (
        <p className="text-sm text-ink-muted">Loading…</p>
      ) : dirs.length === 0 ? (
        <p className="text-sm text-ink-muted">No folders here.</p>
      ) : (
        <>
          <ul aria-label="Folders" aria-busy={loading} className={`max-h-80 overflow-y-auto rounded border border-line ${loading ? 'opacity-60' : ''}`}>
            {dirs.map((d) => (
              <li key={d.path}>
                <button
                  type="button"
                  className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm hover:bg-panel-2"
                  disabled={loading}
                  onClick={() => setPath(d.path)}
                >
                  <Folder className="h-4 w-4 shrink-0 text-accent" aria-hidden="true" />
                  <span className="truncate">{d.name}</span>
                </button>
              </li>
            ))}
          </ul>
          {listing.data?.truncated && (
            <p className="mt-2 text-xs text-ink-muted">Only the first {dirs.length.toLocaleString()} folders are listed; type the path to go deeper.</p>
          )}
        </>
      )}
    </Modal>
  );
}
