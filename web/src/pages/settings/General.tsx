import { Copy, Eye, EyeOff, RefreshCw } from 'lucide-react';
import { useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { api } from '@/api/client';
import type { AuthRequired, GeneralSettings } from '@/api/types';
import { useAuth } from '@/auth';
import { Page, Section } from '@/components/Page';

function Row({ label, help, children }: { label: string; help?: string; children: ReactNode }) {
  return (
    <div className="mb-4 grid gap-2 sm:grid-cols-[12rem_1fr] sm:items-start">
      <div className="pt-2 text-sm font-medium">{label}</div>
      <div>
        {children}
        {help && <p className="mt-1 text-xs text-ink-muted">{help}</p>}
      </div>
    </div>
  );
}

const input = 'w-full max-w-md rounded border border-line bg-page px-3 py-2 outline-none focus:border-accent';

export function General() {
  const { status, refresh } = useAuth();
  const [settings, setSettings] = useState<GeneralSettings | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [showKey, setShowKey] = useState(false);

  useEffect(() => {
    api<GeneralSettings>('/settings/general')
      .then(setSettings)
      .catch((e: Error) => setError(e.message));
  }, []);

  async function run(fn: () => Promise<void>) {
    setError(null);
    setNotice(null);
    try {
      await fn();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }

  const changeMode = (mode: AuthRequired) =>
    run(async () => {
      setSettings(await api<GeneralSettings>('/settings/general', { method: 'PUT', body: { authenticationRequired: mode } }));
      await refresh();
      setNotice('Saved.');
    });

  const regenerate = () =>
    run(async () => {
      if (!window.confirm('Regenerate the API key? Anything using the current key (Sonarr/Radarr webhooks, scripts) stops working until you update it.')) {
        return;
      }
      setSettings(await api<GeneralSettings>('/settings/general/apikey', { method: 'POST' }));
      setShowKey(true);
      setNotice('New API key generated.');
    });

  return (
    <Page title="General">
      {error && (
        <p role="alert" className="mb-4 rounded border border-danger/50 bg-danger/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      {notice && <p className="mb-4 rounded border border-accent/40 bg-accent/10 px-3 py-2 text-sm">{notice}</p>}
      {!settings ? (
        !error && <p className="text-ink-muted">Loading…</p>
      ) : (
        <>
          <Section title="Security">
            <Row label="Authentication" help="Forms login (username and password), like the *arr apps.">
              <input className={input} value="Forms (login page)" disabled />
            </Row>
            <Row
              label="Authentication required"
              help="Disabled for local addresses lets devices on your LAN (private IP ranges) in without a login. Requests through a reverse proxy come from the proxy's address."
            >
              <select
                aria-label="Authentication required"
                className={input}
                value={settings.authenticationRequired}
                onChange={(e) => void changeMode(e.target.value as AuthRequired)}
              >
                <option value="enabled">Enabled</option>
                <option value="disabled_for_local_addresses">Disabled for local addresses</option>
              </select>
            </Row>
            <Row label="API key" help="Send it as the X-Api-Key header or ?apikey= (Sonarr/Radarr webhooks, scripts).">
              <div className="flex max-w-md gap-2">
                <input
                  aria-label="API key"
                  className={`${input} font-mono`}
                  readOnly
                  value={showKey ? settings.apiKey : '•'.repeat(32)}
                />
                <button type="button" title={showKey ? 'Hide' : 'Show'} className="rounded bg-panel-2 px-3" onClick={() => setShowKey(!showKey)}>
                  {showKey ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
                </button>
                <button
                  type="button"
                  title="Copy"
                  className="rounded bg-panel-2 px-3"
                  onClick={() => void navigator.clipboard?.writeText(settings.apiKey).then(() => setNotice('API key copied.'))}
                >
                  <Copy className="h-4 w-4" />
                </button>
                <button type="button" title="Regenerate" className="rounded bg-panel-2 px-3 text-danger" onClick={() => void regenerate()}>
                  <RefreshCw className="h-4 w-4" />
                </button>
              </div>
            </Row>
          </Section>
          <Section title="Host">
            <Row label="Bind address" help="Set with BUNKARR_BIND.">
              <input className={input} value={settings.bindAddress} disabled />
            </Row>
            <Row label="Port" help="Set with BUNKARR_PORT.">
              <input className={input} value={settings.port} disabled />
            </Row>
          </Section>
          {status.via === 'session' && <Credentials onSaved={(m) => setNotice(m)} onError={(m) => setError(m)} />}
        </>
      )}
    </Page>
  );
}

function Credentials({ onSaved, onError }: { onSaved: (m: string) => void; onError: (m: string) => void }) {
  const { status, refresh } = useAuth();
  const [username, setUsername] = useState(status.username ?? '');
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');

  async function submit(e: FormEvent) {
    e.preventDefault();
    try {
      await api('/auth/credentials', { method: 'PUT', body: { currentPassword: current, username, newPassword: next } });
      setCurrent('');
      setNext('');
      await refresh();
      onSaved('Login updated. Other sessions were signed out.');
    } catch (err) {
      onError(err instanceof Error ? err.message : String(err));
    }
  }

  return (
    <Section title="Login">
      <form onSubmit={submit}>
        <Row label="Username">
          <input aria-label="Username" className={input} value={username} autoComplete="username" onChange={(e) => setUsername(e.target.value)} />
        </Row>
        <Row label="New password" help="Leave empty to keep the current password.">
          <input aria-label="New password" type="password" className={input} value={next} autoComplete="new-password" onChange={(e) => setNext(e.target.value)} />
        </Row>
        <Row label="Current password">
          <input aria-label="Current password" type="password" className={input} value={current} autoComplete="current-password" onChange={(e) => setCurrent(e.target.value)} />
        </Row>
        <button type="submit" className="rounded bg-accent px-4 py-2 font-medium text-page hover:bg-accent-strong sm:ml-[12.5rem]">
          Save login
        </button>
      </form>
    </Section>
  );
}
