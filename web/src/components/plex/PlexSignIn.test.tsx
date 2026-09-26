import { QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { PlexProbeAnswer, PlexProbeResult, PlexServerChoice, PlexSignInStatus } from '@/api/types';
import { createQueryClient } from '@/lib/queryClient';
import { callsTo, mockFetch, type Handler } from '@/test/fetch';
import { PlexSignInPanel, usePlexSignIn } from './PlexSignIn';
import type { PlexSignInSelection } from './plexSignInMachine';

// Poll quickly in tests (the app polls every 2 s).
vi.mock('./plexSignInMachine', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./plexSignInMachine')>()),
  PLEX_POLL_MS: 20,
}));

const ID = '0123456789abcdef0123456789abcdef';
const AUTH_URL = 'https://app.plex.tv/auth#?clientID=7c6f&code=codeabcd&context%5Bdevice%5D%5Bproduct%5D=Bunkarr';
const BASE = `/api/v1/plex/signin/${ID}`;

const TOWER: PlexServerChoice = {
  id: 'm1',
  name: 'Tower',
  owned: true,
  productVersion: '1.43.4',
  platform: 'Linux',
  hasAccessToken: true,
  connections: [],
};
const BASEMENT: PlexServerChoice = { ...TOWER, id: 'm2', name: 'Basement', hasAccessToken: false };
const FRIEND: PlexServerChoice = { ...TOWER, id: 'm3', name: 'Friend', owned: false, hasAccessToken: false };

const HTTPS: PlexProbeResult = {
  uri: 'https://10-0-0-2.abc.plex.direct:32400',
  local: true,
  relay: false,
  derived: false,
  protocol: 'https',
  ok: true,
  identityMatches: true,
  tokenAccepted: true,
  version: '1.43.4',
  latencyMs: 4,
  message: 'Connected (Plex Media Server 1.43.4).',
};
const DERIVED: PlexProbeResult = { ...HTTPS, uri: 'http://10.0.0.2:32400', protocol: 'http', derived: true, latencyMs: 2 };
const RELAY: PlexProbeResult = { ...HTTPS, uri: 'https://relay.abc.plex.direct:8443', local: false, relay: true, ok: false, message: 'No answer within 5s.' };

interface FakePopup {
  closed: boolean;
  opener: unknown;
  location: { href: string };
  document: { title: string; body: { textContent: string } };
  closeCalls: number;
  close: () => void;
}

function fakePopup(): FakePopup {
  const popup: FakePopup = {
    closed: false,
    opener: window,
    location: { href: '' },
    document: { title: '', body: { textContent: '' } },
    closeCalls: 0,
    close() {
      popup.closeCalls += 1;
      popup.closed = true;
    },
  };
  return popup;
}

function Harness({ onSelect }: { onSelect: (s: PlexSignInSelection) => void }) {
  const controller = usePlexSignIn();
  const [selection, setSelection] = useState<PlexSignInSelection | null>(null);
  return (
    <PlexSignInPanel
      controller={controller}
      selection={selection}
      onSelect={(s) => {
        setSelection(s);
        onSelect(s);
      }}
    />
  );
}

function renderPanel(routes: Record<string, Handler>) {
  const calls = mockFetch(routes);
  const onSelect = vi.fn();
  const user = userEvent.setup();
  const view = render(
    <QueryClientProvider client={createQueryClient()}>
      <Harness onSelect={onSelect} />
    </QueryClientProvider>,
  );
  return { calls, onSelect, user, ...view };
}

