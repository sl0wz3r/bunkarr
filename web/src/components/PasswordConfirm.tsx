import { KeyRound, LogIn } from 'lucide-react';
import { useId, useState, type ReactNode } from 'react';
import { api, ApiError, errorMessage } from '@/api/client';
import { useAuth } from '@/auth';
import { Button } from './Button';
import { inputClass } from './Form';
import { Notice } from './Notice';

// The fresh password of safety rule S29 (docs/design/phase4.md §1, §15): the requests that choose
// where data goes (an off-site destination, its credentials or host keys, a source or backup
// target linked to it) or relax the encryption and mode checks need a login session and the
// user's password in the same request. The API key and the local-address bypass get 403; a wrong
// password is 400 and counts like a failed login.

/** isPasswordError reports whether an error is S29's refusal (403), a wrong password (400) or the login limiter (429). */
export function isPasswordError(e: unknown): boolean {
  if (!(e instanceof ApiError)) return false;
  return e.status === 403 || e.status === 429 || (e.status === 400 && /current password|password is incorrect/i.test(e.message));
}

/** passwordErrorText explains a password error in the user's terms. */
export function passwordErrorText(e: unknown): string {
  if (!(e instanceof ApiError)) return errorMessage(e);
  switch (e.status) {
    case 403:
      return `${e.message}. Only a login session can do this: the API key and the local-address bypass cannot choose where data goes.`;
    case 429:
      return `${e.message}. Too many wrong passwords from this address: wait, then try again.`;
    default:
      return 'Your password is incorrect. Wrong passwords count like failed logins.';
  }
}

/**
 * PasswordConfirm asks for the user's Bunkarr password inside the dialog of a change that needs it
 * (S29, the recovery kit). reason says why ("This sends data off this server"); error is the last
 * request's error, shown here when it is about the password. When the page was opened without a
 * login session (the local-address bypass), it offers to log in first, since the server refuses
 * those requests otherwise.
 */
export function PasswordConfirm({
  value,
  onChange,
  reason,
  error,
  label = 'Your Bunkarr password',
  title = 'Confirm with your password',
}: {
  value: string;
  onChange: (v: string) => void;
  reason: ReactNode;
  error?: unknown;
  label?: string;
  title?: string;
}) {
  const id = useId();
  const errId = useId();
  const { status, refresh } = useAuth();
  const session = status.via === 'session';
  const [username, setUsername] = useState(status.username ?? '');
  const [login, setLogin] = useState<{ busy: boolean; error: string | null }>({ busy: false, error: null });
  const fieldError = error && isPasswordError(error) ? passwordErrorText(error) : null;

  async function logIn() {
    setLogin({ busy: true, error: null });
    try {
      await api('/auth/login', { method: 'POST', body: { username, password: value } });
      await refresh();
      setLogin({ busy: false, error: null });
    } catch (e) {
      setLogin({ busy: false, error: errorMessage(e) });
    }
  }

  return (
    <section aria-label={title} className="mb-4 rounded border border-warn/50 bg-warn/10 p-3">
      <h3 className="mb-1 flex items-center gap-2 text-sm font-medium">
        <KeyRound className="h-4 w-4" aria-hidden="true" />
        {title}
      </h3>
      <div className="mb-2 text-xs text-ink-muted">{reason}</div>
      {!session && (
        <Notice tone="warning">
          This page is open without a login session ({status.via === 'apikey' ? 'the API key' : 'the local-address bypass'}), which cannot do this. Log in here
          with your username and password first.
        </Notice>
      )}
      {!session && (
        <div className="mb-2 grid gap-1 sm:grid-cols-[11rem_1fr] sm:gap-4">
          <label htmlFor={`${id}-user`} className="pt-2 text-sm font-medium">
            Username
          </label>
          <input id={`${id}-user`} className={`${inputClass} max-w-xs`} value={username} autoComplete="username" onChange={(e) => setUsername(e.target.value)} />
        </div>
      )}
      <div className="grid gap-1 sm:grid-cols-[11rem_1fr] sm:gap-4">
        <label htmlFor={id} className="pt-2 text-sm font-medium">
          {label}
        </label>
        <div className="min-w-0">
          <div className="flex flex-wrap gap-2">
            <input
              id={id}
              type="password"
              className={`${inputClass} max-w-xs`}
              value={value}
              autoComplete="current-password"
              aria-invalid={fieldError ? true : undefined}
              aria-describedby={fieldError ? errId : undefined}
              onChange={(e) => onChange(e.target.value)}
            />
            {!session && (
              <Button icon={LogIn} busy={login.busy} disabled={!username.trim() || !value} onClick={() => void logIn()}>
                Log in
              </Button>
            )}
          </div>
          {fieldError && (
            <p id={errId} role="alert" className="mt-1 text-xs text-danger">
              {fieldError}
            </p>
          )}
          {login.error && (
            <p role="alert" className="mt-1 text-xs text-danger">
              {login.error}
            </p>
          )}
        </div>
      </div>
    </section>
  );
}
