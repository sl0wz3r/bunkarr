import { useQuery, useQueryClient } from '@tanstack/react-query';
import { CheckCircle2, XCircle } from 'lucide-react';
import { useState } from 'react';
import { Link } from 'react-router';
import { errorMessage } from '@/api/client';
import { plexSections } from '@/api/integrations';
import { createSource } from '@/api/library';
import type { PlexLocation, PlexSection, Source } from '@/api/types';
import { Button } from '@/components/Button';
import { Checkbox, FormRow, inputClass } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Badge } from '@/components/StatusBadge';
import { keys, useIntegrations, useSources } from '@/lib/lookups';

interface Candidate {
  id: string;
  section: PlexSection;
  location: PlexLocation;
  defaultName: string;
  /** Why it cannot be imported, or null. */
  blocked: string | null;
}

function baseName(p: string): string {
  const parts = p.split('/').filter(Boolean);
  return parts[parts.length - 1] ?? p;
}

function candidates(sections: PlexSection[], sources: Source[], integrationId: number): Candidate[] {
  const out: Candidate[] = [];
  for (const section of sections) {
    for (const location of section.locations ?? []) {
      const existing = sources.find(
        (s) =>
          (location.localPath && s.path === location.localPath) ||
          (s.plexIntegrationId === integrationId && s.plexSectionId === section.key && s.plexPath === location.path),
      );
      let blocked: string | null = null;
      if (existing) {
        blocked = `Already a source (${existing.name})`;
      } else if (!location.localPath) {
        blocked = 'No path mapping covers this folder';
      } else if (!location.exists) {
        blocked = 'Not found inside Bunkarr';
      }
      out.push({
        id: `${section.key}|${location.path}`,
        section,
        location,
        defaultName: section.locations.length > 1 ? `${section.title} (${baseName(location.path)})` : section.title,
        blocked,
      });
    }
  }
  return out;
}

/**
 * PlexImport creates sources from a Plex server's library sections: each section location is
 * translated to Bunkarr's view of the folder through the server's path mappings.
 */
