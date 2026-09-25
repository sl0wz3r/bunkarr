import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { JobLog } from '@/api/types';

// Each log request waits until the test answers it, so the tests choose the order in which
// concurrent requests (a poll's pages, "Show earlier lines") come back.
type Req = { afterId: number; limit: number; resolve: (v: JobLog[]) => void; reject: (e: unknown) => void };
const reqs: Req[] = [];

vi.mock('@/api/jobs', () => ({
  jobLogs: (_job: number, afterId: number, limit: number) =>
    new Promise<JobLog[]>((resolve, reject) => {
      reqs.push({ afterId, limit, resolve, reject });
    }),
}));

import { LOG_PAGE, MAX_LOG_LINES, useJobLogs } from './JobLogs';

const line = (id: number): JobLog => ({ id, at: new Date().toISOString(), level: 'warn', message: `item ${id} failed`, fields: null });

// answer resolves r with the lines a server holding total lines returns for it.
async function answer(r: Req, total: number) {
  await act(async () => {
    r.resolve(Array.from({ length: Math.max(0, Math.min(r.limit, total - r.afterId)) }, (_, i) => line(r.afterId + i + 1)));
  });
}

async function tick() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(2100);
  });
}

// live renders the hook for a running job whose log holds MAX_LOG_LINES lines, answers the first
// pull (4 pages) and returns the hook.
async function live(jobId: number) {
  const hook = renderHook(() => useJobLogs(jobId, true));
  for (let i = 0; i < 4; i++) {
    await vi.waitFor(() => expect(reqs.length).toBe(i + 1));
    await answer(reqs[i], MAX_LOG_LINES);
  }
  expect(hook.result.current.logs).toHaveLength(MAX_LOG_LINES);
  return hook;
}

