import { describe, expect, it } from 'vitest';
import type { PlexProbeResult, PlexServerChoice } from '@/api/types';
import {
  formatCountdown,
  isSignInId,
  isTrustedPlexAuthUrl,
  localDeadline,
  needsUseAnyway,
  PLEX_SIGN_IN_MAX_MS,
  PLEX_SIGN_IN_INITIAL,
  plexSignInReducer,
  remainingMs,
  signInIdOf,
  sortPlexServers,
  tokenSource,
  type PlexSignInAction,
  type PlexSignInState,
} from './plexSignInMachine';

const ID = '0123456789abcdef0123456789abcdef';
const AUTH_URL = 'https://app.plex.tv/auth#?clientID=x&code=abcd&context%5Bdevice%5D%5Bproduct%5D=Bunkarr';
const created: PlexSignInAction = { type: 'created', signInId: ID, authUrl: AUTH_URL, expiresAt: 600_000, popupBlocked: false };
const waiting: PlexSignInState = { step: 'waiting', signInId: ID, authUrl: AUTH_URL, expiresAt: 600_000, popupBlocked: false };
const authenticated: PlexSignInState = { step: 'authenticated', signInId: ID, username: 'alice' };
const chosen: PlexSignInState = { step: 'serverChosen', signInId: ID, username: 'alice', serverId: 'm1', results: null, recommended: null };
const result: PlexProbeResult = {
  uri: 'https://10-0-0-2.abc.plex.direct:32400',
  local: true,
  relay: false,
  derived: false,
  protocol: 'https',
  ok: true,
  identityMatches: true,
  tokenAccepted: true,
  latencyMs: 4,
  message: 'Connected.',
};

function run(actions: PlexSignInAction[], from: PlexSignInState = PLEX_SIGN_IN_INITIAL): PlexSignInState {
  return actions.reduce(plexSignInReducer, from);
}

