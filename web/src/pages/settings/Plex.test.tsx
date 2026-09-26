import { screen, waitFor, within } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { IntegrationInput, PlexProbeResult } from '@/api/types';
import { callsTo, type Handler } from '@/test/fetch';
import { destination, plexIntegration } from '@/test/fixtures';
import { renderApp } from '@/test/render';

// Poll quickly in tests (the app polls every 2 s).
vi.mock('@/components/plex/plexSignInMachine', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/components/plex/plexSignInMachine')>()),
  PLEX_POLL_MS: 20,
}));

const ID = 'fedcba9876543210fedcba9876543210';
const BASE = `/api/v1/plex/signin/${ID}`;
const AUTH_URL = 'https://app.plex.tv/auth#?clientID=7c6f&code=codeabcd&context%5Bdevice%5D%5Bproduct%5D=Bunkarr';
const HTTPS: PlexProbeResult = {
  uri: 'https://10-0-0-2.abc.plex.direct:32400',
  local: true,
  relay: false,
  derived: false,
  protocol: 'https',
  ok: true,
  identityMatches: true,
  tokenAccepted: true,
  latencyMs: 4,
  message: 'Connected (Plex Media Server 1.43.4).',
};

function fakePopup() {
  const popup = {
    closed: false,
    opener: window as unknown,
    location: { href: '' },
    document: { title: '', body: { textContent: '' } },
    close() {
      popup.closed = true;
    },
  };
  return popup;
}

function routes(extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/integrations': () => ({ body: [] }),
    'GET /api/v1/destinations': () => ({ body: [destination()] }),
    'POST /api/v1/plex/signin': () => ({ status: 201, body: { id: ID, authUrl: AUTH_URL, expiresAt: new Date(Date.now() + 600_000).toISOString() } }),
    [`GET ${BASE}`]: () => ({ body: { status: 'authenticated', username: 'alice', expiresAt: new Date(Date.now() + 1_200_000).toISOString() } }),
    [`GET ${BASE}/servers`]: () => ({
      body: [{ id: 'm1', name: 'Tower', owned: true, productVersion: '1.43.4', platform: 'Linux', hasAccessToken: true, connections: [] }],
    }),
    [`POST ${BASE}/servers/m1/test`]: () => ({ body: { recommended: HTTPS.uri, results: [HTTPS] } }),
    [`DELETE ${BASE}`]: () => ({ status: 204 }),
    ...extra,
  };
}

/** signInAndUse opens the add form, signs in and takes Tower's recommended connection. */
async function signInAndUse(r: ReturnType<typeof renderApp>) {
  const { user } = r;
  await user.click((await screen.findAllByRole('button', { name: 'Add Plex server' }))[0]);
  const form = within(await screen.findByRole('dialog', { name: 'Add Plex server' }));
  await user.click(form.getByRole('button', { name: 'Sign in with Plex' }));
  await user.click(await form.findByRole('radio', { name: /Tower/ }));
  const list = await form.findByRole('list', { name: 'Connections' });
  await user.click(await within(list).findByRole('button', { name: 'Use' }));
  return form;
}

beforeEach(() => {
  vi.spyOn(window, 'open').mockReturnValue(fakePopup() as unknown as Window);
});

