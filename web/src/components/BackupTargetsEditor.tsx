import { Plus, Trash2 } from 'lucide-react';
import { useId } from 'react';
import type { BackupTarget, Destination } from '@/api/types';
import { type CronPreset } from '@/lib/cron';
import { KIND_BADGES, isEncrypted, isRemoteKind } from '@/lib/destinationKinds';
import { Button, IconButton } from './Button';
import { Checkbox, inputClass } from './Form';
import { CronInput } from './CronInput';
import { Badge } from './StatusBadge';

// The backup targets of a Plex DB or *arr backup (docs/design/phase4.md §8.5, §15): up to four
// destinations, each with its own schedule and enabled flag, and acceptInsecureModes only where it
// can apply (a local destination whose probe says it does not keep file modes). A remote
// destination without encryption is refused by the server (the backup holds the application's
// credentials); the editor says so on the row, and shows the server's 400 there too.

export const MAX_BACKUP_TARGETS = 4;

/** insecureModesApply reports whether acceptInsecureModes can matter for a destination (S17 for engines). */
export function insecureModesApply(d: Destination | undefined): boolean {
  return !!d && !isRemoteKind(d.kind) && !isEncrypted(d) && d.capabilities?.enforcesModes !== true;
}

/** targetsOf reads a backup's targets: its targets list, or the single form's one target. */
export function targetsOf(backup: { destinationId?: number; cron?: string; enabled?: boolean; acceptInsecureModes?: boolean; targets?: BackupTarget[] } | undefined): BackupTarget[] {
  if (!backup) return [];
  if (Array.isArray(backup.targets)) return backup.targets.map((t) => ({ ...t, acceptInsecureModes: !!t.acceptInsecureModes }));
  if (backup.destinationId && backup.destinationId > 0) {
    return [{ destinationId: backup.destinationId, cron: backup.cron ?? '', enabled: !!backup.enabled, acceptInsecureModes: !!backup.acceptInsecureModes }];
  }
  return [];
}

/** targetsProblem checks the targets as the server does, or null. */
export function targetsProblem(targets: BackupTarget[], validate: (cron: string) => string | null): string | null {
  if (targets.length > MAX_BACKUP_TARGETS) return `At most ${MAX_BACKUP_TARGETS} backup targets.`;
  const seen = new Set<number>();
  for (const [i, t] of targets.entries()) {
    if (!t.destinationId) return `Target ${i + 1}: choose a destination.`;
    if (seen.has(t.destinationId)) return `Target ${i + 1}: that destination is a target already.`;
    seen.add(t.destinationId);
    const err = t.enabled || t.cron.trim() ? validate(t.cron.trim()) : null;
    if (err) return `Target ${i + 1}: the schedule is invalid: ${err}`;
  }
  return null;
}

/**
 * targetsNeedPassword mirrors the server's S29 check of an integration save: a target on a
 * destination that is not local and was not a target before, or acceptInsecureModes turned on.
 */
export function targetsNeedPassword(targets: BackupTarget[], before: BackupTarget[], destinations: Destination[] | undefined): boolean {
  for (const t of targets) {
    const old = before.find((b) => b.destinationId === t.destinationId);
    if (t.acceptInsecureModes && !old?.acceptInsecureModes) return true;
    if (old) continue;
    const d = destinations?.find((x) => x.id === t.destinationId);
    if (d && isRemoteKind(d.kind)) return true;
  }
  return false;
}

/** targetIndexOf finds "backup.targets[2]" in a server message (the row it is about), or -1. */
export function targetIndexOf(message: string): number {
  const m = /targets\[(\d+)\]/.exec(message);
  return m ? Number(m[1]) : -1;
}

/**
 * BackupTargetsEditor lists and edits the targets. app names the application whose credentials the
 * backup holds ("Plex", "Sonarr"); rowError is the server's message for one row (index), if any.
 */
