import { Loader2 } from 'lucide-react';
import type { ButtonHTMLAttributes, ComponentType } from 'react';

export type ButtonVariant = 'primary' | 'secondary' | 'danger' | 'ghost';

const VARIANTS: Record<ButtonVariant, string> = {
  primary: 'bg-accent font-medium text-page hover:bg-accent-strong',
  secondary: 'bg-panel-2 text-ink hover:bg-line',
  danger: 'bg-danger/90 font-medium text-white hover:bg-danger',
  ghost: 'text-ink hover:bg-panel-2',
};

type Props = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
  icon?: ComponentType<{ className?: string }>;
  /** busy shows a spinner and disables the button while an action runs. */
  busy?: boolean;
  small?: boolean;
};

/** Button is the app's button; type defaults to "button" so it never submits a form by accident. */
export function Button({ variant = 'secondary', icon: Icon, busy, small, className = '', type = 'button', disabled, children, ...rest }: Props) {
  const size = small ? 'px-2 py-1 text-xs gap-1' : 'px-3 py-1.5 text-sm gap-2';
  return (
    <button
      type={type}
      disabled={disabled || busy}
      className={`inline-flex items-center justify-center whitespace-nowrap rounded ${size} ${VARIANTS[variant]} disabled:cursor-not-allowed disabled:opacity-50 ${className}`}
      {...rest}
    >
      {busy ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" /> : Icon && <Icon className="h-4 w-4" aria-hidden="true" />}
      {children}
    </button>
  );
}

/** IconButton is an icon-only button; label becomes its accessible name and tooltip. */
export function IconButton({
  label,
  icon: Icon,
  className = '',
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { label: string; icon: ComponentType<{ className?: string }> }) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      className={`inline-flex h-8 w-8 items-center justify-center rounded text-ink-muted hover:bg-panel-2 hover:text-ink disabled:opacity-40 ${className}`}
      {...rest}
    >
      <Icon className="h-4 w-4" aria-hidden="true" />
    </button>
  );
}
