import { render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ErrorNotice, Notice } from './Notice';

// Messages at the top of a long dialog must scroll into view when they appear: the user clicked
// Save or Test in the dialog's footer and would otherwise see nothing happen.
describe('Notice reveal', () => {
  const scroll = vi.fn();
  beforeEach(() => {
    scroll.mockClear();
    // jsdom has no scrollIntoView.
    Element.prototype.scrollIntoView = scroll;
  });
  afterEach(() => {
    delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
  });

  it('scrolls an error into view when it appears and when its text changes', () => {
    const { rerender } = render(<Notice tone="error">Enter a name.</Notice>);
    expect(scroll).toHaveBeenCalledTimes(1);
    expect(scroll).toHaveBeenCalledWith({ block: 'nearest', behavior: 'smooth' });
    rerender(<Notice tone="error">Enter a name.</Notice>);
    expect(scroll).toHaveBeenCalledTimes(1);
    rerender(<Notice tone="error">Enter a path.</Notice>);
    expect(scroll).toHaveBeenCalledTimes(2);
  });

  it('leaves other notices where they are unless asked, then follows revealKey', () => {
    const { rerender } = render(<Notice tone="info">Preview</Notice>);
    expect(scroll).not.toHaveBeenCalled();
    const first = { ok: true };
    rerender(
      <Notice tone="success" reveal revealKey={first}>
        OK
      </Notice>,
    );
    expect(scroll).toHaveBeenCalledTimes(1);
    rerender(
      <Notice tone="success" reveal revealKey={{ ok: true }}>
        OK
      </Notice>,
    );
    expect(scroll).toHaveBeenCalledTimes(2);
  });

  it('reveals a server error', () => {
    render(<ErrorNotice error={new Error('target is required')} />);
    expect(scroll).toHaveBeenCalledTimes(1);
  });

  it('works without scrollIntoView (older browsers, jsdom)', () => {
    delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
    expect(() => render(<Notice tone="error">Boom</Notice>)).not.toThrow();
  });
});
