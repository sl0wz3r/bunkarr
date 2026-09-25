import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { describe, expect, it } from 'vitest';
import { App } from './App';
import { mockFetch } from './test/fetch';

const signedIn = { setupRequired: false, authenticated: true, via: 'session', username: 'admin', authenticationRequired: 'enabled' };

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

describe('App', () => {
  it('shows the first-run setup when no user exists', async () => {
    mockFetch({ 'GET /api/v1/auth/status': () => ({ body: { ...signedIn, setupRequired: true, authenticated: false } }) });
    renderAt('/');
    expect(await screen.findByText('Welcome to Bunkarr')).toBeInTheDocument();
  });

  it('validates the setup form before calling the API', async () => {
    const calls = mockFetch({ 'GET /api/v1/auth/status': () => ({ body: { ...signedIn, setupRequired: true, authenticated: false } }) });
    renderAt('/');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Username'), 'admin');
    await user.type(screen.getByLabelText('Password'), 'longpassword');
    await user.type(screen.getByLabelText('Confirm password'), 'different1');
    await user.click(screen.getByRole('button', { name: 'Create login' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('do not match');
    expect(calls.some((c) => c.key.startsWith('POST'))).toBe(false);
  });

  it('logs in and shows the navigation', async () => {
    let authed = false;
    const calls = mockFetch({
      'GET /api/v1/auth/status': () => ({ body: authed ? signedIn : { ...signedIn, authenticated: false, via: undefined, username: null } }),
      'POST /api/v1/auth/login': () => {
        authed = true;
        return { body: { username: 'admin' } };
      },
    });
    renderAt('/');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Username'), 'admin');
    await user.type(screen.getByLabelText('Password'), 'correct horse');
    await user.click(screen.getByRole('button', { name: 'Log in' }));
    const nav = await screen.findByRole('navigation', { name: 'Main' });
    for (const label of ['Activity', 'Library', 'Destinations', 'Settings', 'System']) {
      expect(nav).toHaveTextContent(label);
    }
    expect(calls.find((c) => c.key === 'POST /api/v1/auth/login')?.body).toEqual({ username: 'admin', password: 'correct horse' });
    expect(await screen.findByRole('heading', { name: 'Queue' })).toBeInTheDocument();
  });

  it('shows the login error from the server', async () => {
    mockFetch({
      'GET /api/v1/auth/status': () => ({ body: { ...signedIn, authenticated: false, via: undefined, username: null } }),
      'POST /api/v1/auth/login': () => ({ status: 401, body: { message: 'invalid username or password' } }),
    });
    renderAt('/');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Username'), 'admin');
    await user.type(screen.getByLabelText('Password'), 'wrong');
    await user.click(screen.getByRole('button', { name: 'Log in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('invalid username or password');
  });

  it('masks the API key until revealed', async () => {
    mockFetch({
      'GET /api/v1/auth/status': () => ({ body: signedIn }),
      'GET /api/v1/settings/general': () => ({
        body: { apiKey: '0123456789abcdef0123456789abcdef', authenticationRequired: 'enabled', authenticationMethod: 'forms', bindAddress: '*', port: 8787 },
      }),
    });
    renderAt('/settings/general');
    const key = await screen.findByLabelText('API key');
    expect(key).not.toHaveValue('0123456789abcdef0123456789abcdef');
    await userEvent.setup().click(screen.getByTitle('Show'));
    await waitFor(() => expect(key).toHaveValue('0123456789abcdef0123456789abcdef'));
  });

  it('renders the system status', async () => {
    mockFetch({
      'GET /api/v1/auth/status': () => ({ body: signedIn }),
      'GET /api/v1/system/status': () => ({
        body: {
          appName: 'Bunkarr', version: '0.1.0', commit: 'abc1234', buildDate: '', startTime: '2026-09-24T00:00:00Z', uptimeSeconds: 3725,
          databasePath: '/config/bunkarr.db', schemaVersion: 1, configDir: '/config', goVersion: 'go1.27.1', os: 'linux', arch: 'amd64',
          isDocker: true, authenticationMethod: 'forms', authenticationRequired: 'enabled',
        },
      }),
    });
    renderAt('/system/status');
    expect(await screen.findByText('/config/bunkarr.db (schema v1)')).toBeInTheDocument();
    expect(screen.getByText('1h 2m')).toBeInTheDocument();
  });
});
