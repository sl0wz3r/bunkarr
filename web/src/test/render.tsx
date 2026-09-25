import { render } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { App } from '@/App';
import { mockFetch, type Handler } from './fetch';

export const signedIn = { setupRequired: false, authenticated: true, via: 'session', username: 'admin', authenticationRequired: 'enabled' };

/** renderApp renders the whole app, signed in, at path with the given API routes mocked. */
export function renderApp(path: string, routes: Record<string, Handler>) {
  const calls = mockFetch({ 'GET /api/v1/auth/status': () => ({ body: signedIn }), ...routes });
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
  return { calls, user, ...view };
}
