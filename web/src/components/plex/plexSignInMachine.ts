/**
 * "Sign in with Plex" (design §5) — the pure state machine and its helpers, rewritten from
 * Dupearr's. No state holds a token: the server keeps the plex.tv account token and every server's
 * access token, and the browser only knows the opaque sign-in id and the chosen server id.
 *
 * idle → creating (POST /plex/signin) → waiting (popup on app.plex.tv, GET /plex/signin/{id}
 * polled every 2 s until expiresAt) → authenticated (server list) → serverChosen (its connections
 * tested) → saved | expired | error. `cancel` returns to idle from an attempt in flight; actions
 * that do not apply to the current step are ignored, so late answers of an abandoned attempt are
 * harmless.
 */
import type { PlexProbeResult, PlexServerChoice } from '@/api/types';

/** window.open target name (re-uses the same popup on repeated attempts). */
export const PLEX_POPUP_NAME = 'bunkarr-plex-auth';

/** How often a pending sign-in is polled (the server checks plex.tv at most every 2 s). */
export const PLEX_POLL_MS = 2000;

/** A pending sign-in lasts at most 10 minutes on the server (design §5). */
export const PLEX_SIGN_IN_MAX_MS = 10 * 60_000;

const SIGN_IN_ID = /^[0-9a-f]{32}$/;

/** isSignInId accepts the server's sign-in ids (32 lower-case hex characters). */
export function isSignInId(value: unknown): value is string {
  return typeof value === 'string' && SIGN_IN_ID.test(value);
}

export type PlexSignInState =
  | { step: 'idle' }
  | { step: 'creating' }
  | {
      step: 'waiting';
      signInId: string;
      /** When the pending sign-in expires, on this browser's clock (localDeadline): the countdown. */
      expiresAt: number;
      /** The popup could not be opened (blocked): the UI offers a link instead. */
      popupBlocked: boolean;
      /** Only kept for that link; it holds the PIN code, never a token. */
      authUrl: string;
      /** The server's note on the last check ("plex.tv did not answer the last check"). */
      warning?: string;
    }
  | {
      step: 'authenticated';
      signInId: string;
      username: string;
      /** The server's note on the approval ("did not say which account"). */
      warning?: string;
    }
  | {
      step: 'serverChosen';
      signInId: string;
      username: string;
      warning?: string;
      serverId: string;
      /** The tested connections, best first; null while the test runs. */
      results: PlexProbeResult[] | null;
      recommended: string | null;
    }
  | { step: 'saved' }
  | { step: 'expired' }
  | { step: 'error'; message: string };

export type PlexSignInAction =
  | { type: 'start' }
  | { type: 'created'; signInId: string; authUrl: string; expiresAt: number; popupBlocked: boolean }
  | { type: 'pending'; warning?: string }
  | { type: 'authenticated'; username: string; warning?: string }
  | { type: 'chooseServer'; serverId: string }
  /** The connections of serverId are being tested (again): the old results no longer apply. */
  | { type: 'testing'; serverId: string }
  | { type: 'tested'; serverId: string; results: PlexProbeResult[]; recommended: string | null }
  | { type: 'saved' }
  | { type: 'expired' }
  | { type: 'failed'; message: string }
  | { type: 'cancel' }
  | { type: 'reset' };

export const PLEX_SIGN_IN_INITIAL: PlexSignInState = { step: 'idle' };

/** signInIdOf returns the sign-in the state refers to, if any (for DELETE on close). */
export function signInIdOf(state: PlexSignInState): string | null {
  return 'signInId' in state ? state.signInId : null;
}

/** Transition function; an action that does not apply returns the same state object. */
export function plexSignInReducer(state: PlexSignInState, action: PlexSignInAction): PlexSignInState {
  switch (action.type) {
    case 'start':
      return state.step === 'creating' || state.step === 'waiting' ? state : { step: 'creating' };
    case 'created':
      if (state.step !== 'creating') return state;
      if (!isSignInId(action.signInId) || !isTrustedPlexAuthUrl(action.authUrl) || !Number.isFinite(action.expiresAt)) {
        return { step: 'error', message: 'Bunkarr returned an unexpected sign-in, so the Plex window was not opened. Please try again.' };
      }
      return { step: 'waiting', signInId: action.signInId, authUrl: action.authUrl, expiresAt: action.expiresAt, popupBlocked: action.popupBlocked };
    case 'pending':
      if (state.step !== 'waiting' || (state.warning ?? '') === (action.warning ?? '')) return state;
      return {
        step: 'waiting',
        signInId: state.signInId,
        authUrl: state.authUrl,
        expiresAt: state.expiresAt,
        popupBlocked: state.popupBlocked,
        ...(action.warning ? { warning: action.warning } : {}),
      };
    case 'authenticated':
      return state.step === 'waiting'
        ? { step: 'authenticated', signInId: state.signInId, username: action.username, ...(action.warning ? { warning: action.warning } : {}) }
        : state;
    case 'chooseServer':
      if (state.step !== 'authenticated' && state.step !== 'serverChosen') return state;
      if (state.step === 'serverChosen' && state.serverId === action.serverId) return state;
      return {
        step: 'serverChosen',
        signInId: state.signInId,
        username: state.username,
        ...(state.warning ? { warning: state.warning } : {}),
        serverId: action.serverId,
        results: null,
        recommended: null,
      };
    case 'testing':
      if (state.step !== 'serverChosen' || state.serverId !== action.serverId || state.results === null) return state;
      return { ...state, results: null, recommended: null };
    case 'tested':
      if (state.step !== 'serverChosen' || state.serverId !== action.serverId) return state;
      return { ...state, results: action.results, recommended: action.recommended };
    case 'saved':
      return state.step === 'authenticated' || state.step === 'serverChosen' ? { step: 'saved' } : state;
    case 'expired':
      return state.step === 'waiting' || state.step === 'authenticated' || state.step === 'serverChosen' ? { step: 'expired' } : state;
    case 'failed':
      return state.step === 'creating' || state.step === 'waiting' ? { step: 'error', message: action.message || 'Plex sign-in failed' } : state;
    case 'cancel':
      return state.step === 'creating' || state.step === 'waiting' ? PLEX_SIGN_IN_INITIAL : state;
    case 'reset':
      return PLEX_SIGN_IN_INITIAL;
    default:
      return state;
  }
}

