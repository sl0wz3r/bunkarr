import type { FormEvent, ReactNode } from 'react';
import { Logo } from './Logo';

// Centered card used by the login and first-run setup pages.
export function AuthCard(props: { title: string; subtitle?: string; onSubmit: (e: FormEvent) => void; children: ReactNode }) {
  return (
    <div className="flex min-h-full items-center justify-center p-4">
      <form onSubmit={props.onSubmit} className="w-full max-w-sm rounded-lg border border-line bg-panel p-6 shadow-xl">
        <div className="mb-6 flex items-center gap-3">
          <Logo className="h-10 w-10" />
          <div>
            <h1 className="text-xl font-semibold">{props.title}</h1>
            {props.subtitle && <p className="text-sm text-ink-muted">{props.subtitle}</p>}
          </div>
        </div>
        <div className="space-y-4">{props.children}</div>
      </form>
    </div>
  );
}

export function Field(props: {
  label: string;
  type?: string;
  value: string;
  onChange: (v: string) => void;
  autoComplete?: string;
  autoFocus?: boolean;
}) {
  return (
    <label className="block">
      <span className="mb-1 block text-sm text-ink-muted">{props.label}</span>
      <input
        className="w-full rounded border border-line bg-page px-3 py-2 outline-none focus:border-accent"
        type={props.type ?? 'text'}
        value={props.value}
        autoComplete={props.autoComplete}
        autoFocus={props.autoFocus}
        onChange={(e) => props.onChange(e.target.value)}
      />
    </label>
  );
}

export function SubmitButton(props: { busy: boolean; children: ReactNode }) {
  return (
    <button
      type="submit"
      disabled={props.busy}
      className="w-full rounded bg-accent px-4 py-2 font-medium text-page hover:bg-accent-strong disabled:opacity-60"
    >
      {props.busy ? 'Please wait…' : props.children}
    </button>
  );
}
