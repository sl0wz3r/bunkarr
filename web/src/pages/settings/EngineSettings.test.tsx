import { screen, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { EngineSettings } from '@/api/types';
import { callsTo } from '@/test/fetch';
import { renderApp } from '@/test/render';

// Settings → General → Engines (docs/design/phase4.md §9.3, §12): upload slots (read at start-up,
// so a change applies after a restart) and the retry budget.

const general = {
  'GET /api/v1/settings/general': () => ({ body: { apiKey: 'k', authenticationRequired: 'enabled', authenticationMethod: 'forms', bindAddress: '*', port: 8787 } }),
};

const engines = (over: Partial<EngineSettings> = {}): EngineSettings => ({
  uploadSlots: 2,
  retryBudgetMinutes: 10,
  uploadSlotsInEffect: 2,
  restartRequired: false,
  note: 'The number of upload slots is read when Bunkarr starts: a change applies after a restart. The retry budget applies to the next job.',
  ...over,
});

describe('Settings → General → Engines', () => {
  it('saves the slots and the retry budget and says a restart applies the slots', async () => {
    const { calls, user } = renderApp('/settings/general', {
      ...general,
      'GET /api/v1/settings/engines': () => ({ body: engines() }),
      'PUT /api/v1/settings/engines': (init) => {
        const b = JSON.parse(String(init?.body)) as { uploadSlots: number; retryBudgetMinutes: number };
        return { body: engines({ ...b, restartRequired: b.uploadSlots !== 2 }) };
      },
    });
    const slots = await screen.findByLabelText('Upload slots');
    expect(slots).toHaveValue(2);
    expect(screen.getByText(/a change applies after a restart/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save engine settings' })).toBeDisabled();

    await user.clear(slots);
    await user.type(slots, '9');
    expect(screen.getByText('Upload slots must be 1 to 8.')).toBeInTheDocument();
    await user.clear(slots);
    await user.type(slots, '4');
    const budget = screen.getByLabelText('Retry budget');
    await user.clear(budget);
    await user.type(budget, '30');
    await user.click(screen.getByRole('button', { name: 'Save engine settings' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/settings/engines')).toHaveLength(1));
    expect(callsTo(calls, 'PUT /api/v1/settings/engines')[0].body).toEqual({ uploadSlots: 4, retryBudgetMinutes: 30 });
    expect(await screen.findByText('Restart Bunkarr to use 4 upload slots: 2 in effect now.')).toBeInTheDocument();
  });
});
