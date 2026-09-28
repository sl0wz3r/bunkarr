// Bandwidth limits, the timetable and the transfer window of a destination (docs/design/phase4.md
// §9.1, §9.2). validateBandwidth mirrors the server's checks (internal/engines/bwlimit Normalize)
// so the editor can show each problem next to its line, with the overlaps named per day.

import type { Bandwidth, BandwidthEntry, TransferWindow, Weekday } from '@/api/types';

/** DAYS are the day names in week order (Monday first), as the server stores them. */
export const DAYS: Weekday[] = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];

export const DAY_LABELS: Record<Weekday, string> = { mon: 'Mon', tue: 'Tue', wed: 'Wed', thu: 'Thu', fri: 'Fri', sat: 'Sat', sun: 'Sun' };

/** Limits of the server (bwlimit). */
export const MAX_TIMETABLE_ENTRIES = 16;
export const MAX_KIBPS = 10_000_000;
export const DEFAULT_GRACE_MINUTES = 15;
export const MAX_GRACE_MINUTES = 120;

const MINUTES_PER_DAY = 24 * 60;
const MINUTES_PER_WEEK = 7 * MINUTES_PER_DAY;

export const DEFAULT_BANDWIDTH: Bandwidth = { uploadKiBps: 0, downloadKiBps: 0, timetable: [], window: null };

/** DEFAULT_WINDOW is the window offered when one is turned on: every night 01:00–07:00. */
export const DEFAULT_WINDOW: TransferWindow = { days: [...DAYS], from: '01:00', to: '07:00', graceMinutes: DEFAULT_GRACE_MINUTES, allowOverrun: false };

/** DEFAULT_ENTRY is a new timetable line: weekdays 08:00–23:00 at 1 MiB/s up. */
export const DEFAULT_ENTRY: BandwidthEntry = { days: ['mon', 'tue', 'wed', 'thu', 'fri'], from: '08:00', to: '23:00', uploadKiBps: 1024, downloadKiBps: 0 };

/** parseHM parses "HH:MM" (00:00–23:59) into minutes after midnight, or null. */
export function parseHM(s: string): number | null {
  const m = /^(\d\d):(\d\d)$/.exec(s);
  if (!m) return null;
  const h = Number(m[1]);
  const min = Number(m[2]);
  return h > 23 || min > 59 ? null : h * 60 + min;
}

/** spanMinutes is the length of from–to in minutes; a `to` before `from` runs into the next day. */
export function spanMinutes(from: number, to: number): number {
  return (((to - from) % MINUTES_PER_DAY) + MINUTES_PER_DAY) % MINUTES_PER_DAY;
}

/** sortDays returns days in week order without repeats. */
export function sortDays(days: Weekday[]): Weekday[] {
  return DAYS.filter((d) => days.includes(d));
}

/** describeDays is "Every day", "Mon–Fri", "Sat, Sun" or "Mon, Wed, Fri". */
export function describeDays(days: Weekday[]): string {
  const sorted = sortDays(days);
  if (sorted.length === 7) return 'Every day';
  if (sorted.length === 0) return 'No day';
  const idx = sorted.map((d) => DAYS.indexOf(d));
  const run = idx.every((v, i) => i === 0 || v === idx[i - 1] + 1);
  if (run && sorted.length >= 3) return `${DAY_LABELS[sorted[0]]}–${DAY_LABELS[sorted[sorted.length - 1]]}`;
  return sorted.map((d) => DAY_LABELS[d]).join(', ');
}

/** describeWindow is "Every day 01:00–07:00" (plus "(runs into the next day)" when it crosses midnight). */
export function describeWindow(w: TransferWindow): string {
  const from = parseHM(w.from);
  const to = parseHM(w.to);
  const crosses = from != null && to != null && to < from;
  return `${describeDays(w.days)} ${w.from}–${w.to}${crosses ? ' (into the next day)' : ''}`;
}

