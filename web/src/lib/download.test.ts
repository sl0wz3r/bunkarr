import { afterEach, describe, expect, it, vi } from 'vitest';
import { kitFilename } from '@/api/destinations';
import { saveTextFile } from './download';

describe('saveTextFile', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('hands the text to a download and frees its URL', async () => {
    vi.useFakeTimers();
    const create = vi.fn((_: Blob) => 'blob:x');
    const revoke = vi.fn();
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: create, revokeObjectURL: revoke }));
    const clicked: { name: string; href: string; inDom: boolean }[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push({ name: this.download, href: this.getAttribute('href') ?? '', inDom: document.body.contains(this) });
    });
    expect(saveTextFile('kit.txt', 'secret text')).toBe(true);
    expect(clicked).toEqual([{ name: 'kit.txt', href: 'blob:x', inDom: true }]);
    expect(await create.mock.calls[0][0].text()).toBe('secret text');
    // The link is gone from the page, and the URL is freed right after.
    expect(document.querySelector('a[download]')).toBeNull();
    vi.runAllTimers();
    expect(revoke).toHaveBeenCalledWith('blob:x');
    vi.useRealTimers();
  });

  it('reports a browser without Blob URLs', () => {
    vi.stubGlobal('URL', {});
    expect(saveTextFile('kit.txt', 'x')).toBe(false);
  });
});

describe('kitFilename', () => {
  it.each([
    ['attachment; filename=bunkarr-recovery-b2-20260927.txt', 'bunkarr-recovery-b2-20260927.txt'],
    ['attachment; filename="bunkarr-recovery-my-nas-20260927.txt"', 'bunkarr-recovery-my-nas-20260927.txt'],
    ["attachment; filename*=utf-8''bunkarr-recovery-caf%C3%A9-20260927.txt", 'bunkarr-recovery-café-20260927.txt'],
    ['attachment; filename="../../etc/passwd"', 'passwd'],
    [null, 'bunkarr-recovery-kit.txt'],
    ['attachment', 'bunkarr-recovery-kit.txt'],
  ])('reads %j', (header, want) => {
    expect(kitFilename(header)).toBe(want);
  });
});
