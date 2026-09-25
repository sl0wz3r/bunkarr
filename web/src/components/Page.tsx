import type { ComponentType, ReactNode } from 'react';

/** Page is the *arr page frame: a header bar with the title and toolbar actions, then content. */
export function Page({ title, actions, children }: { title: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <div>
      <div className="flex min-h-12 flex-wrap items-center justify-between gap-2 border-b border-line bg-panel px-5 py-2">
        <h1 className="text-base font-medium">{title}</h1>
        <div className="flex flex-wrap items-center gap-1">{actions}</div>
      </div>
      <div className="p-5">{children}</div>
    </div>
  );
}

export function EmptyState({ icon: Icon, title, children }: { icon: ComponentType<{ className?: string }>; title: string; children: ReactNode }) {
  return (
    <div className="mx-auto mt-16 max-w-md text-center text-ink-muted">
      <Icon className="mx-auto mb-4 h-12 w-12 opacity-50" />
      <h2 className="mb-2 text-lg text-ink">{title}</h2>
      <div className="text-sm">{children}</div>
    </div>
  );
}

export function Section({ title, children, actions }: { title: string; children: ReactNode; actions?: ReactNode }) {
  return (
    <section className="mb-8 max-w-3xl">
      <div className="mb-3 flex items-center justify-between border-b border-line pb-2">
        <h2 className="text-lg">{title}</h2>
        {actions}
      </div>
      {children}
    </section>
  );
}

/** Stat is one figure in a StatGrid ("Files 12,345"). */
export function Stat({ label, value, hint, muted }: { label: string; value: ReactNode; hint?: string; muted?: boolean }) {
  return (
    <div className="min-w-0 rounded border border-line bg-panel px-3 py-2" title={hint}>
      <dt className="text-xs text-ink-muted">{label}</dt>
      <dd className={`break-words text-base font-medium ${muted ? 'text-ink-muted' : ''}`}>{value}</dd>
    </div>
  );
}

export function StatGrid({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <div role="group" aria-label={label} className="mb-5">
      <dl className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-6">{children}</dl>
    </div>
  );
}
