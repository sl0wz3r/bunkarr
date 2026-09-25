import { X } from 'lucide-react';
import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { IconButton } from './Button';

const WIDTHS = { md: 'max-w-lg', lg: 'max-w-2xl', xl: 'max-w-4xl' };

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/**
 * Modal is an accessible dialog (role="dialog", labelled by its title) rendered over the page.
 * Escape and the close button call onClose; Tab stays inside the dialog; focus returns to where
 * it was when the dialog closes. Render it conditionally: `{open && <Modal …/>}`.
 */
export function Modal({
  title,
  onClose,
  children,
  footer,
  size = 'md',
}: {
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  size?: keyof typeof WIDTHS;
}) {
  const titleId = useId();
  const ref = useRef<HTMLDivElement>(null);
  // Where focus was before the dialog opened, read while rendering: by the time effects run, a
  // field with autoFocus inside the dialog has already taken it.
  const [previous] = useState(() => (document.activeElement instanceof HTMLElement ? document.activeElement : null));

  useEffect(() => {
    const el = ref.current;
    if (el && !el.contains(document.activeElement)) {
      el.focus();
    }
    return () => previous?.focus();
  }, [previous]);

  function onKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    if (e.key === 'Escape') {
      e.stopPropagation();
      onClose();
      return;
    }
    // A dialog opened over this one (a portal rendered inside it, such as PathPicker's folder
    // browser) passes its key events up here through React: it keeps Tab to itself.
    if (e.key !== 'Tab' || !ref.current || !ref.current.contains(e.target as Node)) {
      return;
    }
    const items = [...ref.current.querySelectorAll<HTMLElement>(FOCUSABLE)];
    if (items.length === 0) {
      return;
    }
    const first = items[0];
    const last = items[items.length - 1];
    // Focus on the dialog itself counts as the start: without this, Shift+Tab from there would
    // leave for the page behind.
    const active = document.activeElement;
    const atStart = active === ref.current;
    if (e.shiftKey && (atStart || active === first)) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && (atStart || active === last)) {
      e.preventDefault();
      first.focus();
    }
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/60 p-4 sm:py-12">
      {/* The dialog never outgrows the viewport: its body scrolls, the header (Close) and the
          footer (Save, Test) stay in view. */}
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        onKeyDown={onKeyDown}
        className={`flex max-h-[calc(100dvh-2rem)] w-full flex-col sm:max-h-[calc(100dvh-6rem)] ${WIDTHS[size]} rounded-lg border border-line bg-panel shadow-2xl outline-none`}
      >
        <div className="flex shrink-0 items-center justify-between border-b border-line px-5 py-3">
          <h2 id={titleId} className="text-base font-medium">
            {title}
          </h2>
          <IconButton label="Close" icon={X} onClick={onClose} />
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>
        {footer && <div className="flex shrink-0 flex-wrap items-center justify-end gap-2 border-t border-line px-5 py-3">{footer}</div>}
      </div>
    </div>,
    document.body,
  );
}
