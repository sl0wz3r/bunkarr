import { availableParallelism } from 'node:os';
import { fileURLToPath, URL } from 'node:url';
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// Backend used by `npm run dev` (override with BUNKARR_URL=http://host:port npm run dev).
const backend = process.env.BUNKARR_URL ?? 'http://localhost:8787';

// Set to "true" by GitHub Actions and by the Gitea runner (act) alike.
const ci = process.env.CI === 'true';

export default defineConfig({
  base: '/',
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: backend, changeOrigin: true },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    css: false,
    restoreMocks: true,
    // Page tests render the whole app and click through it, so they take as long as the CPU they
    // get: several times a workstation's on a loaded machine, and on the shared Gitea runner
    // (other jobs and other projects' CI on one host) the suite took 71 s instead of 8 s. So every
    // test gets 30 s instead of Vitest's 5 s, and findBy*/waitFor 5 s instead of 1 s
    // (src/test/setup.ts).
    testTimeout: 30_000,
    // Vitest runs a worker per CPU but one, and availableParallelism is the host's CPU count in an
    // unlimited container: on a shared runner that many jsdom pages split the job's share of CPU,
    // each slower than when fewer run. On CI at most 4 files run at once (GitHub's 4-CPU runners
    // keep their 3).
    maxWorkers: ci ? Math.min(4, Math.max(1, availableParallelism() - 1)) : undefined,
  },
});
