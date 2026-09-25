import { useState, type FormEvent } from 'react';
import { api } from '@/api/client';
import { AuthCard, Field, SubmitButton } from '@/components/AuthCard';

export const MIN_PASSWORD = 8;

// First run: Bunkarr refuses to run unprotected, so the user must create a login first.
export function Setup({ onDone }: { onDone: () => Promise<void> }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (!username.trim()) {
      setError('Enter a username.');
      return;
    }
    if ([...password].length < MIN_PASSWORD) {
      setError(`The password must be at least ${MIN_PASSWORD} characters.`);
      return;
    }
    if (password !== confirm) {
      setError('The passwords do not match.');
      return;
    }
    setBusy(true);
    try {
      await api('/auth/setup', { method: 'POST', body: { username, password } });
      await onDone();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthCard title="Welcome to Bunkarr" subtitle="Create the login that protects this instance." onSubmit={submit}>
      <Field label="Username" value={username} onChange={setUsername} autoComplete="username" autoFocus />
      <Field label="Password" type="password" value={password} onChange={setPassword} autoComplete="new-password" />
      <Field label="Confirm password" type="password" value={confirm} onChange={setConfirm} autoComplete="new-password" />
      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
      <SubmitButton busy={busy}>Create login</SubmitButton>
    </AuthCard>
  );
}
