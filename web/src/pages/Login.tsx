import { useState, type FormEvent } from 'react';
import { api } from '@/api/client';
import { AuthCard, Field, SubmitButton } from '@/components/AuthCard';

export function Login({ onDone }: { onDone: () => Promise<void> }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api('/auth/login', { method: 'POST', body: { username, password } });
      await onDone();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthCard title="Bunkarr" subtitle="Your library's bunker." onSubmit={submit}>
      <Field label="Username" value={username} onChange={setUsername} autoComplete="username" autoFocus />
      <Field label="Password" type="password" value={password} onChange={setPassword} autoComplete="current-password" />
      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
      <SubmitButton busy={busy}>Log in</SubmitButton>
    </AuthCard>
  );
}