describe('plexSignInReducer', () => {
  it.each<{ name: string; actions: PlexSignInAction[]; from?: PlexSignInState; expected: PlexSignInState }>([
    { name: 'start → creating', actions: [{ type: 'start' }], expected: { step: 'creating' } },
    { name: 'created → waiting', actions: [{ type: 'start' }, created], expected: waiting },
    { name: 'a blocked popup is remembered', actions: [{ type: 'start' }, { ...created, popupBlocked: true }], expected: { ...waiting, popupBlocked: true } },
    { name: 'authenticated keeps the id and the username', actions: [{ type: 'authenticated', username: 'alice' }], from: waiting, expected: authenticated },
    { name: 'choose a server', actions: [{ type: 'chooseServer', serverId: 'm1' }], from: authenticated, expected: chosen },
    {
      name: 'its test results',
      actions: [{ type: 'tested', serverId: 'm1', results: [result], recommended: result.uri }],
      from: chosen,
      expected: { ...chosen, results: [result], recommended: result.uri } as PlexSignInState,
    },
    {
      name: 'another server resets the results',
      actions: [{ type: 'tested', serverId: 'm1', results: [result], recommended: result.uri }, { type: 'chooseServer', serverId: 'm2' }],
      from: chosen,
      expected: { ...chosen, serverId: 'm2' } as PlexSignInState,
    },
    { name: 'saved', actions: [{ type: 'saved' }], from: chosen, expected: { step: 'saved' } },
    { name: 'expired while waiting', actions: [{ type: 'expired' }], from: waiting, expected: { step: 'expired' } },
    { name: 'expired after the sign-in', actions: [{ type: 'expired' }], from: chosen, expected: { step: 'expired' } },
    { name: 'cancel while waiting → idle', actions: [{ type: 'cancel' }], from: waiting, expected: { step: 'idle' } },
    { name: 'cancel while creating → idle', actions: [{ type: 'start' }, { type: 'cancel' }], expected: { step: 'idle' } },
    { name: 'failure while creating', actions: [{ type: 'start' }, { type: 'failed', message: 'boom' }], expected: { step: 'error', message: 'boom' } },
    { name: 'failure while waiting', actions: [{ type: 'failed', message: 'gone' }], from: waiting, expected: { step: 'error', message: 'gone' } },
    {
      name: 'an invalid sign-in id → error',
      actions: [{ type: 'start' }, { ...created, signInId: '42' }],
      expected: { step: 'error', message: 'Bunkarr returned an unexpected sign-in, so the Plex window was not opened. Please try again.' },
    },
    {
      name: 'an untrusted authUrl → error',
      actions: [{ type: 'start' }, { ...created, authUrl: 'https://evil.example/auth' }],
      expected: { step: 'error', message: 'Bunkarr returned an unexpected sign-in, so the Plex window was not opened. Please try again.' },
    },
    { name: 'retry after expiry', actions: [{ type: 'start' }], from: { step: 'expired' }, expected: { step: 'creating' } },
    { name: 'retry after an error', actions: [{ type: 'start' }], from: { step: 'error', message: 'x' }, expected: { step: 'creating' } },
    { name: 'sign in again from a server choice', actions: [{ type: 'start' }], from: chosen, expected: { step: 'creating' } },
    { name: 'reset from a server choice', actions: [{ type: 'reset' }], from: chosen, expected: { step: 'idle' } },
  ])('$name', ({ actions, from, expected }) => {
    expect(run(actions, from)).toEqual(expected);
  });

  it.each<{ name: string; from: PlexSignInState; action: PlexSignInAction }>([
    { name: 'a late sign-in after cancel', from: { step: 'idle' }, action: created },
    { name: 'authenticated after expiry', from: { step: 'expired' }, action: { type: 'authenticated', username: 'x' } },
    { name: 'authenticated while idle', from: { step: 'idle' }, action: { type: 'authenticated', username: 'x' } },
    { name: 'a server choice while waiting', from: waiting, action: { type: 'chooseServer', serverId: 'm1' } },
    { name: 'the same server again', from: chosen, action: { type: 'chooseServer', serverId: 'm1' } },
    { name: 'results of another server', from: chosen, action: { type: 'tested', serverId: 'm2', results: [result], recommended: null } },
    { name: 'results without a server choice', from: authenticated, action: { type: 'tested', serverId: 'm1', results: [], recommended: null } },
    { name: 'expired while idle', from: { step: 'idle' }, action: { type: 'expired' } },
    { name: 'failure after the sign-in', from: authenticated, action: { type: 'failed', message: 'x' } },
    { name: 'double start while waiting', from: waiting, action: { type: 'start' } },
    { name: 'cancel after the sign-in', from: authenticated, action: { type: 'cancel' } },
    { name: 'saved while waiting', from: waiting, action: { type: 'saved' } },
  ])('ignores an action that does not apply: $name', ({ from, action }) => {
    expect(plexSignInReducer(from, action)).toBe(from);
  });

  it('never holds a token in any state', () => {
    const states: PlexSignInState[] = [
      PLEX_SIGN_IN_INITIAL,
      run([{ type: 'start' }]),
      run([{ type: 'start' }, created]),
      run([{ type: 'authenticated', username: 'alice' }], waiting),
      run([{ type: 'tested', serverId: 'm1', results: [result], recommended: result.uri }], chosen),
      run([{ type: 'saved' }], chosen),
      run([{ type: 'expired' }], waiting),
      run([{ type: 'failed', message: 'x' }], waiting),
    ];
    const keys = (v: unknown): string[] =>
      v && typeof v === 'object' ? Object.entries(v).flatMap(([k, inner]) => [k, ...keys(inner)]) : [];
    for (const s of states) {
      for (const k of keys(s)) {
        expect(k).not.toMatch(/^(token|authToken|accessToken|accountToken|apiKey|pin|pinId|code)$/i);
      }
    }
  });

  it('knows the sign-in of a state', () => {
    expect(signInIdOf(waiting)).toBe(ID);
    expect(signInIdOf(chosen)).toBe(ID);
    expect(signInIdOf({ step: 'saved' })).toBeNull();
    expect(signInIdOf(PLEX_SIGN_IN_INITIAL)).toBeNull();
    expect(isSignInId(ID)).toBe(true);
    expect(isSignInId(ID.toUpperCase())).toBe(false);
    expect(isSignInId('../x')).toBe(false);
  });
});

describe('isTrustedPlexAuthUrl', () => {
  it.each([
    [AUTH_URL, true],
    ['https://app.plex.tv/auth', true],
    ['https://app.plex.tv/auth/', false],
    ['https://app.plex.tv/desktop', false],
    ['https://plex.tv/link/?pin=abcd', false],
    ['http://app.plex.tv/auth#?code=x', false],
    ['https://app.plex.tv:8443/auth', false],
    ['https://user@app.plex.tv/auth', false],
    ['https://app.plex.tv.evil.example/auth', false],
    ['https://evil.app.plex.tv/auth', false],
    ['javascript:alert(1)', false],
    ['', false],
    [null, false],
    ['not a url', false],
  ])('%s → %s', (url, expected) => {
    expect(isTrustedPlexAuthUrl(url)).toBe(expected);
  });
});

