// Cron helpers for schedule inputs. The server (robfig/cron, standard 5-field parser) is the
// authority; validateCron applies its rules exactly, so the forms accept what it accepts. This
// module also describes common expressions in words and finds the hours a schedule fires at (for
// the Plex butler-window warning).

/** CronPreset is a named schedule offered by CronInput. */
export interface CronPreset {
  label: string;
  cron: string;
}

/** SCHEDULE_PRESETS are offered for destination sync and verify schedules. */
export const SCHEDULE_PRESETS: CronPreset[] = [
  { label: 'Nightly at 02:00', cron: '0 2 * * *' },
  { label: 'Every 6 hours', cron: '0 */6 * * *' },
  { label: 'Weekly (Sunday 03:00)', cron: '0 3 * * 0' },
  { label: 'Weekly (Sunday 05:00)', cron: '0 5 * * 0' },
];

/** PLEX_BACKUP_PRESETS keep the Plex DB backup outside Plex's default butler window (02–05). */
export const PLEX_BACKUP_PRESETS: CronPreset[] = [
  { label: 'Daily at 06:00', cron: '0 6 * * *' },
  { label: 'Every 12 hours (06:00, 18:00)', cron: '0 6,18 * * *' },
  { label: 'Weekly (Sunday 06:00)', cron: '0 6 * * 0' },
];

/** TASK_PRESETS are offered when editing any schedule on System → Tasks. */
export const TASK_PRESETS: CronPreset[] = [
  { label: 'Every hour', cron: '0 * * * *' },
  { label: 'Every 6 hours', cron: '0 */6 * * *' },
  { label: 'Nightly at 02:00', cron: '0 2 * * *' },
  { label: 'Daily at 04:30', cron: '30 4 * * *' },
  { label: 'Daily at 06:00', cron: '0 6 * * *' },
  { label: 'Weekly (Sunday 03:00)', cron: '0 3 * * 0' },
  { label: 'Weekly (Sunday 05:00)', cron: '0 5 * * 0' },
];

/**
 * Defaults used by new destinations and Plex servers (design §4.5, §5). The verify and Plex
 * backup defaults are the server's (internal/api DefaultVerifyCron, DefaultPlexBackupCron).
 */
export const DEFAULT_SYNC_CRON = '0 2 * * *';
export const DEFAULT_VERIFY_CRON = '0 5 * * 0';
export const DEFAULT_PLEX_BACKUP_CRON = '0 6 * * *';

const MONTHS = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'];
const WEEKDAYS = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'];
const WEEKDAY_NAMES = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

// The parser below is internal/jobqueue parseCron with robfig/cron's getField, getRange and
// dayMatches; web/src/lib/cron-cases.json is checked against both validators.

// White space as Go's unicode.IsSpace sees it (strings.TrimSpace, strings.Fields). JavaScript's
// \s and trim() differ: they include U+FEFF and leave out U+0085.
const SPACE = '\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000';
const TRIM = new RegExp(`^[${SPACE}]+|[${SPACE}]+$`, 'g');
const SEPARATOR = new RegExp(`[${SPACE}]+`);

function trimSpace(s: string): string {
  return s.replace(TRIM, '');
}

function fields(s: string): string[] {
  const t = trimSpace(s);
  return t === '' ? [] : t.split(SEPARATOR);
}

interface FieldSpec {
  name: string;
  min: number;
  max: number;
  names?: string[];
  nameBase?: number;
}

const FIELDS: FieldSpec[] = [
  { name: 'minute', min: 0, max: 59 },
  { name: 'hour', min: 0, max: 23 },
  { name: 'day of month', min: 1, max: 31 },
  { name: 'month', min: 1, max: 12, names: MONTHS, nameBase: 1 },
  { name: 'day of week', min: 0, max: 6, names: WEEKDAYS, nameBase: 0 },
];

/** The only descriptors the server accepts (exact case), as the 5 fields they stand for. */
const DESCRIPTORS: Record<string, string> = {
  '@hourly': '0 * * * *',
  '@daily': '0 0 * * *',
  '@weekly': '0 0 * * 0',
  '@monthly': '0 0 1 * *',
};