/** routes answers a sign-in that is approved after `pendingPolls` polls. */
function routes(over: Partial<Record<string, Handler>> = {}, opts: { pendingPolls?: number; servers?: PlexServerChoice[]; probe?: PlexProbeAnswer } = {}) {
  let polls = 0;
  const base: Record<string, Handler> = {
    'POST /api/v1/plex/signin': () => ({ status: 201, body: { id: ID, authUrl: AUTH_URL, expiresAt: new Date(Date.now() + 600_000).toISOString() } }),
    [`GET ${BASE}`]: () => ({
      body: (++polls > (opts.pendingPolls ?? 1)
        ? { status: 'authenticated', expiresAt: new Date(Date.now() + 1_200_000).toISOString(), username: 'alice' }
        : { status: 'pending', expiresAt: new Date(Date.now() + 600_000).toISOString() }) satisfies PlexSignInStatus,
    }),
    [`GET ${BASE}/servers`]: () => ({ body: opts.servers ?? [TOWER, FRIEND] }),
    [`POST ${BASE}/servers/m1/test`]: () => ({ body: opts.probe ?? { recommended: HTTPS.uri, results: [HTTPS, DERIVED, RELAY] } }),
    [`DELETE ${BASE}`]: () => ({ status: 204 }),
  };
  return { ...base, ...over } as Record<string, Handler>;
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe('PlexSignIn', () => {
  it('opens the Plex window, waits for the approval, lists servers and hands back a tested connection', async () => {
    const popup = fakePopup();
    const open = vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    const { calls, onSelect, user } = renderPanel(routes());

    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    // The popup is opened inside the click, cut off from this page, with a placeholder.
    expect(open).toHaveBeenCalledWith('', 'bunkarr-plex-auth', expect.stringContaining('width=600'));
    expect(popup.opener).toBeNull();
    expect(popup.document.title).toBe('Sign in with Plex');
    await waitFor(() => expect(popup.location.href).toBe(AUTH_URL));
    expect(await screen.findByText('Waiting for Plex sign-in…')).toBeInTheDocument();
    expect(screen.getByText(/\(\d+:\d\d left\)/)).toBeInTheDocument();

    expect(await screen.findByText('alice')).toBeInTheDocument();
    expect(screen.getByText(/Signed in as/)).toBeInTheDocument();
    expect(popup.closeCalls).toBe(1);

    const radios = await screen.findAllByRole('radio');
    expect(radios.map((r) => r.closest('label')?.textContent)).toEqual([expect.stringContaining('Tower'), expect.stringContaining('Friend')]);
    expect(within(radios[0].closest('label') as HTMLElement).getByText('Owner')).toBeInTheDocument();
    expect(within(radios[1].closest('label') as HTMLElement).getByText('Shared')).toBeInTheDocument();

    await user.click(radios[0]);
    const list = await screen.findByRole('list', { name: 'Connections' });
    const rows = within(list).getAllByRole('listitem');
    expect(rows).toHaveLength(3);
    const first = within(rows[0]);
    expect(first.getByText('Recommended')).toBeInTheDocument();
    expect(first.getByText('Local')).toBeInTheDocument();
    expect(first.getByText('HTTPS')).toBeInTheDocument();
    expect(first.getByText(/4 ms/)).toBeInTheDocument();
    const second = within(rows[1]);
    expect(second.getByText('Unencrypted')).toBeInTheDocument();
    expect(second.getByText('Derived')).toBeInTheDocument();
    expect(within(rows[2]).getByText('Relay')).toBeInTheDocument();
    expect(within(rows[2]).getByText('No answer within 5s.')).toBeInTheDocument();
    expect(callsTo(calls, `POST ${BASE}/servers/m1/test`)[0].body).toEqual({});

    await user.click(first.getByRole('button', { name: 'Use' }));
    expect(onSelect).toHaveBeenCalledWith({ signInId: ID, serverId: 'm1', serverName: 'Tower', owned: true, uri: HTTPS.uri, useAccountToken: false });
    expect(first.getByText('Selected')).toBeInTheDocument();

    // Nothing the browser sent or received names a token.
    for (const c of calls) {
      expect(JSON.stringify(c.body ?? {})).not.toMatch(/token"/i);
    }
  });

  it('asks before using an unencrypted or failed connection', async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    const { onSelect, user } = renderPanel(routes({}, { pendingPolls: 0 }));
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    await user.click((await screen.findAllByRole('radio'))[0]);
    const rows = within(await screen.findByRole('list', { name: 'Connections' })).getAllByRole('listitem');
    const http = within(rows[1]);
    await user.click(http.getByRole('button', { name: 'Use…' }));
    expect(onSelect).not.toHaveBeenCalled();
    expect(http.getByText(/not encrypted/)).toBeInTheDocument();
    await user.click(http.getByRole('button', { name: 'Use anyway' }));
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ uri: DERIVED.uri }));
    const relay = within(rows[2]);
    await user.click(relay.getByRole('button', { name: 'Use…' }));
    expect(relay.getByText(/did not work from Bunkarr/)).toBeInTheDocument();
  });

  it('uses the account token for an owned server without a token only when allowed', async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    const { calls, onSelect, user } = renderPanel(
      routes(
        {
          [`POST ${BASE}/servers/m2/test`]: () => ({ body: { recommended: HTTPS.uri, results: [HTTPS] } }),
        },
        { pendingPolls: 0, servers: [BASEMENT, FRIEND] },
      ),
    );
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    await user.click((await screen.findAllByRole('radio'))[0]);
    expect(await screen.findByText('plex.tv lists no token for this server')).toBeInTheDocument();
    const list = await screen.findByRole('list', { name: 'Connections' });
    expect(within(list).getByRole('button', { name: 'Use' })).toBeDisabled();
    await user.click(screen.getByRole('checkbox', { name: 'Use my Plex account token for this server' }));
    await waitFor(() => expect(callsTo(calls, `POST ${BASE}/servers/m2/test`)).toHaveLength(2));
    expect(callsTo(calls, `POST ${BASE}/servers/m2/test`)[1].body).toEqual({ useAccountToken: true });
    const use = await within(await screen.findByRole('list', { name: 'Connections' })).findByRole('button', { name: 'Use' });
    await waitFor(() => expect(use).toBeEnabled());
    await user.click(use);
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ serverId: 'm2', useAccountToken: true }));
  });

  it('never offers a shared server without a token', async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    const { user } = renderPanel(
      routes({ [`POST ${BASE}/servers/m3/test`]: () => ({ body: { recommended: null, results: [HTTPS] } }) }, { pendingPolls: 0, servers: [FRIEND] }),
    );
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    await user.click(await screen.findByRole('radio'));
    expect(await screen.findByText('No token for this shared server')).toBeInTheDocument();
    expect(screen.getByText("You don't own this server")).toBeInTheDocument();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
    const list = await screen.findByRole('list', { name: 'Connections' });
    expect(within(list).getByRole('button', { name: 'Use' })).toBeDisabled();
  });

  it('does not open an untrusted sign-in address and forgets that sign-in', async () => {
    const popup = fakePopup();
    vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    const { calls, user } = renderPanel(
      routes({ 'POST /api/v1/plex/signin': () => ({ status: 201, body: { id: ID, authUrl: 'https://evil.example/auth', expiresAt: new Date().toISOString() } }) }),
    );
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    expect(await screen.findByText(/unexpected sign-in address/)).toBeInTheDocument();
    expect(popup.location.href).toBe('');
    expect(popup.closeCalls).toBe(1);
    await waitFor(() => expect(callsTo(calls, `DELETE ${BASE}`)).toHaveLength(1));
  });

  it('offers a link when the popup is blocked', async () => {
    vi.spyOn(window, 'open').mockReturnValue(null);
    const { user } = renderPanel(routes({}, { pendingPolls: 1000 }));
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    const link = await screen.findByRole('link', { name: /Open Plex sign-in/ });
    expect(link).toHaveAttribute('href', AUTH_URL);
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    expect(link).toHaveAttribute('target', '_blank');
  });

  it('shows an expired sign-in and starts again', async () => {
    const popup = fakePopup();
    vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    const { user } = renderPanel(routes({ [`GET ${BASE}`]: () => ({ body: { status: 'expired', expiresAt: new Date().toISOString() } }) }));
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    expect(await screen.findByText('The Plex sign-in expired')).toBeInTheDocument();
    expect(popup.closeCalls).toBe(1);
    expect(screen.getByRole('button', { name: 'Sign in again' })).toBeEnabled();
  });

  it('reports a failed start', async () => {
    const popup = fakePopup();
    vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
    const { user } = renderPanel(routes({ 'POST /api/v1/plex/signin': () => ({ status: 502, body: { message: 'plex.tv did not create a sign-in PIN' } }) }));
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    expect(await screen.findByText('plex.tv did not create a sign-in PIN')).toBeInTheDocument();
    expect(popup.closeCalls).toBe(1);
  });

  it('forgets the sign-in on cancel and when it unmounts without saving', async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    const { calls, user, unmount } = renderPanel(routes({}, { pendingPolls: 1000 }));
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(callsTo(calls, `DELETE ${BASE}`)).toHaveLength(1));
    expect(screen.getByRole('button', { name: 'Sign in with Plex' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    expect(await screen.findByText('Waiting for Plex sign-in…')).toBeInTheDocument();
    unmount();
    await waitFor(() => expect(callsTo(calls, `DELETE ${BASE}`)).toHaveLength(2));
  });
});