/**
 * localDeadline turns the server's expiresAt into a deadline on this browser's clock, which may be
 * minutes off the server's: the time left as the server counts it (expiresAt minus the answer's
 * Date header), from when the answer arrived, at most 10 minutes. Without a usable Date header the
 * time left is taken at face value; when that is already past (a browser clock ahead of the
 * server's), the full 10 minutes are assumed and the server's "expired" ends the sign-in instead.
 */
export function localDeadline(expiresAt: string | null | undefined, serverDate: string | null | undefined, receivedAt: number): number {
  const expires = expiresAt ? Date.parse(expiresAt) : NaN;
  if (!Number.isFinite(expires)) return NaN;
  const serverNow = serverDate ? Date.parse(serverDate) : NaN;
  let left = Number.isFinite(serverNow) ? expires - serverNow : expires - receivedAt;
  if (!Number.isFinite(left)) {
    left = PLEX_SIGN_IN_MAX_MS;
  } else if (left <= 0 && !Number.isFinite(serverNow)) {
    left = PLEX_SIGN_IN_MAX_MS;
  }
  return receivedAt + Math.min(Math.max(0, left), PLEX_SIGN_IN_MAX_MS);
}

/** Milliseconds left until expiresAt (0 when elapsed). */
export function remainingMs(expiresAt: number, now: number): number {
  return Math.max(0, expiresAt - now);
}

/** "4:05" */
export function formatCountdown(ms: number): string {
  const total = Math.ceil(Math.max(0, ms) / 1000);
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}

/**
 * The popup navigates only to https://app.plex.tv/auth (the PIN approval page). The URL comes from
 * Bunkarr's own server, but refusing anything else keeps a misconfigured or compromised backend
 * from turning the popup into an open redirect.
 */
export function isTrustedPlexAuthUrl(value: string | null | undefined): boolean {
  if (!value) return false;
  try {
    const u = new URL(value);
    return u.protocol === 'https:' && u.hostname === 'app.plex.tv' && u.port === '' && u.username === '' && u.password === '' && u.pathname === '/auth';
  } catch {
    return false;
  }
}

/** Servers the user owns first, then by name (the server already sends them so). */
export function sortPlexServers(servers: readonly PlexServerChoice[] | null | undefined): PlexServerChoice[] {
  return [...(servers ?? [])].sort((a, b) => Number(b.owned) - Number(a.owned) || (a.name || '').localeCompare(b.name || ''));
}

/** Where the saved token comes from: the server's own token, or the plex.tv account token. */
export type PlexTokenSource = 'server' | 'account' | 'none';

/**
 * tokenSource says which token saving `server` would store: its own access token; for an owned
 * server without one, the account token once the user allowed it; never for a shared server
 * (the owner's server would receive the token that controls the whole account: Dupearr SEC-033).
 */
export function tokenSource(server: Pick<PlexServerChoice, 'owned' | 'hasAccessToken'>, useAccountToken: boolean): PlexTokenSource {
  if (server.hasAccessToken) return 'server';
  if (server.owned && useAccountToken) return 'account';
  return 'none';
}

/** A tested connection may be used directly only when it works over https (a relay works but is slow). */
export function needsUseAnyway(r: Pick<PlexProbeResult, 'ok' | 'protocol'>): boolean {
  return !r.ok || r.protocol !== 'https';
}

/** What the sign-in hands to the Plex form. */
export interface PlexSignInSelection {
  signInId: string;
  serverId: string;
  serverName: string;
  owned: boolean;
  /** The chosen connection (becomes the integration's URL). */
  uri: string;
  useAccountToken: boolean;
}

/** Popup window features centred over the current window. */
export function popupFeatures(width = 600, height = 720): string {
  if (typeof window === 'undefined') return `width=${width},height=${height}`;
  const left = Math.max(0, Math.round(window.screenX + (window.outerWidth - width) / 2));
  const top = Math.max(0, Math.round(window.screenY + (window.outerHeight - height) / 2));
  return `width=${width},height=${height},left=${left},top=${top},resizable=yes,scrollbars=yes`;
}