// The longest days of each month (1-12); 29 February comes within the 5 years robfig searches.
const MONTH_DAYS = [0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];

const MAX_INT64 = 9223372036854775807n;

// atoi is strconv.Atoi limited to what robfig accepts: an optional sign, decimal digits, a value
// from 0 to the largest int64. Values past the safe integer range only matter as "too large".
function atoi(token: string): number | null {
  if (!/^[+-]?\d+$/.test(token)) {
    return null;
  }
  const n = BigInt(token);
  if (n < 0n || n > MAX_INT64) {
    return null;
  }
  return n > BigInt(Number.MAX_SAFE_INTEGER) ? Number.MAX_SAFE_INTEGER : Number(n);
}

// lowerName is Go's strings.ToLower as far as it can produce a month or weekday name: ASCII
// letters, and the two non-ASCII letters that lower-case to ASCII ones (U+0130 İ, U+212A Kelvin).
function lowerName(token: string): string {
  return token.replace(/[A-Z\u0130\u212a]/g, (c) => (c === '\u0130' ? 'i' : c === '\u212a' ? 'k' : c.toLowerCase()));
}

function parseValue(token: string, spec: FieldSpec): number | null {
  const idx = spec.names?.indexOf(lowerName(token)) ?? -1;
  return idx >= 0 ? idx + (spec.nameBase ?? 0) : atoi(token);
}

/** A parsed field: the values it matches and whether it was a star (robfig's star bit). */
interface ParsedField {
  values: number[];
  star: boolean;
}

// parseField is robfig/cron's getField and getRange: a comma-separated list (empty items
// skipped) of "*" or "?" or "a" or "a-b", each with an optional "/step". It returns the values
// the field matches, or an error message.
function parseField(field: string, spec: FieldSpec): ParsedField | string {
  const out = new Set<number>();
  let star = false;
  for (const part of field.split(',')) {
    if (part === '') {
      continue;
    }
    const rangeAndStep = part.split('/');
    const lowAndHigh = rangeAndStep[0].split('-');
    let lo: number;
    let hi: number;
    let partStar = false;
    if (lowAndHigh[0] === '*' || lowAndHigh[0] === '?') {
      lo = spec.min;
      hi = spec.max;
      partStar = true;
    } else {
      const a = parseValue(lowAndHigh[0], spec);
      if (a == null || lowAndHigh.length > 2) {
        return `invalid ${spec.name} "${part}"`;
      }
      const b = lowAndHigh.length === 2 ? parseValue(lowAndHigh[1], spec) : a;
      if (b == null) {
        return `invalid ${spec.name} "${part}"`;
      }
      lo = a;
      hi = b;
    }
    let step = 1;
    if (rangeAndStep.length === 2) {
      const s = atoi(rangeAndStep[1]);
      if (s == null) {
        return `invalid step in ${spec.name} "${part}"`;
      }
      step = s;
      // "N/step" means "N-max/step".
      if (lowAndHigh.length === 1) {
        hi = spec.max;
      }
      if (step > 1) {
        partStar = false;
      }
    } else if (rangeAndStep.length > 2) {
      return `invalid ${spec.name} "${part}"`;
    }
    if (lo < spec.min || hi > spec.max || lo > hi) {
      return `${spec.name} must be between ${spec.min} and ${spec.max}`;
    }
    if (step === 0) {
      return `invalid step in ${spec.name} "${part}"`;
    }
    for (let v = lo; v <= hi; v += step) out.add(v);
    star = star || partStar;
  }
  return { values: [...out].sort((x, y) => x - y), star };
}

// fires reports whether a schedule ever runs (the server refuses one that does not). A day
// matches when both the day of month and the weekday do if either is a star, otherwise when
// either does (robfig's dayMatches). A field of only commas matches nothing.
function fires(f: ParsedField[]): boolean {
  const [minute, hour, dom, month, dow] = f;
  if (minute.values.length === 0 || hour.values.length === 0 || month.values.length === 0) {
    return false;
  }
  // Some day of month exists in one of the months (29 February comes within robfig's 5 years).
  const domFits = month.values.some((m) => dom.values.some((d) => d <= MONTH_DAYS[m]));
  // Every weekday comes in every month.
  const dowFits = dow.values.length > 0;
  if (dom.star || dow.star) {
    return domFits && dowFits;
  }
  return domFits || dowFits;
}

