import '@testing-library/jest-dom/vitest';
import { cleanup, configure } from '@testing-library/react';
import { afterEach } from 'vitest';

// Async queries (findBy*, waitFor) wait up to 5 s instead of Testing Library's 1 s, so a loaded
// run (a busy CI runner) does not fail while a page renders; with the 30 s testTimeout of
// vite.config.ts. Runs before every test file (each has its own module graph).
configure({ asyncUtilTimeout: 5_000 });

afterEach(() => cleanup());
