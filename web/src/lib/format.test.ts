import { describe, expect, it } from 'vitest';
import { formatBytes, formatDuration, formatDurationMs, formatEta, formatPercent, formatRate, formatRelative, secondsBetween } from './format';

describe('formatBytes (IEC)', () => {
  it.each([
    [0, '0 B'],
    [512, '512 B'],
    [1023, '1023 B'],
    [1024, '1.0 KiB'],
    [1536, '1.5 KiB'],
    [1024 * 1024 * 150, '150 MiB'],
    [1024 ** 3 * 1.25, '1.3 GiB'],
    [1024 ** 4 * 3, '3.0 TiB'],
    [-2048, '-2.0 KiB'],
    [Number.NaN, '—'],
    [null, '—'],
  ])('%s → %s', (n, want) => {
    expect(formatBytes(n)).toBe(want);
  });
});

describe('formatDuration', () => {
  it.each([
    [0, '0s'],
    [45, '45s'],
    [192, '3m 12s'],
    [3725, '1h 2m'],
    [86400 * 2 + 3600 * 3, '2d 3h'],
    [-1, '—'],
    [null, '—'],
  ])('%s s → %s', (s, want) => {
    expect(formatDuration(s)).toBe(want);
  });

  it('keeps milliseconds below a second', () => {
    expect(formatDurationMs(250)).toBe('250 ms');
    expect(formatDurationMs(65_000)).toBe('1m 5s');
  });

  it('shows no ETA when it is unknown', () => {
    expect(formatEta(0)).toBe('—');
    expect(formatEta(90)).toBe('1m 30s');
  });
});

describe('formatRate and formatPercent', () => {
  it('formats throughput', () => {
    expect(formatRate(50 * 1024 * 1024)).toBe('50.0 MiB/s');
    expect(formatRate(0)).toBe('—');
  });

  it.each([
    [0, '0%'],
    [0.005, '0.5%'],
    [0.25, '25%'],
    [0.999, '99%'],
    [1, '100%'],
  ])('%s → %s', (f, want) => {
    expect(formatPercent(f)).toBe(want);
  });
});

describe('formatRelative', () => {
  const now = Date.parse('2026-09-24T12:00:00Z');
  it.each([
    ['2026-09-24T11:59:50Z', 'just now'],
    ['2026-09-24T11:55:00Z', '5 minutes ago'],
    ['2026-09-24T15:00:00Z', 'in 3 hours'],
    ['2026-09-23T12:00:00Z', 'yesterday'],
    ['2026-09-10T12:00:00Z', '14 days ago'],
    [null, '—'],
    ['not a date', '—'],
  ])('%s → %s', (t, want) => {
    expect(formatRelative(t, now)).toBe(want);
  });

  it('measures durations between times', () => {
    expect(secondsBetween('2026-09-24T11:00:00Z', '2026-09-24T11:01:30Z')).toBe(90);
    expect(secondsBetween(null, null)).toBeNull();
    expect(secondsBetween('2026-09-24T11:59:00Z', null, now)).toBe(60);
  });
});
