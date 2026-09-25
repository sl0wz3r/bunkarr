// Display formatting shared by every page: sizes (IEC units, like the *arr apps), durations,
// throughput, relative and absolute times, counts.

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB'];
const DASH = '—';

/** formatBytes renders a byte count with IEC units (1 KiB = 1024 B): "512 B", "1.5 GiB". */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes == null || !Number.isFinite(bytes)) {
    return DASH;
  }
  const sign = bytes < 0 ? '-' : '';
  let v = Math.abs(bytes);
  let unit = 0;
  while (v >= 1024 && unit < BYTE_UNITS.length - 1) {
    v /= 1024;
    unit++;
  }
  const digits = unit === 0 || v >= 100 ? 0 : 1;
  return `${sign}${v.toFixed(digits)} ${BYTE_UNITS[unit]}`;
}

/** formatRate renders a throughput in bytes per second, or a dash when there is none yet. */
export function formatRate(bytesPerSec: number | null | undefined): string {
  if (!bytesPerSec || bytesPerSec <= 0 || !Number.isFinite(bytesPerSec)) {
    return DASH;
  }
  return `${formatBytes(bytesPerSec)}/s`;
}

/** formatDuration renders seconds compactly: "45s", "3m 12s", "1h 2m", "2d 3h". */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null || !Number.isFinite(seconds) || seconds < 0) {
    return DASH;
  }
  const s = Math.round(seconds);
  if (s < 60) {
    return `${s}s`;
  }
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) {
    return `${d}d ${h}h`;
  }
  if (h > 0) {
    return `${h}h ${m}m`;
  }
  return `${m}m ${s % 60}s`;
}

/** formatDurationMs is formatDuration for milliseconds; sub-second values keep their unit. */
export function formatDurationMs(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) {
    return DASH;
  }
  if (ms < 1000) {
    return `${Math.round(ms)} ms`;
  }
  return formatDuration(ms / 1000);
}

/** formatEta renders the remaining seconds of a running job; 0 or less means unknown. */
export function formatEta(seconds: number | null | undefined): string {
  if (!seconds || seconds <= 0) {
    return DASH;
  }
  return formatDuration(seconds);
}

function toTime(value: string | null | undefined): number | null {
  if (!value) {
    return null;
  }
  const t = Date.parse(value);
  return Number.isNaN(t) ? null : t;
}

const relative = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

/** formatRelative renders an RFC 3339 time relative to now: "5 minutes ago", "in 3 hours". */
export function formatRelative(value: string | null | undefined, now: number = Date.now()): string {
  const t = toTime(value);
  if (t == null) {
    return DASH;
  }
  const diff = (t - now) / 1000;
  const abs = Math.abs(diff);
  if (abs < 45) {
    return diff <= 0 ? 'just now' : 'in a few seconds';
  }
  const steps: [number, Intl.RelativeTimeFormatUnit][] = [
    [3600, 'minute'],
    [86400, 'hour'],
    [86400 * 30, 'day'],
    [86400 * 365, 'month'],
  ];
  const divisors: Record<string, number> = { minute: 60, hour: 3600, day: 86400, month: 86400 * 30, year: 86400 * 365 };
  let unit: Intl.RelativeTimeFormatUnit = 'year';
  for (const [limit, u] of steps) {
    if (abs < limit) {
      unit = u;
      break;
    }
  }
  return relative.format(Math.round(diff / divisors[unit]), unit);
}

/** formatDateTime renders an RFC 3339 time in the browser's locale and time zone. */
export function formatDateTime(value: string | null | undefined): string {
  const t = toTime(value);
  return t == null ? DASH : new Date(t).toLocaleString();
}

const numbers = new Intl.NumberFormat('en-US');

/** formatNumber renders a count with thousands separators. */
export function formatNumber(n: number | null | undefined): string {
  return n == null || !Number.isFinite(n) ? DASH : numbers.format(n);
}

/** formatPercent renders a 0..1 fraction as a percentage ("42%", "0.5%"). */
export function formatPercent(fraction: number): string {
  const p = fraction * 100;
  return `${p > 0 && p < 10 ? p.toFixed(1) : Math.floor(p)}%`;
}

/** secondsBetween returns the seconds from start to end (or now), or null when start is unset. */
export function secondsBetween(start: string | null | undefined, end: string | null | undefined, now: number = Date.now()): number | null {
  const s = toTime(start);
  if (s == null) {
    return null;
  }
  const e = toTime(end) ?? now;
  return Math.max(0, (e - s) / 1000);
}
