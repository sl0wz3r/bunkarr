import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { Link } from 'react-router';
import { jobLogs } from '@/api/jobs';
import type { JobLog, LogLevel } from '@/api/types';
import { Button } from '@/components/Button';
import { ErrorNotice } from '@/components/Notice';
import { formatNumber } from '@/lib/format';
import { POLL_MS } from '@/lib/queryClient';

/** LOG_PAGE is how many log lines one request fetches. */
export const LOG_PAGE = 500;
/**
 * MAX_LOG_LINES bounds the lines kept and rendered as a log grows (a sync where every file fails
 * logs a line per file): older lines are dropped, and loaded back a page at a time on request.
 */
export const MAX_LOG_LINES = 2000;
// Pages fetched per refresh before "Load more" takes over (keeps huge finished logs cheap).
const PAGES_PER_PULL = 4;

/**
 * Dropped is a run of lines dropped from the start of the window: the afterId that fetches it
 * again. Runs are adjacent (each starts where the one before ends) and hold LOG_PAGE lines, all
 * but the newest.
 */
interface Dropped {
  afterId: number;
  count: number;
}

/**
 * useJobLogs loads a job's log incrementally (afterId) and, while the job is live, polls for new
 * lines every 2 s. When the job finishes it fetches once more to get the last lines. It keeps at
 * most MAX_LOG_LINES lines (plus those loaded back with loadEarlier): earlier ones are dropped
 * and counted in hidden. From the moment earlier lines are asked for (loadEarlier), new ones
 * wait (paused) until loadMore: they would push them out again.
 */
export function useJobLogs(jobId: number, live: boolean) {
  const [logs, setLogs] = useState<JobLog[]>([]);
  const [hidden, setHidden] = useState(0);
  const [more, setMore] = useState(false);
  const [paused, setPaused] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const after = useRef(0);
  const epoch = useRef(0);
  // The window of lines (logs is its copy for rendering), the runs dropped from its start (the
  // newest last) and its size limit, which grows by what loadEarlier brings back.
  const lines = useRef<JobLog[]>([]);
  const dropped = useRef<Dropped[]>([]);
  const cap = useRef(MAX_LOG_LINES);
  // Set by loadEarlier: polls leave the window alone until loadMore.
  const pinned = useRef(false);
  // Counts changes of pinned: a loadEarlier that fails undoes its own change only.
  const pins = useRef(0);

  const publish = useCallback(() => {
    setLogs(lines.current);
    setHidden(dropped.current.reduce((n, d) => n + d.count, 0));
  }, []);

  const append = useCallback(
    (fresh: JobLog[]) => {
      let next = [...lines.current, ...fresh];
      const over = next.length - cap.current;
      if (over > 0) {
        const runs = dropped.current;
        for (let i = 0; i < over; ) {
          const last = runs[runs.length - 1];
          if (last && last.count < LOG_PAGE) {
            // Fill up the newest run (a new object: loadEarlier checks that its run is unchanged).
            const n = Math.min(LOG_PAGE - last.count, over - i);
            runs[runs.length - 1] = { afterId: last.afterId, count: last.count + n };
            i += n;
          } else {
            const n = Math.min(LOG_PAGE, over - i);
            runs.push({ afterId: next[i].id - 1, count: n });
            i += n;
          }
        }
        next = next.slice(over);
      }
      lines.current = next;
      publish();
    },
    [publish],
  );

  const pull = useCallback(async () => {
    const mine = epoch.current;
    try {
      for (let i = 0; i < PAGES_PER_PULL; i++) {
        const page = await jobLogs(jobId, after.current, LOG_PAGE);
        if (mine !== epoch.current) {
          return;
        }
        // Concurrent pulls may fetch the same page; keep only lines past the newest one we have.
        const fresh = page.filter((l) => l.id > after.current);
        if (fresh.length > 0 && pinned.current) {
          // loadEarlier ran meanwhile: these lines would push its lines out again. They wait
          // for loadMore, as a poll's do.
          setPaused(true);
          setMore(true);
          return;
        }
        if (fresh.length > 0) {
          after.current = fresh[fresh.length - 1].id;
          append(fresh);
        }
        const full = page.length >= LOG_PAGE;
        setMore(full && i === PAGES_PER_PULL - 1);
        if (!full) {
          break;
        }
      }
      setError(null);
    } catch (e) {
      if (mine === epoch.current) {
        setError(e);
      }
    }
  }, [jobId, append]);

  // loadMore fetches the lines after the newest one (new lines of a paused log, or the next
  // pages of a long one).
  const loadMore = useCallback(() => {
    pinned.current = false;
    pins.current++;
    setPaused(false);
    return pull();
  }, [pull]);

  // poll is loadMore on a timer: it waits while lines loaded back are shown. What it skips stays
  // offered (more) should the pause end without a poll to fetch it: the job's last poll, when a
  // loadEarlier that fails unpins after the job finished.
  const poll = useCallback(() => {
    if (pinned.current) {
      setPaused(true);
      setMore(true);
      return;
    }
    void pull();
  }, [pull]);

  // loadEarlier puts the lines dropped last back at the start: the newest run, with the run
  // before it when it is not a whole page (at most 2 * LOG_PAGE - 1 lines, within the server's
  // limit of 1,000).
  const loadEarlier = useCallback(async () => {
    const runs = dropped.current;
    const newest = runs[runs.length - 1];
    if (!newest) {
      return;
    }
    const take = newest.count < LOG_PAGE && runs.length > 1 ? 2 : 1;
    const from = runs[runs.length - take];
    const count = take === 2 ? from.count + newest.count : newest.count;
    const mine = epoch.current;
    const wasPinned = pinned.current;
    pinned.current = true;
    const pin = ++pins.current;
    try {
      const page = await jobLogs(jobId, from.afterId, count);
      // Another run was dropped meanwhile: these lines no longer join the window; ask again.
      if (mine !== epoch.current || runs[runs.length - 1] !== newest) {
        return;
      }
      runs.splice(runs.length - take, take);
      const first = lines.current[0]?.id ?? Infinity;
      lines.current = [...page.filter((l) => l.id < first), ...lines.current];
      cap.current += count;
      publish();
      setError(null);
    } catch (e) {
      if (mine === epoch.current) {
        // Unless loadMore or another loadEarlier changed it meanwhile, undo the pin: with no
        // earlier lines shown, polls follow new lines again.
        if (pins.current === pin) {
          pinned.current = wasPinned;
          if (!wasPinned) {
            setPaused(false);
          }
        }
        setError(e);
      }
    }
  }, [jobId, publish]);

  useEffect(() => {
    epoch.current++;
    after.current = 0;
    lines.current = [];
    dropped.current = [];
    cap.current = MAX_LOG_LINES;
    pinned.current = false;
    setLogs([]);
    setHidden(0);
    setMore(false);
    setPaused(false);
    setError(null);
    void pull();
    return () => {
      epoch.current++;
    };
  }, [pull]);

  useEffect(() => {
    if (!live) {
      return;
    }
    const t = setInterval(poll, POLL_MS);
    return () => clearInterval(t);
  }, [live, poll]);

  const wasLive = useRef(live);
  useEffect(() => {
    if (wasLive.current && !live) {
      poll();
    }
    wasLive.current = live;
  }, [live, poll]);

  return { logs, hidden, more, paused, error, loadMore, loadEarlier };
}

