import { useMutation, useQueryClient } from '@tanstack/react-query';
import { CheckCircle2, Download, ShieldCheck } from 'lucide-react';
import { useId, useState } from 'react';
import { ApiError, errorMessage } from '@/api/client';
import { confirmRecoveryKit, exportRecoveryKit } from '@/api/destinations';
import type { Destination } from '@/api/types';
import { Button } from '@/components/Button';
import { Checkbox, inputClass } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { isPasswordError, PasswordConfirm } from '@/components/PasswordConfirm';
import { formatDateTime } from '@/lib/format';
import { saveTextFile } from '@/lib/download';
import { kitState } from '@/lib/destinationKinds';
import { keys } from '@/lib/lookups';

// The recovery kit of an encrypted destination (docs/design/phase4.md §5.2, S21, §15 step 6): the
// kit is the only way to read the backup without this server's /config, so no job but a dry run
// runs until its custody is confirmed. For a generated secret: download the kit (the user's
// password in the same dialog), then type the check code printed in it. For a secret the user
// typed at create: type it again (or use the kit).

/** confirmErrorText explains a wrong confirmation. */
function confirmErrorText(e: unknown, bySecret: boolean): string {
  if (e instanceof ApiError && e.status === 400) {
    return bySecret
      ? 'That is not the encryption password this destination was created with.'
      : 'Wrong check code: type the 8 characters after "Check code" in the kit (dashes and case do not matter).';
  }
  if (e instanceof ApiError && e.status === 429) return `${e.message}. Wait, then try again.`;
  if (e instanceof ApiError && e.status === 403) return `${e.message}. Log in with your username and password to confirm.`;
  return errorMessage(e);
}

