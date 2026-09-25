import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Bell, FlaskConical, Pencil, Plus, Trash2 } from 'lucide-react';
import { useState, type FormEvent, type ReactNode } from 'react';
import { errorMessage } from '@/api/client';
import { createNotification, deleteNotification, listNotifications, testNotification, updateNotification } from '@/api/integrations';
import type { Notification, NotificationInput, TestResult } from '@/api/types';
import { Button, IconButton } from '@/components/Button';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { DataTable, type Column } from '@/components/DataTable';
import { Checkbox, CheckboxField, FormRow, FormSection, SecretField, TextField } from '@/components/Form';
import { Modal } from '@/components/Modal';
import { ErrorNotice, Notice } from '@/components/Notice';
import { EmptyState, Page } from '@/components/Page';
import { Badge } from '@/components/StatusBadge';
import { keys } from '@/lib/lookups';

/** joinUrls turns one-per-line (or comma/space separated) Apprise URLs into Apprise's comma list. */
export function joinUrls(text: string): string {
  return text
    .split(/[\s,]+/)
    .map((u) => u.trim())
    .filter(Boolean)
    .join(',');
}

function events(n: Pick<Notification, 'onFailure' | 'onWarning' | 'onSuccess'>): string {
  const list = [n.onFailure && 'failure', n.onWarning && 'warnings', n.onSuccess && 'success'].filter(Boolean);
  return list.length ? list.join(', ') : 'none';
}

