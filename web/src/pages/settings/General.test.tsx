import { screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderApp } from '@/test/render';

const KEY = '0123456789abcdef0123456789abcdef';

function routes() {
  return {
    'GET /api/v1/settings/general': () => ({
      body: { apiKey: KEY, authenticationRequired: 'enabled', authenticationMethod: 'forms', bindAddress: '*', port: 8787 },
    }),
  };
}

/** withExecCommand installs document.execCommand (jsdom has none) for one test. */
function withExecCommand(impl: (command: string) => boolean) {
  const fn = vi.fn(impl);
  Object.defineProperty(document, 'execCommand', { value: fn, configurable: true, writable: true });
  return fn;
}

afterEach(() => {
  Reflect.deleteProperty(document, 'execCommand');
});

describe('Settings → General → API key', () => {
  // D7, §16: webhooks never take the master key, so the help must not send users there.
  it('does not tell users to put the master API key into Sonarr/Radarr webhooks', async () => {
    const { user } = renderApp('/settings/general', routes());
    await screen.findByLabelText('API key');
    const help = screen.getByText(/X-Api-Key header/);
    expect(help.textContent).not.toMatch(/webhook/i);
    expect(help).toHaveTextContent('Settings → Connect');

    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    await user.click(screen.getByTitle('Regenerate'));
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(String(confirm.mock.calls[0][0])).not.toMatch(/webhook/i);
  });

  it('copies the key over plain http, where navigator.clipboard does not exist', async () => {
    const { user } = renderApp('/settings/general', routes());
    await screen.findByLabelText('API key');
    vi.spyOn(navigator, 'clipboard', 'get').mockReturnValue(undefined as unknown as Clipboard);
    let copied = '';
    const exec = withExecCommand((cmd) => {
      copied = (document.activeElement as HTMLTextAreaElement | null)?.value ?? '';
      return cmd === 'copy';
    });
    await user.click(screen.getByTitle('Copy'));
    expect(await screen.findByText('API key copied.')).toBeInTheDocument();
    expect(exec).toHaveBeenCalledWith('copy');
    expect(copied).toBe(KEY);
  });

  it('says so when the browser refuses to copy', async () => {
    const { user } = renderApp('/settings/general', routes());
    await screen.findByLabelText('API key');
    vi.spyOn(navigator, 'clipboard', 'get').mockReturnValue(undefined as unknown as Clipboard);
    withExecCommand(() => false);
    await user.click(screen.getByTitle('Copy'));
    expect(await screen.findByRole('alert')).toHaveTextContent(/copy it by hand/);
    await waitFor(() => expect(screen.getByLabelText('API key')).toHaveValue(KEY));
    expect(screen.queryByText('API key copied.')).not.toBeInTheDocument();
  });
});
