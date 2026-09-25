/**
 * ProgressBar shows a 0..1 fraction; null means "unknown yet" (an animated sliver). The label
 * is the accessible name ("Progress of Sync · NAS").
 */
export function ProgressBar({ value, label, className = '' }: { value: number | null; label: string; className?: string }) {
  const pct = value == null ? null : Math.max(0, Math.min(100, value * 100));
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={pct == null ? undefined : Math.floor(pct)}
      className={`h-2 w-full overflow-hidden rounded bg-panel-2 ${className}`}
    >
      {pct == null ? <div className="h-full w-1/4 animate-pulse rounded bg-info/60" /> : <div className="h-full rounded bg-accent transition-[width]" style={{ width: `${pct}%` }} />}
    </div>
  );
}