describe('Settings → Plex → Sign in with Plex', () => {
  it('fills the form from the chosen server and saves with the sign-in instead of a token', async () => {
    const r = renderApp('/settings/plex', {
      ...routes(),
      'POST /api/v1/integrations/test': () => ({ body: { ok: true, message: 'Connected to Plex Media Server 1.43.4 (2 libraries).', machineIdentifier: 'm1' } }),
      'POST /api/v1/integrations': () => ({ status: 201, body: plexIntegration({ name: 'Tower', url: HTTPS.uri }) }),
    });
    const form = await signInAndUse(r);
    expect(form.getByText(/Signed in as/)).toHaveTextContent('Signed in as alice');
    expect(form.getByLabelText('URL')).toHaveValue(HTTPS.uri);
    expect(form.getByLabelText('Name')).toHaveValue('Tower');
    const token = form.getByLabelText(/Token/);
    expect(token).toHaveValue('');
    expect(token).toHaveAttribute('placeholder', 'From Plex sign-in — Tower (owner)');
    expect(form.getByText(/Token: from Plex sign-in — Tower \(owner/)).toBeInTheDocument();

    await r.user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText(/Connected to Plex Media Server/)).toBeInTheDocument();
    expect(callsTo(r.calls, 'POST /api/v1/integrations/test')[0].body).toEqual({ type: 'plex', url: HTTPS.uri, plexSignIn: { id: ID, serverId: 'm1' } });

    await r.user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(r.calls, 'POST /api/v1/integrations')).toHaveLength(1));
    const body = callsTo(r.calls, 'POST /api/v1/integrations')[0].body as IntegrationInput;
    expect(body).toMatchObject({ type: 'plex', name: 'Tower', url: HTTPS.uri, plexSignIn: { id: ID, serverId: 'm1' } });
    expect(body).not.toHaveProperty('apiKey');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    // The server consumed the sign-in: nothing to forget.
    expect(callsTo(r.calls, `DELETE ${BASE}`)).toHaveLength(0);
  });

  it('"Use a token instead" and typing a token end the sign-in', async () => {
    const r = renderApp('/settings/plex', {
      ...routes(),
      'POST /api/v1/integrations': () => ({ status: 201, body: plexIntegration() }),
    });
    const form = await signInAndUse(r);
    await r.user.click(form.getByRole('button', { name: 'Use a token instead' }));
    await waitFor(() => expect(callsTo(r.calls, `DELETE ${BASE}`)).toHaveLength(1));
    expect(form.getByLabelText(/Token/)).toHaveAttribute('placeholder', 'X-Plex-Token');
    expect(form.getByRole('button', { name: 'Sign in with Plex' })).toBeInTheDocument();

    // Sign in again, then type a token: the typed token is what is saved.
    await r.user.click(form.getByRole('button', { name: 'Sign in with Plex' }));
    await r.user.click(await form.findByRole('radio', { name: /Tower/ }));
    await r.user.click(await within(await form.findByRole('list', { name: 'Connections' })).findByRole('button', { name: 'Use' }));
    await r.user.type(form.getByLabelText(/Token/), 'typed-token');
    await waitFor(() => expect(callsTo(r.calls, `DELETE ${BASE}`)).toHaveLength(2));
    await r.user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(r.calls, 'POST /api/v1/integrations')).toHaveLength(1));
    const body = callsTo(r.calls, 'POST /api/v1/integrations')[0].body as IntegrationInput;
    expect(body.apiKey).toBe('typed-token');
    expect(body).not.toHaveProperty('plexSignIn');
  });

  it('closing the form without saving forgets the sign-in', async () => {
    const r = renderApp('/settings/plex', routes());
    const form = await signInAndUse(r);
    await r.user.click(form.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await waitFor(() => expect(callsTo(r.calls, `DELETE ${BASE}`)).toHaveLength(1));
  });

  it('keeps the manual path of Phase 1 when nobody signs in', async () => {
    const r = renderApp('/settings/plex', {
      ...routes(),
      'GET /api/v1/integrations': () => ({ body: [plexIntegration()] }),
      'PUT /api/v1/integrations/3': () => ({ body: plexIntegration() }),
    });
    await r.user.click(within(await screen.findByRole('article', { name: 'Plex' })).getByRole('button', { name: 'Edit Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    expect(form.getByText('Or enter the details manually.')).toBeInTheDocument();
    await r.user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(r.calls, 'PUT /api/v1/integrations/3')).toHaveLength(1));
    const body = callsTo(r.calls, 'PUT /api/v1/integrations/3')[0].body as IntegrationInput;
    expect(body).not.toHaveProperty('apiKey');
    expect(body).not.toHaveProperty('plexSignIn');
    expect(callsTo(r.calls, 'POST /api/v1/plex/signin')).toHaveLength(0);
  });

  it('keeps the library index (set in Settings → Connect) when the server is edited here', async () => {
    const index = { enabled: true, cron: '0 1 * * *', staleAfterHours: 72 };
    const indexed = plexIntegration({ settings: { ...plexIntegration().settings, index } });
    const r = renderApp('/settings/plex', {
      ...routes(),
      'GET /api/v1/integrations': () => ({ body: [indexed] }),
      'PUT /api/v1/integrations/3': () => ({ body: indexed }),
    });
    await r.user.click(within(await screen.findByRole('article', { name: 'Plex' })).getByRole('button', { name: 'Edit Plex' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit Plex server · Plex' }));
    await r.user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(r.calls, 'PUT /api/v1/integrations/3')).toHaveLength(1));
    const body = callsTo(r.calls, 'PUT /api/v1/integrations/3')[0].body as IntegrationInput;
    expect(body.settings.index).toEqual(index);
    expect(body.settings.dataPath).toBe('/plex');
  });
});

describe('Settings → Plex → Sign in with Plex: the selection follows the picker', () => {
  const tower = { id: 'm1', name: 'Tower', owned: true, productVersion: '1.43.4', platform: 'Linux', hasAccessToken: true, connections: [] };
  const basement = { ...tower, id: 'm2', name: 'Basement', hasAccessToken: false };

  it('unticking "Use my Plex account token" drops the connection taken with it', async () => {
    const r = renderApp('/settings/plex', {
      ...routes({
        [`GET ${BASE}/servers`]: () => ({ body: [basement] }),
        [`POST ${BASE}/servers/m2/test`]: () => ({ body: { recommended: HTTPS.uri, results: [HTTPS] } }),
      }),
      'POST /api/v1/integrations': () => ({ status: 201, body: plexIntegration() }),
    });
    const { user } = r;
    await user.click((await screen.findAllByRole('button', { name: 'Add Plex server' }))[0]);
    const form = within(await screen.findByRole('dialog', { name: 'Add Plex server' }));
    await user.click(form.getByRole('button', { name: 'Sign in with Plex' }));
    await user.click(await form.findByRole('radio', { name: /Basement/ }));
    const box = await form.findByRole('checkbox', { name: 'Use my Plex account token for this server' });
    await user.click(box);
    await waitFor(() => expect(callsTo(r.calls, `POST ${BASE}/servers/m2/test`)).toHaveLength(2));
    const use = await within(await form.findByRole('list', { name: 'Connections' })).findByRole('button', { name: 'Use' });
    await waitFor(() => expect(use).toBeEnabled());
    await user.click(use);
    expect(form.getByText(/Token: from Plex sign-in — Basement \(owner, your account token/)).toBeInTheDocument();

    // Taking the permission back takes the choice back.
    await user.click(box);
    expect(form.queryByText(/Token: from Plex sign-in/)).not.toBeInTheDocument();
    await waitFor(() => expect(callsTo(r.calls, `POST ${BASE}/servers/m2/test`)).toHaveLength(3));
    const list = await form.findByRole('list', { name: 'Connections' });
    expect(within(list).queryByText('Selected')).not.toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('Enter the Plex token.')).toBeInTheDocument();
    expect(callsTo(r.calls, 'POST /api/v1/integrations')).toHaveLength(0);
  });

  it('choosing another server drops the connection taken on the first one', async () => {
    const r = renderApp('/settings/plex', {
      ...routes({
        [`GET ${BASE}/servers`]: () => ({ body: [tower, basement] }),
        [`POST ${BASE}/servers/m2/test`]: () => ({ body: { recommended: HTTPS.uri, results: [HTTPS] } }),
      }),
      'POST /api/v1/integrations': () => ({ status: 201, body: plexIntegration() }),
    });
    const form = await signInAndUse(r);
    expect(form.getByText(/Token: from Plex sign-in — Tower/)).toBeInTheDocument();
    await r.user.click(form.getByRole('radio', { name: /Basement/ }));
    expect(form.queryByText(/Token: from Plex sign-in/)).not.toBeInTheDocument();
    await r.user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('Enter the Plex token.')).toBeInTheDocument();
    expect(callsTo(r.calls, 'POST /api/v1/integrations')).toHaveLength(0);
  });
});
