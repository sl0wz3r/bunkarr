import { useQuery } from '@tanstack/react-query';
import { CheckCircle2, ExternalLink, Loader2, LogIn, RefreshCw, Server } from 'lucide-react';
import { useCallback, useEffect, useId, useReducer, useRef, useState } from 'react';
import { ApiError, errorMessage } from '@/api/client';
import { cancelPlexSignIn, getPlexSignIn, plexSignInServers, startPlexSignIn, testPlexSignInServer } from '@/api/plexSignIn';
import type { PlexProbeResult, PlexServerChoice } from '@/api/types';
import { Button } from '@/components/Button';
import { Checkbox } from '@/components/Form';
import { ErrorNotice, Notice } from '@/components/Notice';
import { Badge } from '@/components/StatusBadge';
import {
  formatCountdown,
  isTrustedPlexAuthUrl,
  localDeadline,
  needsUseAnyway,
  PLEX_POLL_MS,
  PLEX_POPUP_NAME,
  PLEX_SIGN_IN_INITIAL,
  plexSignInReducer,
  popupFeatures,
  remainingMs,
  signInIdOf,
  sortPlexServers,
  tokenSource,
  type PlexSignInSelection,
  type PlexSignInState,
} from './plexSignInMachine';

export interface PlexSignInController {
  state: PlexSignInState;
  /** Opens the popup (it must run inside a click handler) and starts a sign-in. */
  start: () => void;
  /** Abandons the attempt in flight (closes the popup, forgets the sign-in on the server). */
  cancel: () => void;
  /** Back to idle from any step ("Start over", "Use a token instead"); forgets the sign-in. */
  reset: () => void;
  chooseServer: (serverId: string) => void;
  /** The connections of serverId are being tested again: forget the old results. */
  testing: (serverId: string) => void;
  /** Records the tested connections of serverId (ignored when another server was chosen since). */
  tested: (serverId: string, results: PlexProbeResult[], recommended: string | null) => void;
  /** The server said the sign-in is gone. */
  expire: () => void;
  /** The integration was saved with this sign-in: the server consumed it, nothing to forget. */
  markSaved: () => void;
}

/**
 * usePlexSignIn drives "Sign in with Plex": it opens a placeholder popup synchronously (popup
 * blockers only allow windows opened during a click), starts the sign-in, sends the popup to
 * app.plex.tv, polls until the PIN is approved or expires, and closes the popup. The sign-in is
 * forgotten on the server (DELETE) when it is abandoned or the component unmounts without saving.
 */