describe('useJobLogs with requests in flight', () => {
  beforeEach(() => {
    reqs.length = 0;
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('holds back the page of a poll that comes back while earlier lines load, and loads them back', async () => {
    const total = 3500;
    const { result } = await live(1);
    // A fast log: one poll pulls page after page.
    await tick();
    await vi.waitFor(() => expect(reqs.length).toBe(5));
    await answer(reqs[4], total); // lines 2001..2500: 500 dropped
    expect(result.current.hidden).toBe(LOG_PAGE);
    await vi.waitFor(() => expect(reqs.length).toBe(6)); // the poll's next page is in flight
    let done!: Promise<void>;
    act(() => {
      done = result.current.loadEarlier();
    });
    await vi.waitFor(() => expect(reqs.length).toBe(7));
    expect(reqs[6]).toMatchObject({ afterId: 0, limit: LOG_PAGE });
    // The poll's page comes back first: it would drop another 500 lines.
    await answer(reqs[5], total);
    await answer(reqs[6], total);
    await act(async () => {
      await done;
    });
    expect(result.current.logs[0].id).toBe(1);
    expect(result.current.logs.at(-1)!.id).toBe(2500);
    expect(result.current.hidden).toBe(0);
    expect(result.current.paused).toBe(true);
    // Polls wait; "Show new lines" fetches what was held back.
    await tick();
    await tick();
    expect(reqs).toHaveLength(7);
    act(() => {
      void result.current.loadMore();
    });
    await vi.waitFor(() => expect(reqs.length).toBe(8));
    expect(reqs[7].afterId).toBe(2500);
  });

  it('keeps the lines loaded back when a poll that was in flight comes back after them', async () => {
    const { result } = await live(2);
    await tick();
    await vi.waitFor(() => expect(reqs.length).toBe(5));
    await answer(reqs[4], 2500); // lines 2001..2500: 500 dropped
    await vi.waitFor(() => expect(reqs.length).toBe(6));
    await answer(reqs[5], 2500); // nothing more: the pull ends
    expect(result.current.hidden).toBe(LOG_PAGE);
    // The next poll is in flight while the log grows by 300 lines.
    await tick();
    await vi.waitFor(() => expect(reqs.length).toBe(7));
    const poll = reqs[6];
    let done!: Promise<void>;
    act(() => {
      done = result.current.loadEarlier();
    });
    await vi.waitFor(() => expect(reqs.length).toBe(8));
    await answer(reqs[7], 2800);
    await act(async () => {
      await done;
    });
    expect(result.current.logs[0].id).toBe(1);
    await answer(poll, 2800);
    expect(result.current.logs[0].id).toBe(1);
    expect(result.current.logs.at(-1)!.id).toBe(2500);
    expect(result.current.hidden).toBe(0);
    expect(result.current.paused).toBe(true);
  });

  it('follows new lines again, without the paused note, when loading earlier lines fails', async () => {
    const { result } = await live(3);
    await tick();
    await vi.waitFor(() => expect(reqs.length).toBe(5));
    await answer(reqs[4], 2500);
    await vi.waitFor(() => expect(reqs.length).toBe(6));
    await answer(reqs[5], 2500);
    expect(result.current.hidden).toBe(LOG_PAGE);
    let done!: Promise<void>;
    act(() => {
      done = result.current.loadEarlier();
    });
    await vi.waitFor(() => expect(reqs.length).toBe(7));
    // A slow server: a poll comes while the request is pending, and pauses new lines.
    await tick();
    expect(result.current.paused).toBe(true);
    await act(async () => {
      reqs[6].reject(new Error('502 Bad Gateway'));
      await done;
    });
    expect(result.current.error).toBeTruthy();
    expect(result.current.paused).toBe(false);
    await tick();
    await vi.waitFor(() => expect(reqs.length).toBe(8));
    expect(reqs[7].afterId).toBe(2500);
    await answer(reqs[7], 2600);
    expect(result.current.logs.at(-1)!.id).toBe(2600);
    expect(result.current.paused).toBe(false);
  });

  it('does not pause new lines again when earlier lines fail to load after "Show new lines"', async () => {
    const { result } = await live(4);
    await tick();
    for (let i = 4; i < 7; i++) {
      await vi.waitFor(() => expect(reqs.length).toBe(i + 1));
      await answer(reqs[i], 3000);
    }
    expect(result.current.hidden).toBe(2 * LOG_PAGE);
    // Earlier lines are loaded back once ...
    await act(async () => {
      const done = result.current.loadEarlier();
      await vi.waitFor(() => expect(reqs.length).toBe(8));
      await answer(reqs[7], 3000);
      await done;
    });
    expect(result.current.logs[0].id).toBe(501);
    // ... and again: the request is slow, a poll pauses new lines, and the user asks for them.
    let earlier!: Promise<void>;
    act(() => {
      earlier = result.current.loadEarlier();
    });
    await vi.waitFor(() => expect(reqs.length).toBe(9));
    const pending = reqs[8];
    await tick();
    expect(result.current.paused).toBe(true);
    act(() => {
      void result.current.loadMore();
    });
    await vi.waitFor(() => expect(reqs.length).toBe(10));
    await answer(reqs[9], 3100);
    expect(result.current.logs.at(-1)!.id).toBe(3100);
    await act(async () => {
      pending.reject(new Error('timeout'));
      await earlier;
    });
    // New lines keep coming: the failed request does not pin the window again.
    await tick();
    await vi.waitFor(() => expect(reqs.length).toBe(11));
    expect(reqs[10].afterId).toBe(3100);
    expect(result.current.paused).toBe(false);
  });

  it('still offers the last lines when the job finishes while earlier lines fail to load', async () => {
    const hook = renderHook(({ running }) => useJobLogs(5, running), { initialProps: { running: true } });
    const { result } = hook;
    for (let i = 0; i < 4; i++) {
      await vi.waitFor(() => expect(reqs.length).toBe(i + 1));
      await answer(reqs[i], MAX_LOG_LINES);
    }
    await tick();
    await vi.waitFor(() => expect(reqs.length).toBe(5));
    await answer(reqs[4], 2500);
    await vi.waitFor(() => expect(reqs.length).toBe(6));
    await answer(reqs[5], 2500);
    expect(result.current.hidden).toBe(LOG_PAGE);
    let done!: Promise<void>;
    act(() => {
      done = result.current.loadEarlier();
    });
    await vi.waitFor(() => expect(reqs.length).toBe(7));
    // The job finishes, with 40 more lines, while the request is slow: its last poll waits.
    hook.rerender({ running: false });
    await act(async () => {
      reqs[6].reject(new Error('502 Bad Gateway'));
      await done;
    });
    await tick();
    expect(reqs).toHaveLength(7);
    expect(result.current.paused).toBe(false);
    // No poll comes for a finished job: "Load more" fetches its last lines.
    expect(result.current.more).toBe(true);
    act(() => {
      void result.current.loadMore();
    });
    await vi.waitFor(() => expect(reqs.length).toBe(8));
    expect(reqs[7].afterId).toBe(2500);
    await answer(reqs[7], 2540);
    expect(result.current.logs.at(-1)!.id).toBe(2540);
    expect(result.current.more).toBe(false);
  });
});
