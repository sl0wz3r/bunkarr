import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react';
import { api } from '@/api/client';
import type { AuthStatus } from '@/api/types';
import { Login } from '@/pages/Login';
import { Setup } from '@/pages/Setup';

interface AuthContextValue {
  status: AuthStatus;
  refresh: () => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error('useAuth outside AuthGate');
  }
  return ctx;
}

// AuthGate renders the first-run setup, the login page, or the app, depending on /auth/status.
export function AuthGate({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus | null>(null);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setStatus(await api<AuthStatus>('/auth/status'));
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  const logout = useCallback(async () => {
    await api<void>('/auth/logout', { method: 'POST' });
    await refresh();
  }, [refresh]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  if (error) {
    return (
      <div className="flex h-full items-center justify-center p-6 text-center">
        <div>
          <p className="text-danger">Cannot reach Bunkarr: {error}</p>
          <button className="mt-4 rounded bg-panel-2 px-4 py-2" onClick={() => void refresh()}>
            Retry
          </button>
        </div>
      </div>
    );
  }
  if (!status) {
    return <div className="flex h-full items-center justify-center text-ink-muted">Loading…</div>;
  }
  if (status.setupRequired) {
    return <Setup onDone={refresh} />;
  }
  if (!status.authenticated) {
    return <Login onDone={refresh} />;
  }
  return <AuthContext.Provider value={{ status, refresh, logout }}>{children}</AuthContext.Provider>;
}
