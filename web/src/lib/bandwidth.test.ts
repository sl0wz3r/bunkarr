import { describe, expect, it } from 'vitest';
import type { Bandwidth, BandwidthEntry } from '@/api/types';
import { DEFAULT_BANDWIDTH, DEFAULT_WINDOW, describeDays, describeWindow, formatClock, formatKiBps, normalizeBandwidth, parseHM, QUEUED_POLLS, QUEUED_SLOW_MS, queuedPollDelay, spanMinutes, validateBandwidth } from './bandwidth';

const entry = (over: Partial<BandwidthEntry>): BandwidthEntry => ({ days: ['mon'], from: '08:00', to: '23:00', uploadKiBps: 1024, downloadKiBps: 0, ...over });
const withLines = (...timetable: BandwidthEntry[]): Bandwidth => ({ ...DEFAULT_BANDWIDTH, timetable });

describe('parseHM and spanMinutes', () => {
  it.each([
    ['00:00', 0],
    ['23:59', 1439],
    ['07:30', 450],
    ['24:00', null],
    ['7:30', null],
    ['12:60', null],
    ['', null],
  ])('parses %j', (s, want) => expect(parseHM(s)).toBe(want));

  it('crosses midnight when to is before from', () => {
    expect(spanMinutes(23 * 60, 6 * 60)).toBe(7 * 60);
    expect(spanMinutes(8 * 60, 23 * 60)).toBe(15 * 60);
  });
});

describe('validateBandwidth', () => {
  it('accepts the defaults and the design example', () => {
    expect(validateBandwidth(DEFAULT_BANDWIDTH).first).toBeNull();
    const ex: Bandwidth = {
      uploadKiBps: 0,
      downloadKiBps: 0,
      timetable: [entry({ days: ['mon', 'tue', 'wed', 'thu', 'fri'] })],
      window: DEFAULT_WINDOW,
    };
    expect(validateBandwidth(ex).first).toBeNull();
  });

  const cases: { name: string; value: Bandwidth; line?: number; want: string[] }[] = [
    {
      name: 'the same day and hours',
      value: withLines(entry({ days: ['mon', 'tue'] }), entry({ days: ['tue'], from: '22:00', to: '23:30' })),
      line: 1,
      want: ['Tue: overlaps line 1 (Tue 08:00–23:00).'],
    },
    {
      name: 'a night line running into the next morning',
      value: withLines(entry({ days: ['mon'], from: '23:00', to: '06:00' }), entry({ days: ['tue'], from: '05:00', to: '08:00' })),
      line: 1,
      want: ['Tue: overlaps line 1 (Mon 23:00–06:00).'],
    },
    {
      name: 'Sunday night into Monday (the week wraps)',
      value: withLines(entry({ days: ['sun'], from: '22:00', to: '02:00' }), entry({ days: ['mon'], from: '01:00', to: '03:00' })),
      line: 1,
      want: ['Mon: overlaps line 1 (Sun 22:00–02:00).'],
    },
    {
      name: 'every overlapping day, once each',
      value: withLines(entry({ days: ['mon', 'wed', 'fri'] }), entry({ days: ['mon', 'tue', 'wed'], from: '09:00', to: '10:00' })),
      line: 1,
      want: ['Mon: overlaps line 1 (Mon 08:00–23:00).', 'Wed: overlaps line 1 (Wed 08:00–23:00).'],
    },
  ];
  it.each(cases)('reports an overlap: $name', ({ value, line, want }) => {
    const p = validateBandwidth(value);
    expect(p.entries[line!]).toEqual(want);
    expect(p.entries[0]).toBeUndefined();
    expect(p.first).toBe(`Timetable line ${line! + 1}: ${want[0]}`);
  });

  it('allows lines that only touch', () => {
    expect(validateBandwidth(withLines(entry({ from: '08:00', to: '12:00' }), entry({ from: '12:00', to: '18:00' }))).first).toBeNull();
    expect(validateBandwidth(withLines(entry({ days: ['mon'], from: '23:00', to: '06:00' }), entry({ days: ['tue'], from: '06:00', to: '08:00' }))).first).toBeNull();
  });

  it.each([
    [entry({ days: [] }), 'Choose at least one day.'],
    [entry({ from: '8:00' }), '"8:00" is not a time HH:MM (00:00–23:59).'],
    [entry({ to: '08:00' }), '"From" and "to" must differ.'],
    [entry({ uploadKiBps: -1 }), 'The upload limit must be a whole number from 0 to 10,000,000 KiB/s (0 = unlimited).'],
    [entry({ downloadKiBps: 1.5 }), 'The download limit must be a whole number from 0 to 10,000,000 KiB/s (0 = unlimited).'],
  ])('refuses a bad line %#', (e, msg) => {
    expect(validateBandwidth(withLines(e)).entries[0]).toContain(msg);
  });

  it('refuses bad base limits, too many lines and a bad window', () => {
    expect(validateBandwidth({ ...DEFAULT_BANDWIDTH, uploadKiBps: Number.NaN }).base).toHaveLength(1);
    const many = Array.from({ length: 17 }, (_, i) => entry({ days: [(['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'] as const)[i % 7]], from: `0${i % 7}:00`.slice(-5), to: '23:59' }));
    expect(validateBandwidth(withLines(...many)).timetable).toEqual(['At most 16 timetable lines.']);
    const w = validateBandwidth({ ...DEFAULT_BANDWIDTH, window: { ...DEFAULT_WINDOW, days: [], graceMinutes: 121 } });
    expect(w.window).toEqual(['Choose at least one day.', 'The grace must be 0 to 120 minutes.']);
    expect(w.first).toBe('Transfer window: Choose at least one day.');
  });
});

