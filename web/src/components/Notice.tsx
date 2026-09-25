import { AlertTriangle, CheckCircle2, Info, XCircle } from 'lucide-react';
import { useEffect, useRef, type ReactNode } from 'react';
import { errorMessage } from '@/api/client';

export type NoticeTone = 'info' | 'success' | 'warning' | 'error';

const TONES: Record<NoticeTone, { box: string; icon: typeof Info }> = {
  info: { box: 'border-info/40 bg-info/10', icon: Info },
  success: { box: 'border-accent/40 bg-accent/10', icon: CheckCircle2 },
  warning: { box: 'border-warn/50 bg-warn/10', icon: AlertTriangle },
  error: { box: 'border-danger/50 bg-danger/10 text-danger', icon: XCircle },
};

/**
 * useReveal scrolls the element into view when it appears and whenever key changes. A message at
 * the top of a long dialog (a form error, a test result) would otherwise stay out of sight after
 * the user clicked Save or Test in the dialog's footer.
 */
export function useReveal<T extends HTMLElement>(key: unknown, enabled = true) {
  const ref = useRef<T>(null);
  useEffect(() => {
    if (enabled) {
      // Optional call: jsdom (tests) has no scrollIntoView.
      ref.current?.scrollIntoView?.({ block: 'nearest', behavior: 'smooth' });
    }
  }, [key, enabled]);
  return ref;
}

/**
 * Notice is an inline message box; errors are announced (role="alert"), others politely. Errors,
 * and notices with `reveal`, scroll themselves into view when they appear and whenever revealKey
 * (default: their text) changes; pass a new revealKey (an attempt counter, a new result object)
 * to reveal the same message again.
 */
export function Notice({
  tone = 'info',
  title,
  children,
  className = '',
  reveal,
  revealKey,
}: {
  tone?: NoticeTone;
  title?: ReactNode;
  children?: ReactNode;
  className?: string;
  reveal?: boolean;
  revealKey?: unknown;
}) {
  const { box, icon: Icon } = TONES[tone];
  const text = typeof children === 'string' ? children : typeof title === 'string' ? title : null;
  const ref = useReveal<HTMLDivElement>(revealKey ?? text, reveal ?? tone === 'error');
  return (
    <div
      ref={ref}
      role={tone === 'error' ? 'alert' : 'status'}
      className={`mb-4 flex scroll-my-4 gap-3 rounded border px-3 py-2 text-sm ${box} ${className}`}
    >
      <Icon className="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
      <div className="min-w-0 flex-1 break-words">
        {title && <div className="font-medium">{title}</div>}
        {children}
      </div>
    </div>
  );
}

/** ErrorNotice shows an error when there is one. */
export function ErrorNotice({ error, className }: { error: unknown; className?: string }) {
  if (!error) {
    return null;
  }
  return (
    <Notice tone="error" className={className}>
      {errorMessage(error)}
    </Notice>
  );
}

/** WarningList renders server warnings (source/destination tests) as a list. */
export function WarningList({ warnings }: { warnings: string[] | null | undefined }) {
  if (!warnings || warnings.length === 0) {
    return null;
  }
  return (
    <Notice tone="warning" title={warnings.length === 1 ? 'Warning' : `${warnings.length} warnings`}>
      <ul className="list-disc space-y-1 pl-4">
        {warnings.map((w) => (
          <li key={w}>{w}</li>
        ))}
      </ul>
    </Notice>
  );
}