describe('helpers', () => {
  it('counts down and never goes negative', () => {
    expect(remainingMs(10_000, 4_000)).toBe(6_000);
    expect(remainingMs(10_000, 12_000)).toBe(0);
  });

  it.each([
    [600_000, '10:00'],
    [245_000, '4:05'],
    [999, '0:01'],
    [0, '0:00'],
    [-10, '0:00'],
  ])('formatCountdown(%i) = %s', (ms, expected) => {
    expect(formatCountdown(ms)).toBe(expected);
  });

  const server = (over: Partial<PlexServerChoice>): PlexServerChoice => ({
    id: 'x',
    name: 'X',
    owned: true,
    productVersion: '',
    platform: '',
    hasAccessToken: true,
    connections: [],
    ...over,
  });

  it('sorts owned servers first, then by name', () => {
    const sorted = sortPlexServers([server({ name: 'b', owned: false }), server({ name: 'z' }), server({ name: 'a', owned: false }), server({ name: 'c' })]);
    expect(sorted.map((s) => s.name)).toEqual(['c', 'z', 'a', 'b']);
    expect(sortPlexServers(null)).toEqual([]);
  });

  it('never uses the account token for a shared server', () => {
    expect(tokenSource(server({ hasAccessToken: true }), false)).toBe('server');
    expect(tokenSource(server({ hasAccessToken: false }), false)).toBe('none');
    expect(tokenSource(server({ hasAccessToken: false }), true)).toBe('account');
    expect(tokenSource(server({ owned: false, hasAccessToken: false }), true)).toBe('none');
    expect(tokenSource(server({ owned: false, hasAccessToken: true }), true)).toBe('server');
  });

  it('asks "Use anyway" for a failed or unencrypted connection', () => {
    expect(needsUseAnyway(result)).toBe(false);
    expect(needsUseAnyway({ ...result, ok: false })).toBe(true);
    expect(needsUseAnyway({ ...result, protocol: 'http' })).toBe(true);
  });
});

describe('review fixes', () => {
  it('keeps the server\'s warnings: while waiting, and through the approval and the server choice', () => {
    const warned = run([{ type: 'pending', warning: 'plex.tv did not answer the last check; still waiting.' }], waiting);
    expect(warned).toEqual({ ...waiting, warning: 'plex.tv did not answer the last check; still waiting.' });
    expect(run([{ type: 'pending' }], warned)).toEqual(waiting);
    expect(plexSignInReducer(waiting, { type: 'pending' })).toBe(waiting);
    const approved = run([{ type: 'authenticated', username: '', warning: 'Signed in, but plex.tv did not say which account' }], warned);
    expect(approved).toEqual({ step: 'authenticated', signInId: ID, username: '', warning: 'Signed in, but plex.tv did not say which account' });
    expect(run([{ type: 'chooseServer', serverId: 'm1' }], approved)).toMatchObject({ step: 'serverChosen', warning: 'Signed in, but plex.tv did not say which account' });
  });

  it('forgets the old results while the same server is tested again', () => {
    const tested = run([{ type: 'tested', serverId: 'm1', results: [result], recommended: result.uri }], chosen);
    expect(run([{ type: 'testing', serverId: 'm1' }], tested)).toEqual(chosen);
    expect(plexSignInReducer(tested, { type: 'testing', serverId: 'm2' })).toBe(tested);
    expect(plexSignInReducer(chosen, { type: 'testing', serverId: 'm1' })).toBe(chosen);
  });

  it('reads expiresAt against the server\'s clock (its Date header), not this browser\'s', () => {
    const received = Date.parse('2026-09-25T12:15:00Z');
    // This browser is 15 minutes ahead: the raw expiresAt is already past here.
    expect(localDeadline('2026-09-25T12:10:00Z', 'Fri, 25 Sep 2026 12:00:00 GMT', received)).toBe(received + 600_000);
    // 5 minutes behind.
    expect(localDeadline('2026-09-25T12:30:00Z', 'Fri, 25 Sep 2026 12:20:00 GMT', received)).toBe(received + 600_000);
    // Never more than the 10 minutes a sign-in lasts, and 0 when the server says it is over.
    expect(localDeadline('2026-09-25T13:00:00Z', 'Fri, 25 Sep 2026 12:00:00 GMT', received)).toBe(received + PLEX_SIGN_IN_MAX_MS);
    expect(localDeadline('2026-09-25T12:00:00Z', 'Fri, 25 Sep 2026 12:00:01 GMT', received)).toBe(received);
    // No Date header: the time left at face value, and when that is past the server decides.
    expect(localDeadline('2026-09-25T12:20:00Z', null, received)).toBe(received + 300_000);
    expect(localDeadline('2026-09-25T12:10:00Z', null, received)).toBe(received + PLEX_SIGN_IN_MAX_MS);
    expect(localDeadline('2026-09-25T12:20:00Z', 'garbage', received)).toBe(received + 300_000);
    expect(localDeadline('not a date', null, received)).toBeNaN();
  });
});