/** formatKiBps renders a limit: "unlimited", "512 KiB/s", "2.0 MiB/s". */
export function formatKiBps(kib: number): string {
  if (!kib || kib <= 0) return 'unlimited';
  if (kib < 1024) return `${kib} KiB/s`;
  const mib = kib / 1024;
  return mib < 1024 ? `${mib.toFixed(mib >= 100 ? 0 : 1)} MiB/s` : `${(mib / 1024).toFixed(1)} GiB/s`;
}

/** BandwidthProblems are the problems of a bandwidth value, by where the editor shows them. */
export interface BandwidthProblems {
  /** Base upload and download limits. */
  base: string[];
  /** Problems of each timetable line, by its index (overlaps are named per day). */
  entries: Record<number, string[]>;
  window: string[];
  /** The timetable as a whole (too many lines). */
  timetable: string[];
  /** The first problem as a sentence, or null when the value is valid. */
  first: string | null;
}

function checkRate(label: string, v: number): string | null {
  if (!Number.isInteger(v) || v < 0 || v > MAX_KIBPS) {
    return `${label} must be a whole number from 0 to ${MAX_KIBPS.toLocaleString('en-US')} KiB/s (0 = unlimited).`;
  }
  return null;
}

function checkDays(days: Weekday[]): string | null {
  if (!days || days.length === 0) return 'Choose at least one day.';
  if (new Set(days).size !== days.length || days.some((d) => !DAYS.includes(d))) return 'Each day may be chosen once.';
  return null;
}

function checkSpan(from: string, to: string): string[] {
  const out: string[] = [];
  const f = parseHM(from);
  const t = parseHM(to);
  if (f == null) out.push(`"${from}" is not a time HH:MM (00:00–23:59).`);
  if (t == null) out.push(`"${to}" is not a time HH:MM (00:00–23:59).`);
  if (f != null && t != null && f === t) out.push('"From" and "to" must differ.');
  return out;
}

/**
 * validateBandwidth checks a bandwidth value as the server does: limits 0–10^7 KiB/s, at most 16
 * timetable lines, days and HH:MM times, from ≠ to, a grace of 0–120 minutes, and no two lines
 * covering the same moment of the week. A line that runs past midnight (23:00–06:00) covers the
 * next morning, so it can overlap the next day's line; each overlap is reported on the later line,
 * per day ("Tue: overlaps line 1 (Mon 23:00–06:00)").
 */
export function validateBandwidth(b: Bandwidth): BandwidthProblems {
  const p: BandwidthProblems = { base: [], entries: {}, window: [], timetable: [], first: null };
  const add = (list: string[], msg: string | null) => {
    if (msg) list.push(msg);
  };
  add(p.base, checkRate('The upload limit', b.uploadKiBps));
  add(p.base, checkRate('The download limit', b.downloadKiBps));
  const timetable = b.timetable ?? [];
  if (timetable.length > MAX_TIMETABLE_ENTRIES) {
    p.timetable.push(`At most ${MAX_TIMETABLE_ENTRIES} timetable lines.`);
  }
  type Interval = { entry: number; day: Weekday; start: number; end: number };
  const intervals: Interval[] = [];
  timetable.forEach((e, i) => {
    const list: string[] = [];
    add(list, checkDays(e.days));
    list.push(...checkSpan(e.from, e.to));
    add(list, checkRate('The upload limit', e.uploadKiBps));
    add(list, checkRate('The download limit', e.downloadKiBps));
    if (list.length > 0) {
      p.entries[i] = list;
      return;
    }
    const from = parseHM(e.from)!;
    const to = parseHM(e.to)!;
    for (const d of sortDays(e.days)) {
      const start = DAYS.indexOf(d) * MINUTES_PER_DAY + from;
      intervals.push({ entry: i, day: d, start, end: start + spanMinutes(from, to) });
    }
  });
  for (let a = 0; a < intervals.length; a++) {
    for (let c = a + 1; c < intervals.length; c++) {
      const x = intervals[a];
      const y = intervals[c];
      if (x.entry === y.entry) continue;
      const overlaps = [-MINUTES_PER_WEEK, 0, MINUTES_PER_WEEK].some((shift) => x.start < y.end + shift && y.start + shift < x.end);
      if (!overlaps) continue;
      const [first, later] = x.entry < y.entry ? [x, y] : [y, x];
      const e = timetable[first.entry];
      const msg = `${DAY_LABELS[later.day]}: overlaps line ${first.entry + 1} (${DAY_LABELS[first.day]} ${e.from}–${e.to}).`;
      const list = (p.entries[later.entry] ??= []);
      if (!list.includes(msg)) list.push(msg);
    }
  }
  if (b.window) {
    const w = b.window;
    add(p.window, checkDays(w.days));
    p.window.push(...checkSpan(w.from, w.to));
    if (!Number.isInteger(w.graceMinutes) || w.graceMinutes < 0 || w.graceMinutes > MAX_GRACE_MINUTES) {
      p.window.push(`The grace must be 0 to ${MAX_GRACE_MINUTES} minutes.`);
    }
  }
  const lines = Object.keys(p.entries)
    .map(Number)
    .sort((a, c) => a - c);
  p.first =
    p.base[0] ??
    p.timetable[0] ??
    (lines.length > 0 ? `Timetable line ${lines[0] + 1}: ${p.entries[lines[0]][0]}` : undefined) ??
    (p.window[0] ? `Transfer window: ${p.window[0]}` : null);
  return p;
}

