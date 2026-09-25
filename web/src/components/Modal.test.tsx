import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { Modal } from './Modal';

// A page with a button that opens a dialog, and another button behind it.
function Page({ autoFocus }: { autoFocus: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      <button type="button">Behind</button>
      {open && (
        <Modal
          title="Dialog"
          onClose={() => setOpen(false)}
          footer={
            <button type="button" onClick={() => setOpen(false)}>
              Cancel
            </button>
          }
        >
          <input aria-label="Name" autoFocus={autoFocus} />
        </Modal>
      )}
    </>
  );
}

describe('Modal focus', () => {
  it.each([true, false])('returns focus to the button that opened it (autoFocus field: %s)', async (autoFocus) => {
    const user = userEvent.setup();
    render(<Page autoFocus={autoFocus} />);
    const open = screen.getByRole('button', { name: 'Open' });

    await user.click(open);
    expect(screen.getByRole('dialog', { name: 'Dialog' })).toContainElement(document.activeElement as HTMLElement);
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(open).toHaveFocus();

    await user.click(open);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(open).toHaveFocus();
  });

  it('keeps Tab and Shift+Tab inside the dialog when the dialog itself has focus', async () => {
    const user = userEvent.setup();
    render(<Page autoFocus={false} />);
    await user.click(screen.getByRole('button', { name: 'Open' }));
    const dialog = screen.getByRole('dialog', { name: 'Dialog' });
    expect(dialog).toHaveFocus();

    await user.keyboard('{Shift>}{Tab}{/Shift}');
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'Close' })).toHaveFocus();

    dialog.focus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'Close' })).toHaveFocus();
    await user.keyboard('{Shift>}{Tab}{/Shift}');
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    expect(screen.getByRole('button', { name: 'Behind' })).not.toHaveFocus();
  });
});

// A form dialog with a Browse button that opens a second dialog over it (as PathPicker's folder
// browser does inside the source, destination and Plex forms).
function NestedPage() {
  const [open, setOpen] = useState(false);
  const [inner, setInner] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      {open && (
        <Modal title="Outer" onClose={() => setOpen(false)} footer={<button type="button">Save</button>}>
          <button type="button" onClick={() => setInner(true)}>
            Browse
          </button>
          {inner && (
            <Modal
              title="Inner"
              onClose={() => setInner(false)}
              footer={
                <>
                  <button type="button" onClick={() => setInner(false)}>
                    Cancel
                  </button>
                  <button type="button">Select</button>
                </>
              }
            >
              <button type="button">Up</button>
            </Modal>
          )}
        </Modal>
      )}
    </>
  );
}

describe('Modal inside a Modal', () => {
  it('keeps Tab and Shift+Tab inside the inner dialog', async () => {
    const user = userEvent.setup();
    render(<NestedPage />);
    await user.click(screen.getByRole('button', { name: 'Open' }));
    await user.click(screen.getByRole('button', { name: 'Browse' }));
    const inner = screen.getByRole('dialog', { name: 'Inner' });
    const innerButton = (name: string) => within(inner).getByRole('button', { name });

    // From a middle item, Tab and Shift+Tab move to the neighbours.
    innerButton('Up').focus();
    await user.tab();
    expect(innerButton('Cancel')).toHaveFocus();
    innerButton('Up').focus();
    await user.keyboard('{Shift>}{Tab}{/Shift}');
    expect(innerButton('Close')).toHaveFocus();

    // At the ends, they wrap around within the inner dialog.
    await user.keyboard('{Shift>}{Tab}{/Shift}');
    expect(innerButton('Select')).toHaveFocus();
    await user.tab();
    expect(innerButton('Close')).toHaveFocus();

    // From the inner dialog itself.
    inner.focus();
    await user.tab();
    expect(innerButton('Close')).toHaveFocus();
    inner.focus();
    await user.keyboard('{Shift>}{Tab}{/Shift}');
    expect(innerButton('Select')).toHaveFocus();
  });
});