export function BackupTargetsEditor({
  targets,
  onChange,
  destinations,
  presets,
  defaultCron,
  app,
  offset = 0,
  rowError,
}: {
  targets: BackupTarget[];
  onChange: (t: BackupTarget[]) => void;
  destinations: Destination[];
  presets: CronPreset[];
  defaultCron: string;
  app: string;
  /** Targets shown before this list (the form's first one): numbering starts after them. */
  offset?: number;
  rowError?: { index: number; message: string } | null;
}) {
  const baseId = useId();
  const set = (i: number, patch: Partial<BackupTarget>) => onChange(targets.map((t, j) => (j === i ? { ...t, ...patch } : t)));
  const free = destinations.filter((d) => !targets.some((t) => t.destinationId === d.id));
  return (
    <div>
      {targets.length === 0 && offset === 0 && <p className="mb-2 text-sm text-ink-muted">No backup target: {app} is not backed up.</p>}
      <ul className="space-y-3" aria-label="Backup targets">
        {targets.map((t, i) => {
          const d = destinations.find((x) => x.id === t.destinationId);
          const unencryptedRemote = !!d && isRemoteKind(d.kind) && !isEncrypted(d);
          const insecure = insecureModesApply(d);
          const n = i + 1 + offset;
          const selectId = `${baseId}-dest-${i}`;
          const err = rowError && rowError.index === i ? rowError.message : null;
          return (
            <li key={i}>
              {/* The group is inside the list item, so the list keeps its items for screen readers. */}
              <div role="group" aria-label={`Backup target ${n}`} className={`rounded border p-3 ${err ? 'border-danger/60' : 'border-line/70'}`}>
                <div className="flex flex-wrap items-center gap-2">
                  <label htmlFor={selectId} className="text-sm font-medium">
                    Target {n}
                  </label>
                  <select
                    id={selectId}
                    aria-label={`Destination of target ${n}`}
                    className={`${inputClass} w-full sm:w-64`}
                    value={String(t.destinationId)}
                    onChange={(e) => set(i, { destinationId: Number(e.target.value), acceptInsecureModes: false })}
                  >
                    <option value="0">Choose a destination</option>
                    {destinations
                      .filter((x) => x.id === t.destinationId || free.includes(x))
                      .map((x) => (
                        <option key={x.id} value={x.id}>
                          {x.name}
                        </option>
                      ))}
                  </select>
                  {d && <Badge>{KIND_BADGES[d.kind] ?? d.kind}</Badge>}
                  {d && isEncrypted(d) && <Badge tone="ok">Encrypted</Badge>}
                  <IconButton label={`Remove backup target ${n}`} icon={Trash2} className="ml-auto hover:text-danger" onClick={() => onChange(targets.filter((_, j) => j !== i))} />
                </div>
                {unencryptedRemote && (
                  <p className="mt-2 text-xs text-danger">
                    {d.name} is off this server and not encrypted: the backup holds {app}&apos;s credentials, so Bunkarr refuses it there. Choose an encrypted
                    destination.
                  </p>
                )}
                <div className="mt-2">
                  <CronInput label={`Schedule of target ${n}`} value={t} onChange={(v) => set(i, { cron: v.cron, enabled: v.enabled })} presets={presets} allowManual={false} />
                  <div className="-mt-2 sm:ml-[12rem]">
                    <Checkbox label={`Scheduled backups to target ${n}`} checked={t.enabled} onChange={(enabled) => set(i, { enabled, cron: t.cron || defaultCron })} />
                  </div>
                </div>
                {(insecure || t.acceptInsecureModes) && (
                  <div className="mt-2 rounded border border-warn/50 bg-warn/10 p-2 text-xs">
                    <Checkbox
                      label="Accept insecure file modes on this destination"
                      help={`${d?.name ?? 'This destination'} does not keep files private (an SMB share without POSIX extensions), or was probed before Bunkarr checked it. The backup holds ${app}'s credentials. Needs your password.`}
                      checked={t.acceptInsecureModes}
                      onChange={(acceptInsecureModes) => set(i, { acceptInsecureModes })}
                    />
                  </div>
                )}
                {err && (
                  <p role="alert" className="mt-2 text-xs text-danger">
                    {err}
                  </p>
                )}
              </div>
            </li>
          );
        })}
      </ul>
      <div className="mt-2">
        <Button
          small
          icon={Plus}
          disabled={targets.length + offset >= MAX_BACKUP_TARGETS || free.length === 0}
          title={targets.length + offset >= MAX_BACKUP_TARGETS ? `At most ${MAX_BACKUP_TARGETS} targets` : free.length === 0 ? 'Every destination is a target already' : undefined}
          onClick={() => onChange([...targets, { destinationId: free[0]?.id ?? 0, cron: defaultCron, enabled: true, acceptInsecureModes: false }])}
        >
          Add a backup target
        </Button>
      </div>
    </div>
  );
}