export function usePlexSignIn(): PlexSignInController {
  const [state, dispatch] = useReducer(plexSignInReducer, PLEX_SIGN_IN_INITIAL);
  const popupRef = useRef<Window | null>(null);
  /** Incremented on every start/cancel/reset so late answers of old attempts are dropped. */
  const attemptRef = useRef(0);
  /** The sign-in to forget when the component unmounts (null once saved). */
  const liveIdRef = useRef<string | null>(null);

  const closePopup = useCallback(() => {
    const w = popupRef.current;
    popupRef.current = null;
    try {
      w?.close();
    } catch {
      // already gone or cross-origin: nothing to do
    }
  }, []);

  const forget = useCallback((id: string | null) => {
    if (id) {
      void cancelPlexSignIn(id).catch(() => undefined);
    }
  }, []);

  const currentId = signInIdOf(state);
  useEffect(() => {
    if (currentId) {
      liveIdRef.current = currentId;
    }
  }, [currentId]);

  // Poll a pending sign-in.
  const waitingId = state.step === 'waiting' ? state.signInId : null;
  useEffect(() => {
    if (!waitingId) return;
    let stopped = false;
    const poll = async () => {
      try {
        const st = await getPlexSignIn(waitingId);
        if (stopped) return;
        if (st.status === 'authenticated') {
          dispatch({ type: 'authenticated', username: st.username ?? '', warning: st.warning || undefined });
          closePopup();
        } else if (st.status === 'expired') {
          dispatch({ type: 'expired' });
          closePopup();
        } else {
          dispatch({ type: 'pending', warning: st.warning || undefined });
        }
      } catch (e) {
        if (stopped) return;
        if (e instanceof ApiError && e.status === 404) {
          dispatch({ type: 'expired' });
          closePopup();
        } else if (e instanceof ApiError && e.status >= 400 && e.status < 500) {
          dispatch({ type: 'failed', message: `Plex sign-in failed: ${errorMessage(e)}` });
          closePopup();
        }
        // Other errors (Bunkarr or plex.tv briefly unavailable): keep polling.
      }
    };
    const id = setInterval(() => void poll(), PLEX_POLL_MS);
    return () => {
      stopped = true;
      clearInterval(id);
    };
  }, [waitingId, closePopup]);

  // Expiry of a pending sign-in (the countdown reaching zero).
  const waitingUntil = state.step === 'waiting' ? state.expiresAt : null;
  useEffect(() => {
    if (waitingUntil === null) return;
    const timer = setTimeout(() => {
      dispatch({ type: 'expired' });
      closePopup();
    }, remainingMs(waitingUntil, Date.now()));
    return () => clearTimeout(timer);
  }, [waitingUntil, closePopup]);

  // Never leave a popup or a sign-in behind.
  useEffect(
    () => () => {
      attemptRef.current += 1;
      closePopup();
      forget(liveIdRef.current);
      liveIdRef.current = null;
    },
    [closePopup, forget],
  );

  const inFlight = state.step === 'creating' || state.step === 'waiting';

  const start = useCallback(() => {
    if (inFlight) return;
    const attempt = ++attemptRef.current;
    forget(liveIdRef.current);
    liveIdRef.current = null;
    let popup: Window | null = null;
    try {
      popup = window.open('', PLEX_POPUP_NAME, popupFeatures());
    } catch {
      popup = null;
    }
    if (popup) {
      // plex.tv must not be able to script or navigate this tab (reverse tabnabbing).
      try {
        popup.opener = null;
      } catch {
        // ignore
      }
      try {
        popup.document.title = 'Sign in with Plex';
        popup.document.body.textContent = 'Loading Plex sign-in…';
      } catch {
        // a popup re-used from an earlier attempt, already on plex.tv
      }
    }
    popupRef.current = popup;
    dispatch({ type: 'start' });
    startPlexSignIn().then(
      (created) => {
        if (attempt !== attemptRef.current) {
          forget(created?.id ?? null);
          return;
        }
        if (!created || !isTrustedPlexAuthUrl(created.authUrl)) {
          closePopup();
          forget(created?.id ?? null);
          dispatch({ type: 'failed', message: 'Bunkarr returned an unexpected sign-in address, so the Plex window was not opened.' });
          return;
        }
        liveIdRef.current = created.id;
        const w = popupRef.current;
        let blocked = !w || w.closed;
        if (w && !blocked) {
          try {
            w.location.href = created.authUrl;
          } catch {
            blocked = true;
          }
        }
        // The countdown runs on this browser's clock, from the time the server says is left.
        const expiresAt = localDeadline(created.expiresAt, created.serverDate, Date.now());
        dispatch({ type: 'created', signInId: created.id, authUrl: created.authUrl, expiresAt, popupBlocked: blocked });
      },
      (e: unknown) => {
        if (attempt !== attemptRef.current) return;
        closePopup();
        dispatch({ type: 'failed', message: errorMessage(e) || 'Could not start the Plex sign-in' });
      },
    );
  }, [inFlight, closePopup, forget]);

  const cancel = useCallback(() => {
    attemptRef.current += 1;
    closePopup();
    forget(liveIdRef.current);
    liveIdRef.current = null;
    dispatch({ type: 'cancel' });
  }, [closePopup, forget]);

  const reset = useCallback(() => {
    attemptRef.current += 1;
    closePopup();
    forget(liveIdRef.current);
    liveIdRef.current = null;
    dispatch({ type: 'reset' });
  }, [closePopup, forget]);

  const chooseServer = useCallback((serverId: string) => dispatch({ type: 'chooseServer', serverId }), []);
  const testing = useCallback((serverId: string) => dispatch({ type: 'testing', serverId }), []);
  const tested = useCallback(
    (serverId: string, results: PlexProbeResult[], recommended: string | null) => dispatch({ type: 'tested', serverId, results, recommended }),
    [],
  );
  const expire = useCallback(() => {
    forget(liveIdRef.current);
    liveIdRef.current = null;
    dispatch({ type: 'expired' });
  }, [forget]);

  const markSaved = useCallback(() => {
    liveIdRef.current = null;
    dispatch({ type: 'saved' });
  }, []);

  return { state, start, cancel, reset, chooseServer, testing, tested, expire, markSaved };
}

