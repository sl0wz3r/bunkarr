// React Query setup: one client per app instance (so tests are isolated), no retries for client
// errors (4xx are answers, not glitches), no refetch on window focus (pages poll explicitly).

import { QueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';

/** POLL_MS is how often live pages (queue, running jobs) refresh (design §10: every 2 s). */
export const POLL_MS = 2000;

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: (failures, error) => !(error instanceof ApiError && error.status < 500) && failures < 2,
        staleTime: 5_000,
        refetchOnWindowFocus: false,
      },
      mutations: { retry: false },
    },
  });
}