/** Settings → Connect: Apprise notification targets. */
export function Connect() {
  const qc = useQueryClient();
  const notifications = useQuery({ queryKey: keys.notifications, queryFn: listNotifications });
  const [editing, setEditing] = useState<Notification | 'new' | null>(null);
  const [deleting, setDeleting] = useState<Notification | null>(null);
  const [notice, setNotice] = useState<{ tone: 'success' | 'error'; body: ReactNode } | null>(null);
  const [testing, setTesting] = useState<number | null>(null);

  async function testSaved(n: Notification) {
    setNotice(null);
    setTesting(n.id);
    try {
      const r = await testNotification({ id: n.id, name: n.name, kind: 'apprise', enabled: n.enabled, apiUrl: n.apiUrl, configKey: n.configKey, onFailure: n.onFailure, onWarning: n.onWarning, onSuccess: n.onSuccess });
      setNotice({ tone: r.ok ? 'success' : 'error', body: `${n.name}: ${r.message || (r.ok ? 'Test notification sent.' : 'Test failed.')}` });
    } catch (e) {
      setNotice({ tone: 'error', body: `${n.name}: ${errorMessage(e)}` });
    } finally {
      setTesting(null);
    }
  }

  const columns: Column<Notification>[] = [
    {
      key: 'name',
      header: 'Name',
      cell: (n) => (
        <div>
          <span className="font-medium">{n.name}</span>
          {!n.enabled && (
            <span className="ml-2">
              <Badge>Disabled</Badge>
            </span>
          )}
          <div className="break-all font-mono text-xs text-ink-muted">{n.apiUrl}</div>
        </div>
      ),
    },
    {
      key: 'mode',
      header: 'Apprise',
      cell: (n) =>
        n.configKey ? (
          <span className="text-xs">
            Stateful: config <code>{n.configKey}</code>
          </span>
        ) : (
          <span className="flex items-center gap-2 text-xs">
            Stateless {n.hasUrls ? <Badge tone="ok">URLs stored</Badge> : <Badge tone="warn">No URLs</Badge>}
          </span>
        ),
    },
    { key: 'events', header: 'Notify on', cell: (n) => <span className="text-xs">{events(n)}</span> },
    {
      key: 'actions',
      header: <span className="sr-only">Actions</span>,
      className: 'whitespace-nowrap text-right',
      cell: (n) => (
        <>
          <Button small variant="ghost" icon={FlaskConical} busy={testing === n.id} aria-label={`Test ${n.name}`} onClick={() => void testSaved(n)}>
            Test
          </Button>
          <IconButton label={`Edit ${n.name}`} icon={Pencil} onClick={() => setEditing(n)} />
          <IconButton label={`Delete ${n.name}`} icon={Trash2} className="hover:text-danger" onClick={() => setDeleting(n)} />
        </>
      ),
    },
  ];

  const list = notifications.data;
  return (
    <Page
      title="Connect"
      actions={
        <Button variant="ghost" icon={Plus} onClick={() => setEditing('new')}>
          Add notification
        </Button>
      }
    >
      {notice && <Notice tone={notice.tone}>{notice.body}</Notice>}
      <ErrorNotice error={notifications.error} />
      {list && list.length === 0 ? (
        <EmptyState icon={Bell} title="No notifications yet">
          <p>
            Bunkarr notifies through an Apprise API server: failed jobs, jobs with warnings (held changes, verify mismatches) and, if you like, successful ones.
          </p>
          <div className="mt-4">
            <Button variant="primary" icon={Plus} onClick={() => setEditing('new')}>
              Add notification
            </Button>
          </div>
        </EmptyState>
      ) : (
        <DataTable columns={columns} rows={list} rowKey={(n) => n.id} loading={notifications.isPending} caption="Notifications" />
      )}
      {editing && <NotificationForm notification={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
      {deleting && (
        <ConfirmDialog
          title="Delete notification"
          confirmLabel="Delete"
          danger
          onClose={() => setDeleting(null)}
          onConfirm={async () => {
            await deleteNotification(deleting.id);
            await qc.invalidateQueries({ queryKey: keys.notifications });
          }}
        >
          <p>
            Delete <strong>{deleting.name}</strong>? Its stored URLs are removed.
          </p>
        </ConfirmDialog>
      )}
    </Page>
  );
}

type Mode = 'stateless' | 'stateful';

/**
 * NotificationForm adds or edits an Apprise target. Stateless: Bunkarr sends the Apprise URLs
 * with every notification (write-only, stored encrypted). Stateful: the Apprise server holds
 * them under a configuration key.
 */
function NotificationForm({ notification, onClose }: { notification: Notification | null; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(notification?.name ?? '');
  const [apiUrl, setApiUrl] = useState(notification?.apiUrl ?? '');
  const [mode, setMode] = useState<Mode>(notification?.configKey ? 'stateful' : 'stateless');
  const [urls, setUrls] = useState('');
  const [configKey, setConfigKey] = useState(notification?.configKey ?? '');
  const [onFailure, setOnFailure] = useState(notification?.onFailure ?? true);
  const [onWarning, setOnWarning] = useState(notification?.onWarning ?? true);
  const [onSuccess, setOnSuccess] = useState(notification?.onSuccess ?? false);
  const [enabled, setEnabled] = useState(notification?.enabled ?? true);
  const [formError, setFormError] = useState<string | null>(null);
  // Counts Save and Test clicks, so a repeated form error is shown (scrolled into view) again.
  const [attempt, setAttempt] = useState(0);
  const [test, setTest] = useState<TestResult | null>(null);

  const storedUrls = !!notification?.hasUrls && mode === 'stateless';

  function body(): NotificationInput | null {
    setAttempt((n) => n + 1);
    if (!name.trim() || !apiUrl.trim()) {
      setFormError('Enter a name and the Apprise API URL.');
      return null;
    }
    const joined = joinUrls(urls);
    if (mode === 'stateless' && !joined && !storedUrls) {
      setFormError('Enter at least one Apprise URL.');
      return null;
    }
    if (mode === 'stateful' && !configKey.trim()) {
      setFormError('Enter the Apprise configuration key.');
      return null;
    }
    const b: NotificationInput = {
      name: name.trim(),
      kind: 'apprise',
      enabled,
      apiUrl: apiUrl.trim(),
      configKey: mode === 'stateful' ? configKey.trim() : '',
      onFailure,
      onWarning,
      onSuccess,
    };
    if (mode === 'stateless' && joined) {
      b.urls = joined;
    }
    return b;
  }

  const tester = useMutation({ mutationFn: testNotification, onSuccess: setTest });
  const saver = useMutation({
    mutationFn: (b: NotificationInput) => (notification ? updateNotification(notification.id, b) : createNotification(b)),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: keys.notifications });
      onClose();
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    setFormError(null);
    const b = body();
    if (b) saver.mutate(b);
  }

  function runTest() {
    setFormError(null);
    setTest(null);
    const b = body();
    if (b) tester.mutate(notification ? { ...b, id: notification.id } : b);
  }

  return (
    <Modal
      title={notification ? `Edit notification · ${notification.name}` : 'Add notification'}
      size="lg"
      onClose={onClose}
      footer={
        <>
          <Button icon={FlaskConical} busy={tester.isPending} onClick={runTest}>
            Test
          </Button>
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="notification-form" busy={saver.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="notification-form" onSubmit={submit} noValidate>
        {formError && (
          <Notice key={attempt} tone="error">
            {formError}
          </Notice>
        )}
        <ErrorNotice error={saver.error ?? tester.error} />
        {test && (
          <Notice tone={test.ok ? 'success' : 'error'} reveal revealKey={test}>
            {test.message || (test.ok ? 'Test notification sent.' : 'Test failed.')}
          </Notice>
        )}
        <FormSection title="Apprise">
          <TextField label="Name" value={name} onChange={setName} autoFocus={!notification} placeholder="Phone" />
          <TextField label="Apprise API URL" type="url" value={apiUrl} onChange={setApiUrl} mono placeholder="http://apprise:8000" help="The Apprise API server (caronc/apprise) Bunkarr posts to." />
          <FormRow label="Mode" group>
            <div className="space-y-2 pt-2">
              <label className="flex items-start gap-2 text-sm">
                <input type="radio" name="apprise-mode" className="mt-0.5 accent-[var(--color-accent)]" checked={mode === 'stateless'} onChange={() => setMode('stateless')} />
                <span>
                  Stateless
                  <span className="block text-xs text-ink-muted">Bunkarr stores the Apprise URLs (encrypted) and sends them with each notification.</span>
                </span>
              </label>
              <label className="flex items-start gap-2 text-sm">
                <input type="radio" name="apprise-mode" className="mt-0.5 accent-[var(--color-accent)]" checked={mode === 'stateful'} onChange={() => setMode('stateful')} />
                <span>
                  Stateful
                  <span className="block text-xs text-ink-muted">The Apprise server keeps the URLs under a configuration key.</span>
                </span>
              </label>
            </div>
          </FormRow>
          {mode === 'stateless' ? (
            <SecretField
              label="Apprise URLs"
              multiline
              value={urls}
              onChange={setUrls}
              stored={storedUrls}
              reenter={!!notification && apiUrl.trim() !== notification.apiUrl}
              placeholder={'tgram://bottoken/ChatID\nntfy://ntfy.sh/topic'}
              help="One per line. They often contain tokens, so they are write-only: stored encrypted and never shown again."
            />
          ) : (
            <TextField label="Configuration key" value={configKey} onChange={setConfigKey} mono placeholder="bunkarr" />
          )}
        </FormSection>
        <FormSection title="Events" description="Previews (dry runs) only notify when they fail.">
          <FormRow label="Notify on" group>
            <div className="space-y-2 pt-2">
              <Checkbox label="Failed jobs" checked={onFailure} onChange={setOnFailure} />
              <Checkbox label="Jobs with warnings" help="Including held changes and verify mismatches." checked={onWarning} onChange={setOnWarning} />
              <Checkbox label="Successful jobs" checked={onSuccess} onChange={setOnSuccess} />
            </div>
          </FormRow>
          <CheckboxField label="Enabled" checked={enabled} onChange={setEnabled} text="Send notifications to this target" />
        </FormSection>
      </form>
    </Modal>
  );
}