/** normalizeBandwidth returns b as the server stores it: days in week order, [] for no timetable. */
export function normalizeBandwidth(b: Bandwidth): Bandwidth {
  return {
    uploadKiBps: b.uploadKiBps,
    downloadKiBps: b.downloadKiBps,
    timetable: (b.timetable ?? []).map((e) => ({ ...e, days: sortDays(e.days) })),
    window: b.window ? { ...b.window, days: sortDays(b.window.days) } : null,
  };
}

/** bandwidthOf fills a destination's bandwidth from the API (older servers omit it). */
export function bandwidthOf(b: Partial<Bandwidth> | null | undefined): Bandwidth {
  return {
    uploadKiBps: b?.uploadKiBps ?? 0,
    downloadKiBps: b?.downloadKiBps ?? 0,
    timetable: b?.timetable ?? [],
    window: b?.window ?? null,
  };
}

/**
 * formatClock renders a moment for "until 01:00": the time alone when it is within the next 24
 * hours of now, else with the weekday ("Tue 01:00"), in the browser's time zone.
 */
export function formatClock(value: string | null | undefined, now: number = Date.now()): string {
  if (!value) return '—';
  const t = Date.parse(value);
  if (Number.isNaN(t)) return '—';
  const d = new Date(t);
  const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
  if (t - now < MINUTES_PER_DAY * 60_000 && t - now > -MINUTES_PER_DAY * 60_000) return hm;
  return `${d.toLocaleDateString('en-US', { weekday: 'short' })} ${hm}`;
}

/** QUEUED_POLLS is how many times a just-queued job is re-read every second before the pace slows. */
export const QUEUED_POLLS = 15;

/** QUEUED_SLOW_MS is the pace after that: a busy queue (no free worker or upload slot) can hold the job for long. */
export const QUEUED_SLOW_MS = 10_000;

/**
 * queuedPollDelay is how long to wait before re-reading a just-queued job of a destination with a
 * transfer window, or false once it is known: it runs (or ended) or has deferred (notBefore set).
 * The queue answers before the runner looks at the window, and the runner starts only when a worker
 * and an upload slot are free, so the job is read every second at first and then every
 * QUEUED_SLOW_MS for as long as it stays queued: the notice never stops at "queued" when it defers
 * late.
 */
export function queuedPollDelay(job: { status: string; notBefore?: string | null } | undefined, reads: number): number | false {
  if (job && (job.status !== 'queued' || job.notBefore)) return false;
  return reads < QUEUED_POLLS ? 1000 : QUEUED_SLOW_MS;
}
