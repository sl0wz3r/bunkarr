import { describe, expect, it } from 'vitest';
import { cronHours, describeCron, overlapsWindow, validateCron } from './cron';
import parity from './cron-cases.json';

describe('validateCron', () => {
  // The same list is checked against the server's validator (internal/api TestCronCasesMatchServer).
  it.each(parity.cases.map((c) => [c.expr, c.valid, c.why] as const))('%j valid=%s (%s), as on the server', (expr, valid) => {
    expect(validateCron(expr) === null).toBe(valid);
  });

  it.each(['0 2 * * *', '0 */6 * * *', '30 4 * * *', '0 3 * * 0', '0 6,18 * * *', '*/15 * * * *', '0 0 1 JAN *', '0 2 * * MON-FRI', '5/10 * ? * *', '@daily'])(
    'accepts %s',
    (expr) => {
      expect(validateCron(expr)).toBeNull();
    },
  );

  it.each([
    ['', 'Enter a cron expression.'],
    ['0 2 * *', 'A cron expression has 5 fields'],
    ['60 2 * * *', 'minute must be between 0 and 59'],
    ['0 24 * * *', 'hour must be between 0 and 23'],
    ['0 2 0 * *', 'day of month must be between 1 and 31'],
    ['0 2 * 13 *', 'month must be between 1 and 12'],
    ['0 2 * * 7', 'day of week must be between 0 and 6'],
    ['0 5-2 * * *', 'hour must be between 0 and 23'],
    ['0 */0 * * *', 'invalid step'],
    ['x 2 * * *', 'invalid minute'],
    ['@often', 'Unsupported descriptor'],
    ['@every 1h30m', 'Unsupported descriptor'],
    ['@yearly', 'Unsupported descriptor'],
    ['TZ=UTC 0 2 * * *', 'time zone'],
    ['0 0 30 2 *', 'never runs'],
  ])('rejects %j', (expr, want) => {
    expect(validateCron(expr)).toContain(want);
  });
});

describe('describeCron', () => {
  it.each([
    ['0 2 * * *', 'Daily at 02:00'],
    ['30 4 * * *', 'Daily at 04:30'],
    ['0 */6 * * *', 'Every 6 hours at minute 0'],
    ['0 3 * * 0', 'Weekly on Sunday at 03:00'],
    ['0 6,18 * * *', 'Daily at 06:00, 18:00'],
    ['0 * * * *', 'Every hour'],
    ['*/15 * * * *', 'Every 15 minutes'],
    ['@daily', 'Daily at 00:00'],
    ['@hourly', 'Every hour'],
    ['@every 2h', '@every 2h'],
    ['0 2 1 * *', '0 2 1 * *'],
  ])('%s → %s', (expr, want) => {
    expect(describeCron(expr)).toBe(want);
  });
});

describe('butler window overlap', () => {
  it('finds the hours a schedule fires at', () => {
    expect(cronHours('0 */6 * * *')).toEqual([0, 6, 12, 18]);
    expect(cronHours('0 1-3 * * *')).toEqual([1, 2, 3]);
    expect(cronHours('@every 1h')).toBeNull();
    expect(cronHours('@hourly')).toHaveLength(24);
    expect(cronHours('0 ? * * *')).toHaveLength(24);
  });

  it.each([
    ['0 3 * * *', 2, 5, true],
    ['0 5 * * *', 2, 5, false],
    ['0 6 * * *', 2, 5, false],
    ['0 */6 * * *', 2, 5, false],
    ['0 */4 * * *', 2, 5, true],
    ['0 1 * * *', 23, 3, true],
    ['0 12 * * *', 23, 3, false],
    ['0 3 * * *', 4, 4, false],
  ])('%s in %i–%i → %s', (expr, start, end, want) => {
    expect(overlapsWindow(expr, start, end)).toBe(want);
  });
});