export function PlexImport({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const integrations = useIntegrations();
  const sources = useSources();
  const servers = (integrations.data ?? []).filter((i) => i.type === 'plex');
  const [chosen, setChosen] = useState<number | null>(null);
  const integrationId = chosen ?? servers[0]?.id ?? null;
  const sections = useQuery({
    queryKey: ['integrations', integrationId, 'plex', 'sections'],
    queryFn: () => plexSections(integrationId as number),
    enabled: integrationId != null,
    // A 502 means Plex did not answer: show that at once instead of retrying for seconds.
    retry: false,
  });
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [names, setNames] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [results, setResults] = useState<Record<string, string | null>>({});

  const list = integrationId != null && sections.data ? candidates(sections.data, sources.data ?? [], integrationId) : [];
  const nameOf = (c: Candidate) => names[c.id] ?? c.defaultName;

  function toggle(id: string, on: boolean) {
    const next = new Set(selected);
    if (on) next.add(id);
    else next.delete(id);
    setSelected(next);
  }

  async function importSelected() {
    if (integrationId == null) return;
    setBusy(true);
    const outcome: Record<string, string | null> = {};
    for (const c of list.filter((x) => selected.has(x.id) && !x.blocked)) {
      try {
        await createSource({
          name: nameOf(c).trim(),
          path: c.location.localPath,
          exclude: [],
          enabled: true,
          plexIntegrationId: integrationId,
          plexSectionId: c.section.key,
          plexPath: c.location.path,
        });
        outcome[c.id] = null;
      } catch (e) {
        outcome[c.id] = errorMessage(e);
      }
    }
    setResults(outcome);
    setBusy(false);
    await qc.invalidateQueries({ queryKey: keys.sources });
    await qc.invalidateQueries({ queryKey: keys.catalogStats });
    const failed = Object.values(outcome).some((v) => v !== null);
    if (!failed) {
      onClose();
    } else {
      setSelected(new Set(Object.entries(outcome).filter(([, v]) => v !== null).map(([k]) => k)));
    }
  }

  const count = list.filter((c) => selected.has(c.id) && !c.blocked).length;
  return (
    <Modal
      title="Import from Plex"
      size="xl"
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" busy={busy} disabled={count === 0} onClick={() => void importSelected()}>
            {count > 1 ? `Create ${count} sources` : 'Create source'}
          </Button>
        </>
      }
    >
      <ErrorNotice error={integrations.error ?? sections.error} />
      {integrations.data && servers.length === 0 ? (
        <Notice tone="info">
          No Plex server is set up yet. Add one in{' '}
          <Link to="/settings/plex" className="text-accent hover:underline" onClick={onClose}>
            Settings → Plex
          </Link>
          , with path mappings from Plex's paths to Bunkarr's.
        </Notice>
      ) : (
        <>
          <FormRow label="Plex server" htmlFor="plex-import-server">
            <select
              id="plex-import-server"
              className={`${inputClass} max-w-md`}
              value={integrationId ?? ''}
              onChange={(e) => {
                setChosen(Number(e.target.value));
                setSelected(new Set());
                setResults({});
              }}
            >
              {servers.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name}
                </option>
              ))}
            </select>
          </FormRow>
          <p className="mb-3 text-xs text-ink-muted">
            Each library folder is shown as Plex sees it and as Bunkarr sees it after the server's path mappings. Folders Bunkarr cannot see need a mapping (Settings →
            Plex) or a volume mount.
          </p>
          {sections.isPending && integrationId != null && <p className="text-sm text-ink-muted">Loading libraries…</p>}
          {sections.data && list.length === 0 && <p className="text-sm text-ink-muted">This server has no library folders.</p>}
          {list.length > 0 && (
            <div className="relative overflow-x-auto rounded border border-line">
              <table className="w-full text-sm">
                <caption className="sr-only">Plex library folders</caption>
                <thead>
                  <tr className="border-b border-line text-left text-ink-muted">
                    <th scope="col" className="px-3 py-2 font-medium">
                      <span className="sr-only">Import</span>
                    </th>
                    <th scope="col" className="px-3 py-2 font-medium">
                      Library
                    </th>
                    <th scope="col" className="px-3 py-2 font-medium">
                      Plex path → Bunkarr path
                    </th>
                    <th scope="col" className="px-3 py-2 font-medium">
                      Source name
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((c) => (
                    <tr key={c.id} className="border-b border-line/60 last:border-b-0 align-top">
                      <td className="px-3 py-2">
                        <Checkbox
                          label={<span className="sr-only">Import {c.location.path}</span>}
                          checked={selected.has(c.id) && !c.blocked}
                          disabled={!!c.blocked}
                          onChange={(on) => toggle(c.id, on)}
                        />
                      </td>
                      <td className="px-3 py-2">
                        <div>{c.section.title}</div>
                        <Badge>{c.section.type}</Badge>
                      </td>
                      <td className="px-3 py-2 font-mono text-xs">
                        <div className="break-all text-ink-muted">{c.location.path}</div>
                        <div className="flex items-center gap-1 break-all">
                          {c.location.localPath ? (
                            <>
                              {c.location.exists ? (
                                <CheckCircle2 className="h-3.5 w-3.5 shrink-0 text-accent" aria-label="Found" />
                              ) : (
                                <XCircle className="h-3.5 w-3.5 shrink-0 text-danger" aria-label="Not found" />
                              )}
                              → {c.location.localPath}
                            </>
                          ) : (
                            <span className="text-danger">→ no mapping</span>
                          )}
                        </div>
                        {c.blocked && <div className="mt-1 font-sans text-warn">{c.blocked}</div>}
                        {results[c.id] && <div className="mt-1 font-sans text-danger">{results[c.id]}</div>}
                      </td>
                      <td className="px-3 py-2">
                        <input
                          aria-label={`Source name for ${c.location.path}`}
                          className={inputClass}
                          value={nameOf(c)}
                          disabled={!!c.blocked}
                          onChange={(e) => setNames({ ...names, [c.id]: e.target.value })}
                        />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </Modal>
  );
}