describe('describing', () => {
  it('names days and windows', () => {
    expect(describeDays(['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'])).toBe('Every day');
    expect(describeDays(['fri', 'mon', 'wed', 'tue', 'thu'])).toBe('Mon–Fri');
    expect(describeDays(['sat', 'sun'])).toBe('Sat, Sun');
    expect(describeWindow(DEFAULT_WINDOW)).toBe('Every day 01:00–07:00');
    expect(describeWindow({ ...DEFAULT_WINDOW, from: '23:00', to: '06:00', days: ['sat'] })).toBe('Sat 23:00–06:00 (into the next day)');
  });

  it('formats limits and times', () => {
    expect(formatKiBps(0)).toBe('unlimited');
    expect(formatKiBps(512)).toBe('512 KiB/s');
    expect(formatKiBps(2048)).toBe('2.0 MiB/s');
    const now = new Date(2026, 8, 27, 20, 0).getTime();
    expect(formatClock(new Date(2026, 8, 28, 1, 0).toISOString(), now)).toBe('01:00');
    expect(formatClock(new Date(2026, 8, 30, 1, 0).toISOString(), now)).toBe('Wed 01:00');
    expect(formatClock(null, now)).toBe('—');
  });

  it('normalizes days into week order', () => {
    const n = normalizeBandwidth({ ...DEFAULT_BANDWIDTH, timetable: [entry({ days: ['fri', 'mon'] })], window: { ...DEFAULT_WINDOW, days: ['sun', 'sat'] } });
    expect(n.timetable![0].days).toEqual(['mon', 'fri']);
    expect(n.window!.days).toEqual(['sat', 'sun']);
    expect(normalizeBandwidth({ ...DEFAULT_BANDWIDTH, timetable: null }).timetable).toEqual([]);
  });
});

describe('queuedPollDelay', () => {
  it('keeps reading a queued job until it runs or defers, slower once the first reads found it still queued', () => {
    const queued = { status: 'queued', notBefore: null };
    expect(queuedPollDelay(undefined, 0)).toBe(1000);
    expect(queuedPollDelay(queued, 1)).toBe(1000);
    // A busy queue (both workers or upload slots taken) holds it past the fast reads: keep reading.
    expect(queuedPollDelay(queued, QUEUED_POLLS)).toBe(QUEUED_SLOW_MS);
    expect(queuedPollDelay(queued, QUEUED_POLLS * 100)).toBe(QUEUED_SLOW_MS);
    expect(queuedPollDelay({ status: 'queued', notBefore: '2026-09-28T01:00:00Z' }, QUEUED_POLLS + 3)).toBe(false);
    expect(queuedPollDelay({ status: 'running', notBefore: null }, 2)).toBe(false);
    expect(queuedPollDelay({ status: 'completed', notBefore: null }, QUEUED_POLLS + 3)).toBe(false);
  });
});