/** RecoveryKitPanel is the kit step: download, then confirm with the check code or the typed secret. */
export function RecoveryKitPanel({ destination, onConfirmed }: { destination: Destination; onConfirmed?: () => void }) {
  const qc = useQueryClient();
  const codeId = useId();
  const secretId = useId();
  const [password, setPassword] = useState('');
  const [includeCredentials, setIncludeCredentials] = useState(false);
  const [saved, setSaved] = useState<{ filename: string; text: string | null } | null>(null);
  const [code, setCode] = useState('');
  const [secret, setSecret] = useState('');
  const [confirmedNow, setConfirmedNow] = useState(false);
  const state = confirmedNow ? 'confirmed' : kitState(destination);
  const userSecret = destination.encryption.origin === 'user';

  const exporter = useMutation({
    mutationFn: () => exportRecoveryKit(destination.id, { currentPassword: password, includeStorageCredentials: includeCredentials }),
    onSuccess: async (kit) => {
      // Handed to the browser's download only; the text is shown here only when it cannot be.
      const ok = saveTextFile(kit.filename, kit.text);
      setSaved({ filename: kit.filename, text: ok ? null : kit.text });
      setPassword('');
      await qc.invalidateQueries({ queryKey: keys.destinations });
    },
  });
  const confirmer = useMutation({
    mutationFn: (body: { checkCode: string } | { secret: string }) => confirmRecoveryKit(destination.id, body),
    onSuccess: async () => {
      setConfirmedNow(true);
      setCode('');
      setSecret('');
      await qc.invalidateQueries({ queryKey: keys.destinations });
      await qc.invalidateQueries({ queryKey: keys.schedules });
      onConfirmed?.();
    },
  });
  const bySecret = !!confirmer.variables && 'secret' in confirmer.variables;

  return (
    <div>
      {state === 'confirmed' ? (
        <Notice tone="success" title="Recovery kit confirmed">
          {destination.encryption.kitConfirmedAt && !confirmedNow ? `Confirmed on ${formatDateTime(destination.encryption.kitConfirmedAt)}. ` : ''}
          Backups to {destination.name} can run. Keep the kit offline, away from this server; you can download it again at any time.
        </Notice>
      ) : (
        <Notice tone="error" title="Recovery kit not confirmed">
          No backup runs to {destination.name} until you {userSecret ? 'type your encryption password again, or download the kit and type its check code' : 'download its recovery kit and type the check code printed in it'}. Without
          the kit, losing this server&apos;s /config makes the backup unreadable.
        </Notice>
      )}

      {userSecret && state !== 'confirmed' && (
        <section aria-label="Type your password again" className="mb-5">
          <h3 className="mb-1 text-sm font-semibold">Type your password again</h3>
          <p className="mb-2 text-xs text-ink-muted">The encryption password you chose when you created {destination.name}: typing it again proves you have it.</p>
          <div className="flex flex-wrap items-center gap-2">
            <label htmlFor={secretId} className="sr-only">
              Encryption password
            </label>
            <input
              id={secretId}
              type="password"
              className={`${inputClass} max-w-xs`}
              value={secret}
              autoComplete="off"
              onChange={(e) => setSecret(e.target.value)}
            />
            <Button variant="primary" icon={ShieldCheck} busy={confirmer.isPending && bySecret} disabled={!secret} onClick={() => confirmer.mutate({ secret })}>
              Confirm password
            </Button>
          </div>
          {confirmer.error != null && bySecret && (
            <p role="alert" className="mt-1 text-xs text-danger">
              {confirmErrorText(confirmer.error, true)}
            </p>
          )}
          <p className="mt-3 text-xs text-ink-muted">Or use the recovery kit:</p>
        </section>
      )}

      <section aria-label="Download recovery kit" className="mb-5">
        <h3 className="mb-1 text-sm font-semibold">Download recovery kit</h3>
        <p className="mb-2 text-xs text-ink-muted">
          A text file with the encryption password, the location and the exact commands to list and restore the backup with restic or rclone alone. Whoever holds it
          (and access to the storage) can read the backup. Every download sends a warning notification.
        </p>
        <PasswordConfirm
          value={password}
          onChange={setPassword}
          error={exporter.error}
          reason="The kit contains the encryption password, so it needs your Bunkarr password."
        />
        <div className="mb-2">
          <Checkbox
            label="Include the storage credentials"
            help="Off: the kit says to create a new access key in your provider's console when you need it, which keeps the kit alone from reaching the storage."
            checked={includeCredentials}
            onChange={setIncludeCredentials}
          />
        </div>
        <Button icon={Download} busy={exporter.isPending} disabled={!password} onClick={() => exporter.mutate()}>
          Download recovery kit
        </Button>
        {exporter.error != null && !isPasswordError(exporter.error) && <ErrorNotice error={exporter.error} className="mt-2" />}
        {saved && (
          <p className="mt-2 flex items-center gap-1 text-xs text-accent" role="status">
            <CheckCircle2 className="h-4 w-4" aria-hidden="true" /> Saved as {saved.filename}. Print it or store it offline, away from this server.
          </p>
        )}
        {saved?.text && (
          <textarea aria-label="Recovery kit" readOnly rows={8} className={`${inputClass} mt-2 font-mono text-xs`} value={saved.text} />
        )}
      </section>

      {state !== 'confirmed' && (
        <section aria-label="Type the check code from the kit">
          <h3 className="mb-1 text-sm font-semibold">Type the check code from the kit</h3>
          <p className="mb-2 text-xs text-ink-muted">Open the kit you saved and type its check code (XXXX-XXXX). This proves the kit reached you.</p>
          <div className="flex flex-wrap items-center gap-2">
            <label htmlFor={codeId} className="sr-only">
              Check code
            </label>
            <input
              id={codeId}
              className={`${inputClass} w-40 font-mono uppercase`}
              value={code}
              placeholder="XXXX-XXXX"
              autoComplete="off"
              spellCheck={false}
              maxLength={16}
              onChange={(e) => setCode(e.target.value)}
            />
            <Button variant="primary" icon={ShieldCheck} busy={confirmer.isPending && !bySecret} disabled={!code.trim()} onClick={() => confirmer.mutate({ checkCode: code.trim() })}>
              Confirm check code
            </Button>
          </div>
          {confirmer.error != null && !bySecret && (
            <p role="alert" className="mt-1 text-xs text-danger">
              {confirmErrorText(confirmer.error, false)}
            </p>
          )}
        </section>
      )}
    </div>
  );
}

/** RecoveryKitDialog opens the kit step for a destination from its card. */
export function RecoveryKitDialog({ destination, onClose }: { destination: Destination; onClose: () => void }) {
  return (
    <Modal
      title={`Recovery kit · ${destination.name}`}
      size="lg"
      onClose={onClose}
      footer={
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <RecoveryKitPanel destination={destination} />
    </Modal>
  );
}