/** The current time, re-rendered every second while active. */
function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

/** "14:05": a local time for screen readers (it does not change every second). */
function formatClock(ms: number): string {
  return new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

export interface PlexSignInPanelProps {
  controller: PlexSignInController;
  /** The connection the user took ("Use"): the form fills its URL and the token line. */
  onSelect: (selection: PlexSignInSelection) => void;
  /**
   * The selection no longer matches what the picker shows (another server, or the account-token
   * box changed): the form must drop it, so Save never sends a choice the user took back.
   */
  onClear?: () => void;
  /** The current selection, to mark its row. */
  selection: PlexSignInSelection | null;
  disabled?: boolean;
}

/** "Sign in with Plex": the popup flow, then the server and connection picker. */
export function PlexSignInPanel({ controller, onSelect, onClear, selection, disabled }: PlexSignInPanelProps) {
  const { state, start, cancel, reset } = controller;
  const now = useNow(state.step === 'waiting');

  switch (state.step) {
    case 'idle':
    case 'creating':
    case 'saved':
      return (
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="primary" icon={LogIn} busy={state.step === 'creating'} disabled={disabled} onClick={start}>
            Sign in with Plex
          </Button>
          <span className="text-sm text-ink-muted">
            Recommended: sign in on plex.tv, pick your server and one of its tested connections. The token stays on Bunkarr&apos;s server.
          </span>
        </div>
      );
    case 'waiting':
      return (
        <Notice tone="info" title="Waiting for Plex sign-in…">
          <div className="flex flex-wrap items-center gap-2">
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
            <span>
              Approve the sign-in in the Plex window
              {/* The per-second countdown is hidden from the live region (it would be read out every
                  second); screen readers get the fixed expiry time instead. */}
              <span aria-hidden="true" data-testid="plex-countdown">
                {' '}
                ({formatCountdown(remainingMs(state.expiresAt, now))} left)
              </span>
              <span className="sr-only"> (it expires at {formatClock(state.expiresAt)})</span>.
            </span>
            <Button small onClick={cancel}>
              Cancel
            </Button>
          </div>
          {state.warning && <div className="mt-2 text-warn">{state.warning}</div>}
          {state.popupBlocked && (
            <div className="mt-2">
              <span className="text-warn">Your browser blocked the Plex window. </span>
              <a href={state.authUrl} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 text-accent underline">
                Open Plex sign-in
                <ExternalLink className="h-3 w-3" aria-hidden="true" />
              </a>
            </div>
          )}
        </Notice>
      );
    case 'expired':
      return (
        <Notice tone="warning" title="The Plex sign-in expired">
          <p>Sign in again (sign-ins last 10 minutes, and do not survive a restart of Bunkarr).</p>
          <div className="mt-2">
            <Button small icon={RefreshCw} disabled={disabled} onClick={start}>
              Sign in again
            </Button>
          </div>
        </Notice>
      );
    case 'error':
      return (
        <Notice tone="error" title="Plex sign-in failed">
          <p>{state.message}</p>
          <div className="mt-2">
            <Button small icon={RefreshCw} disabled={disabled} onClick={start}>
              Try again
            </Button>
          </div>
        </Notice>
      );
    case 'authenticated':
    case 'serverChosen':
      return (
        <ServerPicker controller={controller} state={state} onSelect={onSelect} onClear={onClear} selection={selection} onReset={reset} disabled={disabled} />
      );
  }
}

type SignedIn = Extract<PlexSignInState, { step: 'authenticated' | 'serverChosen' }>;

function ServerPicker({
  controller,
  state,
  onSelect,
  onClear,
  selection,
  onReset,
  disabled,
}: {
  controller: PlexSignInController;
  state: SignedIn;
  onSelect: (s: PlexSignInSelection) => void;
  onClear?: () => void;
  selection: PlexSignInSelection | null;
  onReset: () => void;
  disabled?: boolean;
}) {
  const groupName = useId();
  const servers = useQuery({
    queryKey: ['plexSignIn', state.signInId, 'servers'],
    queryFn: () => plexSignInServers(state.signInId),
    staleTime: 10_000,
  });
  const [useAccountToken, setUseAccountToken] = useState(false);
  const serverId = state.step === 'serverChosen' ? state.serverId : null;
  const testRun = useRef(0);
  const [testError, setTestError] = useState<unknown>(null);
  const { testing, tested, expire } = controller;

  const runTest = useCallback(
    (id: string, account: boolean) => {
      const run = ++testRun.current;
      setTestError(null);
      testing(id);
      testPlexSignInServer(state.signInId, id, account).then(
        (ans) => {
          if (run === testRun.current) tested(id, ans.results ?? [], ans.recommended ?? null);
        },
        (e: unknown) => {
          if (run !== testRun.current) return;
          if (e instanceof ApiError && (e.status === 404 || e.status === 409)) {
            expire();
            return;
          }
          // The results stay unknown (null): a failed test says nothing about the connections.
          setTestError(e);
        },
      );
    },
    [state.signInId, testing, tested, expire],
  );

  useEffect(() => {
    if (serverId) runTest(serverId, useAccountToken);
  }, [serverId, useAccountToken, runTest]);

  // An expired sign-in answers 409 or 404 to the server list.
  const listError = servers.error;
  useEffect(() => {
    if (listError instanceof ApiError && (listError.status === 404 || (listError.status === 409 && /expired/i.test(listError.message)))) {
      expire();
    }
  }, [listError, expire]);

  const header = (
    <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
      <div className="flex items-center gap-2 text-sm">
        <CheckCircle2 className="h-4 w-4 text-accent" aria-hidden="true" />
        {state.username ? (
          <span>
            Signed in as <strong>{state.username}</strong>
          </span>
        ) : (
          <span>Signed in to Plex</span>
        )}
      </div>
      <Button small variant="ghost" onClick={onReset}>
        Start over
      </Button>
      {state.warning && (
        <Notice tone="warning" className="w-full">
          {state.warning}
        </Notice>
      )}
    </div>
  );

  if (servers.isPending) {
    return (
      <div>
        {header}
        <div className="flex items-center gap-2 text-sm text-ink-muted">
          <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
          Loading your Plex servers…
        </div>
      </div>
    );
  }
  if (servers.isError) {
    return (
      <div>
        {header}
        <ErrorNotice error={servers.error} />
        <Button small icon={RefreshCw} onClick={() => void servers.refetch()}>
          Retry
        </Button>
      </div>
    );
  }
  const list = sortPlexServers(servers.data);
  if (list.length === 0) {
    return (
      <div>
        {header}
        <Notice tone="warning" title="No Plex Media Server found">
          This Plex account has no servers. Sign in with the account that owns your server, or enter the details manually below.
        </Notice>
      </div>
    );
  }
  const chosen = list.find((s) => s.id === serverId) ?? null;
  const source = chosen ? tokenSource(chosen, useAccountToken) : 'none';

  return (
    <div>
      {header}
      <fieldset className="m-0 border-0 p-0">
        <legend className="mb-1.5 text-sm font-medium">Server</legend>
        <div className="flex flex-col gap-1.5">
          {list.map((s) => (
            <ServerOption
              key={s.id}
              server={s}
              name={groupName}
              checked={s.id === serverId}
              disabled={disabled}
              onChange={() => {
                setUseAccountToken(false);
                if (selection && selection.serverId !== s.id) onClear?.();
                controller.chooseServer(s.id);
              }}
            />
          ))}
        </div>
      </fieldset>

      {chosen && !chosen.owned && (
        <Notice tone="warning" title="You don't own this server" className="mt-3">
          Bunkarr can import its libraries, but a shared server does not let Bunkarr read its settings (no maintenance-window check), and backing up
          its database needs its data folder mounted into Bunkarr.
        </Notice>
      )}
      {chosen && !chosen.hasAccessToken && chosen.owned && (
        <Notice tone="warning" title="plex.tv lists no token for this server" className="mt-3">
          <p>
            Bunkarr can save your plex.tv account token for it instead. That token controls your whole Plex account and stays valid until you sign out of
            all devices on plex.tv; Bunkarr only sends it to an address that answers as this server.
          </p>
          <div className="mt-2">
            <Checkbox
              label="Use my Plex account token for this server"
              checked={useAccountToken}
              onChange={(v) => {
                setUseAccountToken(v);
                // A connection taken with (or without) the account token no longer stands.
                if (selection?.serverId === chosen.id) onClear?.();
              }}
              disabled={disabled}
            />
          </div>
        </Notice>
      )}
      {chosen && !chosen.hasAccessToken && !chosen.owned && (
        <Notice tone="error" title="No token for this shared server" className="mt-3">
          plex.tv lists no access token for this server, and Bunkarr never sends your account token to a server you don&apos;t own. Enter the server&apos;s URL
          and a token for it manually below.
        </Notice>
      )}

      {chosen && (
        <ConnectionResults
          server={chosen}
          state={state}
          canUse={source !== 'none'}
          selection={selection}
          testError={testError}
          disabled={disabled}
          onRetest={() => runTest(chosen.id, useAccountToken)}
          onUse={(r) =>
            onSelect({ signInId: state.signInId, serverId: chosen.id, serverName: chosen.name || chosen.id, owned: chosen.owned, uri: r.uri, useAccountToken: source === 'account' })
          }
        />
      )}
    </div>
  );
}

function ServerOption({ server, name, checked, disabled, onChange }: { server: PlexServerChoice; name: string; checked: boolean; disabled?: boolean; onChange: () => void }) {
  return (
    <label className={`flex cursor-pointer items-center gap-3 rounded border px-3 py-2 ${checked ? 'border-accent bg-accent/10' : 'border-line hover:bg-panel-2'}`}>
      <input type="radio" name={name} checked={checked} disabled={disabled} onChange={onChange} className="accent-[var(--color-accent)]" />
      <Server className="h-4 w-4 shrink-0 text-ink-muted" aria-hidden="true" />
      <span className="min-w-0 flex-1 truncate">{server.name || server.id}</span>
      {server.productVersion && <span className="hidden text-xs text-ink-muted sm:inline">v{server.productVersion}</span>}
      {server.owned ? <Badge tone="ok">Owner</Badge> : <Badge tone="warn">Shared</Badge>}
    </label>
  );
}

function ConnectionResults({
  server,
  state,
  canUse,
  selection,
  testError,
  disabled,
  onRetest,
  onUse,
}: {
  server: PlexServerChoice;
  state: SignedIn;
  canUse: boolean;
  selection: PlexSignInSelection | null;
  testError: unknown;
  disabled?: boolean;
  onRetest: () => void;
  onUse: (r: PlexProbeResult) => void;
}) {
  const results = state.step === 'serverChosen' ? state.results : null;
  const recommended = state.step === 'serverChosen' ? state.recommended : null;
  const [confirm, setConfirm] = useState<string | null>(null);
  const relayChosen = !!selection && selection.serverId === server.id && !!results?.find((r) => r.uri === selection.uri && r.relay);
  return (
    <div className="mt-3">
      <div className="mb-1.5 flex items-center justify-between gap-2">
        <span className="text-sm font-medium">Connections</span>
        <Button small variant="ghost" icon={RefreshCw} disabled={results === null && !testError} onClick={onRetest}>
          Test again
        </Button>
      </div>
      <ErrorNotice error={testError} />
      {results === null ? (
        testError ? null : (
        <div className="flex items-center gap-2 text-sm text-ink-muted">
          <Loader2 className="h-4 w-4 animate-spin" aria-hidden="true" />
          Testing {server.name}&apos;s connections from Bunkarr…
        </div>
        )
      ) : results.length === 0 ? (
        <p className="text-sm text-ink-muted">plex.tv lists no connection Bunkarr can use for this server. Enter its URL manually below.</p>
      ) : (
        <ul className="m-0 flex list-none flex-col gap-1.5 p-0" aria-label="Connections">
          {results.map((r) => {
            const isRecommended = r.uri === recommended;
            const chosen = selection?.serverId === server.id && selection.uri === r.uri;
            const anyway = needsUseAnyway(r);
            return (
              <li key={r.uri} className={`rounded border px-3 py-2 ${chosen ? 'border-accent bg-accent/10' : 'border-line'}`}>
                <div className="flex flex-wrap items-center gap-2">
                  <code className="min-w-0 flex-1 break-all text-xs">{r.uri}</code>
                  <span className="flex flex-wrap items-center gap-1">
                    {isRecommended && <Badge tone="ok">Recommended</Badge>}
                    {r.relay ? <Badge tone="warn">Relay</Badge> : <Badge tone="info">{r.local ? 'Local' : 'Remote'}</Badge>}
                    {r.protocol === 'https' ? <Badge tone="ok">HTTPS</Badge> : <Badge tone="warn">Unencrypted</Badge>}
                    {r.derived && (
                      <Badge tone="muted" title="The LAN address behind a plex.direct name (home routers often block plex.direct names)">
                        Derived
                      </Badge>
                    )}
                  </span>
                  {chosen ? (
                    <Badge tone="ok">Selected</Badge>
                  ) : anyway ? (
                    confirm === r.uri ? (
                      <Button small variant="danger" disabled={disabled || !canUse} onClick={() => onUse(r)}>
                        Use anyway
                      </Button>
                    ) : (
                      <Button small disabled={disabled || !canUse} onClick={() => setConfirm(r.uri)}>
                        Use…
                      </Button>
                    )
                  ) : (
                    <Button small variant={isRecommended ? 'primary' : 'secondary'} disabled={disabled || !canUse} onClick={() => onUse(r)}>
                      Use
                    </Button>
                  )}
                </div>
                <div className={`mt-1 text-xs ${r.ok ? 'text-ink-muted' : 'text-danger'}`}>
                  {r.ok ? `${r.message} ${r.latencyMs} ms.` : r.message}
                </div>
                {confirm === r.uri && !chosen && (
                  <div className="mt-1 text-xs text-warn">
                    {!r.ok
                      ? 'This connection did not work from Bunkarr. Use it only if you know it will.'
                      : 'This connection is not encrypted: the token would cross the network in clear text.'}
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
      {relayChosen && (
        <Notice tone="warning" className="mt-2">
          A relay connection is slow and bandwidth-limited; prefer a local or remote one when it works.
        </Notice>
      )}
    </div>
  );
}
