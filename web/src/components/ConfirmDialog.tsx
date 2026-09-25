import { useState, type ReactNode } from 'react';
import { Button } from './Button';
import { Modal } from './Modal';
import { ErrorNotice } from './Notice';

/**
 * ConfirmDialog asks before an action. onConfirm may be async: the dialog shows a spinner, keeps
 * itself open and shows the error if it throws, and closes when it succeeds.
 */
export function ConfirmDialog({
  title,
  children,
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  danger,
  onConfirm,
  onClose,
}: {
  title: string;
  children: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
  onConfirm: () => void | Promise<void>;
  onClose: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function confirm() {
    setBusy(true);
    setError(null);
    try {
      await onConfirm();
      onClose();
    } catch (e) {
      setError(e);
      setBusy(false);
    }
  }

  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <Button onClick={onClose}>{cancelLabel}</Button>
          <Button variant={danger ? 'danger' : 'primary'} busy={busy} onClick={() => void confirm()}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      <ErrorNotice error={error} />
      <div className="space-y-2 text-sm">{children}</div>
    </Modal>
  );
}