const LEVEL_CLASS: Record<LogLevel, string> = {
  debug: 'text-ink-muted',
  info: 'text-info',
  warn: 'text-warn',
  error: 'text-danger',
};

/**
 * LogFields shows a line's fields as key=value. A jobId field names another job (a job's own id is
 * not among its fields), such as a destination's retention job the global one queued: it links
 * to it.
 */
function LogFields({ fields }: { fields: Record<string, unknown> }) {
  return (
    <span className="ml-2 text-ink-muted">
      {Object.entries(fields).map(([k, v], i) => {
        const text = `${k}=${typeof v === 'string' ? v : JSON.stringify(v)}`;
        return (
          <Fragment key={k}>
            {i > 0 && ' '}
            {k === 'jobId' && typeof v === 'number' ? (
              <Link to={`/activity/jobs/${v}`} className="text-accent hover:underline">
                {text}
              </Link>
            ) : (
              text
            )}
          </Fragment>
        );
      })}
    </span>
  );
}

function timeOf(at: string): string {
  const d = new Date(at);
  return Number.isNaN(d.getTime()) ? at : d.toLocaleTimeString();
}

/** JobLogs shows a job's log lines, following new lines while the job runs. */
export function JobLogs({ jobId, live }: { jobId: number; live: boolean }) {
  const { logs, hidden, more, paused, error, loadMore, loadEarlier } = useJobLogs(jobId, live);
  const [loadingEarlier, setLoadingEarlier] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  const follow = useRef(true);

  useLayoutEffect(() => {
    const el = box.current;
    if (el && follow.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [logs]);

  return (
    <div>
      <ErrorNotice error={error} />
      {hidden > 0 && (
        <div className="mb-2 flex flex-wrap items-center gap-2 text-xs text-ink-muted">
          <span>{formatNumber(hidden)} earlier lines are not shown.</span>
          <Button
            small
            busy={loadingEarlier}
            onClick={() => {
              setLoadingEarlier(true);
              void loadEarlier().finally(() => setLoadingEarlier(false));
            }}
          >
            Show earlier lines
          </Button>
        </div>
      )}
      <div
        ref={box}
        role="log"
        aria-label="Job log"
        aria-live="off"
        tabIndex={0}
        onScroll={(e) => {
          const el = e.currentTarget;
          follow.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
        }}
        className="max-h-[28rem] overflow-y-auto rounded border border-line bg-page p-2 font-mono text-xs"
      >
        {logs.length === 0 ? (
          <p className="p-2 text-ink-muted">{live ? 'Waiting for log lines…' : 'No log lines.'}</p>
        ) : (
          logs.map((l) => (
            <div key={l.id} className="flex gap-2 py-0.5">
              <span className="shrink-0 text-ink-muted" title={l.at}>
                {timeOf(l.at)}
              </span>
              <span className={`w-10 shrink-0 uppercase ${LEVEL_CLASS[l.level] ?? ''}`}>{l.level}</span>
              <span className="min-w-0 break-words">
                {l.message}
                {l.fields && Object.keys(l.fields).length > 0 && <LogFields fields={l.fields} />}
              </span>
            </div>
          ))
        )}
      </div>
      {paused ? (
        <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-ink-muted">
          <span>New lines are paused while earlier lines are shown.</span>
          <Button small onClick={() => void loadMore()}>
            Show new lines
          </Button>
        </div>
      ) : (
        more && (
          <Button small className="mt-2" onClick={() => void loadMore()}>
            Load more
          </Button>
        )
      )}
    </div>
  );
}
