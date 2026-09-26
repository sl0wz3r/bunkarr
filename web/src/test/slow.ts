import { configure } from '@testing-library/react';
import { vi } from 'vitest';

/**
 * For the test files of a heavy page (Settings → Connect, heavier since Phase 3): async queries
 * (findBy*, waitFor) wait up to 5 s instead of Testing Library's 1 s, and a test up to 30 s, so a
 * loaded run (the whole suite in parallel) does not time out while the page renders. Call it once
 * at the top of the file; both settings are per test file (each file runs in its own module
 * graph).
 */
export function slowPage() {
  configure({ asyncUtilTimeout: 5_000 });
  vi.setConfig({ testTimeout: 30_000 });
}
