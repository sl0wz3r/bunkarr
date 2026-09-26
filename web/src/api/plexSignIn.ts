// "Sign in with Plex" (design §5). The browser only ever holds the sign-in id and a server id:
// every token stays on the server.

import { api, apiResponse } from './client';
import type { PlexProbeAnswer, PlexServerChoice, PlexSignInCreated, PlexSignInStatus } from './types';

/** A started sign-in, with the server's clock (its Date header) to read expiresAt against. */
export type PlexSignInStarted = PlexSignInCreated & { serverDate: string | null };

export async function startPlexSignIn(): Promise<PlexSignInStarted> {
  const { data, headers } = await apiResponse<PlexSignInCreated>('/plex/signin', { method: 'POST' });
  return data ? { ...data, serverDate: headers.get('Date') } : data;
}

export function getPlexSignIn(id: string): Promise<PlexSignInStatus> {
  return api<PlexSignInStatus>(`/plex/signin/${encodeURIComponent(id)}`);
}

export async function plexSignInServers(id: string): Promise<PlexServerChoice[]> {
  return (await api<PlexServerChoice[] | null>(`/plex/signin/${encodeURIComponent(id)}/servers`)) ?? [];
}

export function testPlexSignInServer(id: string, serverId: string, useAccountToken: boolean): Promise<PlexProbeAnswer> {
  return api<PlexProbeAnswer>(`/plex/signin/${encodeURIComponent(id)}/servers/${encodeURIComponent(serverId)}/test`, {
    method: 'POST',
    body: useAccountToken ? { useAccountToken: true } : {},
  });
}

/** cancelPlexSignIn forgets a sign-in on the server (idempotent). */
export function cancelPlexSignIn(id: string): Promise<void> {
  return api<void>(`/plex/signin/${encodeURIComponent(id)}`, { method: 'DELETE' });
}