describe('PlexSignIn (review fixes)', () => {
  it("shows the server's warnings: while waiting, and on an approval that names no account", async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    let polls = 0;
    const { user } = renderPanel(
      routes({
        [`GET ${BASE}`]: () => ({
          body: (++polls < 8
            ? { status: 'pending', expiresAt: new Date(Date.now() + 600_000).toISOString(), warning: 'plex.tv did not answer the last check; still waiting.' }
            : {
                status: 'authenticated',
                expiresAt: new Date(Date.now() + 1_200_000).toISOString(),
                warning: 'Signed in, but plex.tv did not say which account; check the servers listed.',
              }) satisfies PlexSignInStatus,
        }),
      }),
    );
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    expect(await screen.findByText('plex.tv did not answer the last check; still waiting.')).toBeInTheDocument();
    expect(await screen.findByText(/Signed in, but plex.tv did not say which account/)).toBeInTheDocument();
    expect(screen.getByText('Signed in to Plex')).toBeInTheDocument();
    // It stays while a server is chosen.
    await user.click((await screen.findAllByRole('radio'))[0]);
    await screen.findByRole('list', { name: 'Connections' });
    expect(screen.getByText(/Signed in, but plex.tv did not say which account/)).toBeInTheDocument();
  });

  it("counts down from the time the server gives, even when this browser's clock is 15 minutes ahead", async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    const serverNow = Date.now() - 15 * 60_000;
    const { calls } = renderPanel(
      routes(
        {
          'POST /api/v1/plex/signin': () => ({
            status: 201,
            body: { id: ID, authUrl: AUTH_URL, expiresAt: new Date(serverNow + 600_000).toISOString() },
            headers: { Date: new Date(serverNow).toUTCString() },
          }),
        },
        { pendingPolls: 1000 },
      ),
    );
    await userEvent.setup().click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    expect(await screen.findByText(/\((10:00|9:5\d) left\)/)).toBeInTheDocument();
    // Long enough for a timer set from the raw expiresAt (already past here) to fire.
    await waitFor(() => expect(callsTo(calls, `GET ${BASE}`).length).toBeGreaterThanOrEqual(3));
    expect(screen.queryByText('The Plex sign-in expired')).not.toBeInTheDocument();
    expect(screen.getByText('Waiting for Plex sign-in…')).toBeInTheDocument();
    expect(callsTo(calls, `DELETE ${BASE}`)).toHaveLength(0);
  });

  it('keeps the per-second countdown out of the live region', async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    const { user } = renderPanel(routes({}, { pendingPolls: 1000 }));
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    const status = (await screen.findByText('Waiting for Plex sign-in…')).closest('[role="status"]') as HTMLElement;
    expect(status).not.toBeNull();
    const countdown = within(status).getByText(/\(\d+:\d\d left\)/);
    expect(countdown.closest('[aria-hidden="true"]')).not.toBeNull();
    // Screen readers get a fixed expiry time instead.
    expect(status).toHaveTextContent(/expires at \d/);
  });

  it('shows a failed connection test as a failure, not as "no connection", and tests again', async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    let fail = true;
    const { user } = renderPanel(
      routes(
        {
          [`POST ${BASE}/servers/m1/test`]: () =>
            fail ? { status: 502, body: { message: 'Bunkarr could not test the connections' } } : { body: { recommended: HTTPS.uri, results: [HTTPS] } },
        },
        { pendingPolls: 0 },
      ),
    );
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    await user.click((await screen.findAllByRole('radio'))[0]);
    expect(await screen.findByText('Bunkarr could not test the connections')).toBeInTheDocument();
    expect(screen.queryByText(/lists no connection/)).not.toBeInTheDocument();
    expect(screen.queryByText(/connections from Bunkarr/)).not.toBeInTheDocument();
    const again = screen.getByRole('button', { name: 'Test again' });
    expect(again).toBeEnabled();

    fail = false;
    await user.click(again);
    const list = await screen.findByRole('list', { name: 'Connections' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(1);
    expect(screen.queryByText('Bunkarr could not test the connections')).not.toBeInTheDocument();
  });

  it('a new test of the same server replaces the old results while it runs', async () => {
    vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
    let fail = false;
    const { user } = renderPanel(
      routes(
        {
          [`POST ${BASE}/servers/m1/test`]: () =>
            fail ? { status: 500, body: { message: 'test failed' } } : { body: { recommended: HTTPS.uri, results: [HTTPS, DERIVED] } },
        },
        { pendingPolls: 0 },
      ),
    );
    await user.click(screen.getByRole('button', { name: 'Sign in with Plex' }));
    await user.click((await screen.findAllByRole('radio'))[0]);
    expect(within(await screen.findByRole('list', { name: 'Connections' })).getAllByRole('listitem')).toHaveLength(2);
    fail = true;
    await user.click(screen.getByRole('button', { name: 'Test again' }));
    expect(await screen.findByText('test failed')).toBeInTheDocument();
    // The old rows (and their Use buttons) are gone with the failed test, which says nothing
    // about the server's connections either.
    expect(screen.queryByRole('list', { name: 'Connections' })).not.toBeInTheDocument();
    expect(screen.queryByText(/lists no connection/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Test again' })).toBeEnabled();
  });
});
