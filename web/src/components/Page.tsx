import type { ComponentType, ReactNode } from 'react';

export function Page({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <div>
      <div className="flex h-12 items-center justify-between border-b border-line bg-panel px-5">
        <h1 className="text-base font-medium">{title}</h1>
        <div className="flex items-center gap-2">{actions}</div>
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
      <p className="text-sm">{children}</p>
    </div>
  );
}

export function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="mb-8 max-w-3xl">
      <h2 className="mb-3 border-b border-line pb-2 text-lg">{title}</h2>
      {children}
    </section>
  );
}