// parse returns the 5 parsed fields of expr, or an error message (the server's rules).
function parse(expr: string): ParsedField[] | string {
  const e = trimSpace(expr);
  if (!e) {
    return 'Enter a cron expression.';
  }
  if (e.startsWith('TZ=') || e.startsWith('CRON_TZ=')) {
    return "Time zone prefixes are not supported: schedules run in the server's time zone (TZ).";
  }
  let parts = fields(e);
  if (e.startsWith('@')) {
    const d = DESCRIPTORS[e];
    if (!d) {
      return `Unsupported descriptor ${e}: use @hourly, @daily, @weekly, @monthly or 5 fields.`;
    }
    parts = d.split(' ');
  }
  if (parts.length !== 5) {
    return 'A cron expression has 5 fields: minute hour day-of-month month day-of-week.';
  }
  const out: ParsedField[] = [];
  for (let i = 0; i < 5; i++) {
    const r = parseField(parts[i], FIELDS[i]);
    if (typeof r === 'string') {
      return `Invalid cron: ${r}.`;
    }
    out.push(r);
  }
  if (!fires(out)) {
    return 'This schedule never runs (no such date).';
  }
  return out;
}

// normalize returns the 5 fields of a valid expression (descriptors expanded).
function normalize(expr: string): string[] {
  const e = trimSpace(expr);
  return (DESCRIPTORS[e] ?? e).split(SEPARATOR);
}

/**
 * validateCron returns a message describing what is wrong with expr, or null when the server
 * accepts it (internal/jobqueue ValidateCron: 5 fields or @hourly, @daily, @weekly, @monthly).
 */
export function validateCron(expr: string): string | null {
  const r = parse(expr);
  return typeof r === 'string' ? r : null;
}

function pad(n: number): string {
  return String(n).padStart(2, '0');
}

/** describeCron renders common cron shapes in words and falls back to the expression itself. */
export function describeCron(expr: string): string {
  const e = trimSpace(expr);
  if (validateCron(e) !== null) {
    return e;
  }
  const [min, hour, dom, mon, dow] = normalize(e);
  const num = /^\d+$/;
  if (dom !== '*' || mon !== '*') {
    return e;
  }
  if (num.test(min) && num.test(hour)) {
    const at = `${pad(Number(hour))}:${pad(Number(min))}`;
    if (dow === '*') return `Daily at ${at}`;
    if (num.test(dow)) return `Weekly on ${WEEKDAY_NAMES[Number(dow)]} at ${at}`;
    return e;
  }
  if (dow !== '*') {
    return e;
  }
  if (num.test(min) && /^\*\/\d+$/.test(hour)) {
    return `Every ${hour.slice(2)} hours at minute ${Number(min)}`;
  }
  if (num.test(min) && /^\d+(,\d+)+$/.test(hour)) {
    return `Daily at ${hour
      .split(',')
      .map((h) => `${pad(Number(h))}:${pad(Number(min))}`)
      .join(', ')}`;
  }
  if (num.test(min) && hour === '*') {
    return Number(min) === 0 ? 'Every hour' : `Every hour at minute ${Number(min)}`;
  }
  if (/^\*\/\d+$/.test(min) && hour === '*') {
    return `Every ${min.slice(2)} minutes`;
  }
  return e;
}

/** cronHours returns the hours of the day a schedule fires at, or null when it cannot tell. */
export function cronHours(expr: string): number[] | null {
  const r = parse(expr);
  return typeof r === 'string' ? null : r[1].values;
}

/**
 * overlapsWindow reports whether a schedule fires within [startHour, endHour) (the window may
 * wrap past midnight). Unknown schedules do not overlap.
 */
export function overlapsWindow(expr: string, startHour: number, endHour: number): boolean {
  if (startHour === endHour) {
    return false;
  }
  const hours = cronHours(expr);
  if (!hours) {
    return false;
  }
  return hours.some((h) => (startHour < endHour ? h >= startHour && h < endHour : h >= startHour || h < endHour));
}
