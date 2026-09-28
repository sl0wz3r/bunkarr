import { screen, waitFor, within } from '@testing-library/react';
import type { UserEvent } from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import type { DestinationInput, EngineTestInput, EngineTestResult } from '@/api/types';
import { callsTo, type Handler } from '@/test/fetch';
import { destination, GiB, hostKeys, rcloneDestination, resticDestination, source } from '@/test/fixtures';
import { renderApp } from '@/test/render';
import { slowPage } from '@/test/slow';

// The add wizard and Edit of restic and rclone destinations (docs/design/phase4.md §15, §14.1
// web): the flow per kind and engine, write-only credentials, host-key confirmation, the "no
// encryption" acknowledgement, the recovery kit download and confirmation, and S29's password.

slowPage();

const LOGIN = 'my-login-password';
const pinned = hostKeys.map(({ type, key }) => ({ type, key }));

function base(extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    'GET /api/v1/destinations': () => ({ body: [] }),
    'GET /api/v1/sources': () => ({ body: [source(), source({ id: 2, name: 'TV', path: '/media/tv', destFolder: 'tv' })] }),
    'GET /api/v1/integrations': () => ({ body: [] }),
    ...extra,
  };
}

const tested = (over: Partial<EngineTestResult> = {}): EngineTestResult => ({
  ok: true,
  reachable: true,
  entries: 0,
  freeBytes: null,
  engineVersion: 'restic 0.18.1',
  message: 'Reachable; a new repository will be created.',
  warnings: null,
  ...over,
});

/** fill replaces a field's text (pasting, so long values stay quick). */
async function fill(user: UserEvent, el: HTMLElement, text: string) {
  await user.clear(el);
  await user.click(el);
  await user.paste(text);
}

async function openAdd(user: UserEvent) {
  await user.click((await screen.findAllByRole('button', { name: 'Add destination' }))[0]);
  return within(await screen.findByRole('dialog', { name: 'Add destination' }));
}

/** captureDownloads stubs the browser download of a Blob URL and records the file names. */
function captureDownloads() {
  const names: string[] = [];
  const blobs: Blob[] = [];
  vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: vi.fn((b: Blob) => (blobs.push(b), 'blob:kit')), revokeObjectURL: vi.fn() }));
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    names.push(this.download);
  });
  return { names, blobs };
}

describe('Add an SFTP restic destination', () => {
  it('pins the confirmed host keys, tests, asks for the password and leads to the recovery kit', async () => {
    const downloads = captureDownloads();
    const created = resticDestination();
    let confirms = 0;
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'POST /api/v1/destinations/sftp/hostkeys': () => ({ body: hostKeys }),
        'POST /api/v1/destinations/test': () => ({ body: tested({ repository: 'missing' }) }),
        'POST /api/v1/destinations': () => ({ status: 201, body: created }),
        'POST /api/v1/destinations/9/recovery-kit': () => ({
          text: 'BUNKARR RECOVERY KIT\nCheck code: ABCD-EFGH\n',
          headers: { 'Content-Type': 'text/plain; charset=utf-8', 'Content-Disposition': 'attachment; filename=bunkarr-recovery-offsite-20260927.txt' },
        }),
        'POST /api/v1/destinations/9/recovery-kit/confirm': () => (++confirms === 1 ? { status: 400, body: { message: 'wrong check code' } } : { status: 204 }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'Offsite');
    await user.click(form.getByRole('radio', { name: /SFTP server/ }));
    // Off-site kinds default to restic.
    expect(form.getByRole('radio', { name: /restic \(snapshots, deduplicated\)/ })).toBeChecked();
    expect(form.getByText(/Restores need restic and the recovery kit/)).toBeInTheDocument();
    await fill(user, form.getByLabelText('Host'), 'backup.example.net');
    await fill(user, form.getByLabelText('User'), 'bunkarr');
    await fill(user, form.getByLabelText('Path'), '/srv/bunkarr');

    // Nothing is tested (the password never reaches an unverified host) before keys are pinned.
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText(/Fetch the server.s host keys and confirm their fingerprints/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/test')).toHaveLength(0);

    await user.click(form.getByRole('button', { name: 'Fetch host keys' }));
    const presented = within(await form.findByRole('group', { name: 'Presented host keys' }));
    expect(presented.getByText(hostKeys[0].fingerprint)).toBeInTheDocument();
    expect(presented.getByText(hostKeys[1].fingerprint)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/sftp/hostkeys')[0].body).toEqual({ host: 'backup.example.net', port: 22 });
    expect(presented.getByRole('button', { name: 'Pin these keys' })).toBeDisabled();
    await user.click(presented.getByRole('checkbox', { name: /These fingerprints are my server's/ }));
    await user.click(presented.getByRole('button', { name: 'Pin these keys' }));
    expect(within(form.getByRole('list', { name: 'Pinned host keys' })).getAllByRole('listitem')).toHaveLength(2);

    // Write-only credentials: password inputs, never prefilled.
    const pw = form.getByLabelText('Password');
    expect(pw).toHaveAttribute('type', 'password');
    await fill(user, pw, 'short');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText(/SFTP passwords and key passphrases need at least 8 characters/)).toBeInTheDocument();
    await fill(user, pw, 'correct horse battery');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('No repository yet')).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/test')[0].body as EngineTestInput).toEqual({
      kind: 'sftp',
      engine: 'restic',
      remote: { host: 'backup.example.net', port: 22, user: 'bunkarr', path: '/srv/bunkarr', hostKeys: pinned },
      credentials: { password: 'correct horse battery' },
      encryption: { mode: 'restic' },
    });

    await user.click(form.getByRole('checkbox', { name: /^Movies/ }));
    // S29: an off-site destination needs the user's password in this dialog.
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Enter your Bunkarr password to confirm/)).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(0);
    await fill(user, form.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput;
    expect(body).toMatchObject({
      name: 'Offsite',
      kind: 'sftp',
      engine: 'restic',
      target: '',
      sourceIds: [1],
      remote: { host: 'backup.example.net', port: 22, user: 'bunkarr', path: '/srv/bunkarr', hostKeys: pinned },
      credentials: { password: 'correct horse battery' },
      encryption: { mode: 'restic' },
      retentionSchedule: { cron: '30 4 * * *', enabled: true },
      currentPassword: LOGIN,
    });
    expect(body).not.toHaveProperty('attach');
    expect(body.settings).toMatchObject({ transfers: 4, verify: { sampleMaxBytes: 4 * GiB } });
    expect(body.settings.restic).toEqual({ packSizeMiB: 64, batchBytes: 64 * GiB, batchFiles: 20000, pruneEveryDays: 7, pruneMaxUnused: '10%' });
    expect(body.retention).toMatchObject({ deletedDays: 30, snapshotDaily: 7, snapshotWeekly: 4, snapshotMonthly: 6, snapshotYearly: 0 });

    // The recovery kit step follows in the same dialog.
    const kit = within(await screen.findByRole('dialog', { name: 'Recovery kit · Offsite' }));
    expect(kit.getByText('Recovery kit not confirmed')).toBeInTheDocument();
    expect(kit.getByRole('button', { name: 'Download recovery kit' })).toBeDisabled();
    await fill(user, kit.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(kit.getByRole('button', { name: 'Download recovery kit' }));
    expect(await kit.findByText(/Saved as bunkarr-recovery-offsite-20260927.txt/)).toBeInTheDocument();
    expect(downloads.names).toEqual(['bunkarr-recovery-offsite-20260927.txt']);
    expect(await downloads.blobs[0].text()).toContain('Check code: ABCD-EFGH');
    expect(callsTo(calls, 'POST /api/v1/destinations/9/recovery-kit')[0].body).toEqual({ currentPassword: LOGIN, includeStorageCredentials: false });
    // The kit is never kept on the page.
    expect(kit.queryByRole('textbox', { name: 'Recovery kit' })).not.toBeInTheDocument();

    await fill(user, kit.getByLabelText('Check code'), 'abcd-efgx');
    await user.click(kit.getByRole('button', { name: 'Confirm check code' }));
    expect(await kit.findByText(/Wrong check code/)).toBeInTheDocument();
    await fill(user, kit.getByLabelText('Check code'), 'abcd-efgh');
    await user.click(kit.getByRole('button', { name: 'Confirm check code' }));
    expect(await kit.findByText('Recovery kit confirmed')).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/9/recovery-kit/confirm').map((c) => c.body)).toEqual([{ checkCode: 'abcd-efgx' }, { checkCode: 'abcd-efgh' }]);
  });
});

describe('Add an S3 rclone destination', () => {
  it('fills the provider endpoint and needs the acknowledgement and the password to store plain files', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'POST /api/v1/destinations/test': () => ({ body: tested({ marker: 'missing', engineVersion: 'rclone v1.74.1', message: 'Reachable; the bucket is empty.' }) }),
        'POST /api/v1/destinations': () => ({ status: 201, body: rcloneDestination({ encryption: { mode: 'none', origin: '', kitExportedAt: null, kitConfirmedAt: null } }) }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'Wasabi');
    await user.click(form.getByRole('radio', { name: /S3-compatible storage/ }));
    await user.click(form.getByRole('radio', { name: /rclone \(plain copy of the files\)/ }));
    await user.selectOptions(form.getByLabelText('Provider'), 'Wasabi');
    expect(form.getByLabelText('Endpoint')).toHaveValue('https://s3.us-east-1.wasabisys.com');
    await fill(user, form.getByLabelText('Region'), 'eu-central-2');
    expect(form.getByLabelText('Endpoint')).toHaveValue('https://s3.eu-central-2.wasabisys.com');
    await fill(user, form.getByLabelText('Bucket'), 'media-backup');
    await fill(user, form.getByLabelText('Prefix'), 'bunkarr');
    await fill(user, form.getByLabelText('Access key ID'), 'AKIAEXAMPLE');
    await fill(user, form.getByLabelText('Secret access key'), 'wJalrXUtnFEMI/K7MDENG');

    // Encrypted by default; opting out needs a checkbox.
    expect(form.getByRole('radio', { name: /Encrypt with rclone crypt \(recommended\)/ })).toBeChecked();
    await user.click(form.getByRole('radio', { name: /Do not encrypt/ }));
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('No marker yet')).toBeInTheDocument();
    expect((callsTo(calls, 'POST /api/v1/destinations/test')[0].body as EngineTestInput).remote).toEqual({
      provider: 'Wasabi',
      endpoint: 'https://s3.eu-central-2.wasabisys.com',
      region: 'eu-central-2',
      bucket: 'media-backup',
      prefix: 'bunkarr',
      storageClass: '',
      forcePathStyle: false,
      caCert: '',
    });

    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Confirm that the storage provider can read every file/)).toBeInTheDocument();
    await user.click(form.getByRole('checkbox', { name: /I understand that the storage provider can read every file/ }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Enter your Bunkarr password/)).toBeInTheDocument();
    expect(form.getByRole('region', { name: 'Confirm with your password' })).toHaveTextContent(/without encryption/);
    await fill(user, form.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput;
    expect(body).toMatchObject({
      kind: 's3',
      engine: 'rclone',
      encryption: { mode: 'none', acceptUnencrypted: true },
      credentials: { accessKeyId: 'AKIAEXAMPLE', secretAccessKey: 'wJalrXUtnFEMI/K7MDENG' },
      currentPassword: LOGIN,
    });
    expect(body.settings.rclone).toEqual({ batchFiles: 1000, batchBytes: 64 * GiB });
    expect(body.retention).not.toHaveProperty('snapshotDaily');
    // Nothing to confirm without a secret: the dialog closes.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});

describe('Add a B2 restic destination with my own password', () => {
  it('explains a 403 and a wrong password, then asks for the encryption password again', async () => {
    let posts = 0;
    const created = resticDestination({
      id: 11,
      name: 'B2',
      kind: 'b2',
      target: 'b2:bunkarr-media/',
      remote: { bucket: 'bunkarr-media', prefix: '' },
      hasCredentials: { keyId: true, applicationKey: true },
      encryption: { mode: 'restic', origin: 'user', kitExportedAt: null, kitConfirmedAt: null },
    });
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'POST /api/v1/destinations/test': () => ({ body: tested({ repository: 'missing', warnings: ['The application key is not restricted to the bucket.'] }) }),
        'POST /api/v1/destinations': () => {
          posts++;
          if (posts === 1) return { status: 403, body: { message: 'log in and confirm your password to send data off-site' } };
          if (posts === 2) return { status: 400, body: { message: 'current password is incorrect' } };
          return { status: 201, body: created };
        },
        'POST /api/v1/destinations/11/recovery-kit/confirm': () => ({ status: 204 }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'B2');
    await user.click(form.getByRole('radio', { name: /Backblaze B2/ }));
    await fill(user, form.getByLabelText('Bucket'), 'bunkarr-media');
    await fill(user, form.getByLabelText('Application key ID'), '0012345abcdef0000000001');
    await fill(user, form.getByLabelText('Application key'), 'K001abcdefghijklmnopqrstuvwxyz');
    await user.click(form.getByRole('radio', { name: /Use my own \/ existing repository/ }));
    await fill(user, form.getByLabelText('Encryption password'), 'short');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('No repository yet')).toBeInTheDocument();
    expect(form.getByText('The application key is not restricted to the bucket.')).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('The encryption password needs at least 16 characters.')).toBeInTheDocument();

    const secret = 'my own long repository password';
    await fill(user, form.getByLabelText('Encryption password'), secret);
    // A changed secret needs a new test.
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Test the connection first/)).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Test' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations/test')).toHaveLength(2));
    expect((callsTo(calls, 'POST /api/v1/destinations/test')[1].body as EngineTestInput).encryption).toEqual({ mode: 'restic', generate: false, secret });

    await fill(user, form.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Only a login session can do this/)).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Your password is incorrect/)).toBeInTheDocument();
    expect(form.getByLabelText('Your Bunkarr password')).toHaveAttribute('aria-invalid', 'true');
    await user.click(form.getByRole('button', { name: 'Save' }));

    const kit = within(await screen.findByRole('dialog', { name: 'Recovery kit · B2' }));
    expect(kit.getByRole('region', { name: 'Type your password again' })).toBeInTheDocument();
    await fill(user, kit.getByLabelText('Encryption password'), secret);
    await user.click(kit.getByRole('button', { name: 'Confirm password' }));
    expect(await kit.findByText('Recovery kit confirmed')).toBeInTheDocument();
    expect(callsTo(calls, 'POST /api/v1/destinations/11/recovery-kit/confirm')[0].body).toEqual({ secret });
    expect((callsTo(calls, 'POST /api/v1/destinations')[2].body as DestinationInput).encryption).toEqual({ mode: 'restic', generate: false, secret });
  });
});

describe('Attach a local restic repository', () => {
  it('needs the existing password and an explicit attach, without the off-site password', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'POST /api/v1/destinations/test': () => ({ body: tested({ repository: 'exists', id: '4f2a9c', message: 'A restic repository is here.' }) }),
        'POST /api/v1/destinations': () => ({
          status: 201,
          body: resticDestination({ kind: 'local', target: '/backup/restic', encryption: { mode: 'restic', origin: 'user', kitExportedAt: null, kitConfirmedAt: new Date().toISOString() } }),
        }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'NAS restic');
    await user.click(form.getByRole('radio', { name: /restic repository/ }));
    await fill(user, form.getByLabelText('Target'), '/backup/restic');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Repository exists')).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/A restic repository already exists here/)).toBeInTheDocument();

    await user.click(form.getByRole('radio', { name: /Use my own \/ existing repository/ }));
    await fill(user, form.getByLabelText('Encryption password'), 'the existing repository password');
    await user.click(form.getByRole('button', { name: 'Test' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations/test')).toHaveLength(2));
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('Confirm that you want to attach the existing repository.')).toBeInTheDocument();
    await user.click(await form.findByRole('checkbox', { name: /Attach the existing repository/ }));
    expect(form.queryByLabelText('Your Bunkarr password')).not.toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput;
    expect(body).toMatchObject({ kind: 'local', engine: 'restic', target: '/backup/restic', attach: true, encryption: { mode: 'restic', generate: false } });
    expect(body).not.toHaveProperty('currentPassword');
    expect(body).not.toHaveProperty('remote');
    // An attached repository's password already opened it: custody is confirmed, the dialog closes.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});

describe('The crypt password2 of an rclone destination', () => {
  it('sends both crypt passwords of the kit, on the test and on the attach', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'POST /api/v1/destinations/test': () => ({ body: tested({ marker: 'ok', engineVersion: 'rclone v1.74.1', message: 'A Bunkarr destination is here.' }) }),
        'POST /api/v1/destinations': () => ({
          status: 201,
          body: rcloneDestination({ encryption: { mode: 'crypt', origin: 'user', kitExportedAt: null, kitConfirmedAt: new Date().toISOString() } }),
        }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'Wasabi');
    await user.click(form.getByRole('radio', { name: /S3-compatible storage/ }));
    await user.click(form.getByRole('radio', { name: /rclone \(plain copy of the files\)/ }));
    await fill(user, form.getByLabelText('Bucket'), 'media-backup');
    await fill(user, form.getByLabelText('Access key ID'), 'AKIAEXAMPLE');
    await fill(user, form.getByLabelText('Secret access key'), 'wJalrXUtnFEMI/K7MDENG');

    // password2 belongs to the user's own crypt password only.
    expect(form.queryByLabelText('Crypt password2 (salt)')).not.toBeInTheDocument();
    await user.click(form.getByRole('radio', { name: /Use my own \/ an existing crypt password/ }));
    const secret = 'the kit rclone crypt password';
    const secret2 = 'the kit rclone crypt password2';
    await fill(user, form.getByLabelText('Encryption password'), secret);
    await fill(user, form.getByLabelText('Crypt password2 (salt)'), 'short');
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('The crypt password2 needs at least 16 characters.')).toBeInTheDocument();

    await fill(user, form.getByLabelText('Crypt password2 (salt)'), secret2);
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Existing Bunkarr destination')).toBeInTheDocument();
    expect((callsTo(calls, 'POST /api/v1/destinations/test')[0].body as EngineTestInput).encryption).toEqual({ mode: 'crypt', generate: false, secret, secret2 });

    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('Confirm that you want to attach the existing Bunkarr destination on this remote.')).toBeInTheDocument();
    await user.click(form.getByRole('checkbox', { name: /Attach the existing Bunkarr destination on this remote/ }));
    await fill(user, form.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput).toMatchObject({
      kind: 's3',
      engine: 'rclone',
      attach: true,
      encryption: { mode: 'crypt', generate: false, secret, secret2 },
      currentPassword: LOGIN,
    });
    // An attach confirms the kit's custody: the dialog closes.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('sends no password2 when it is left empty, or with generated crypt passwords', async () => {
    const { calls, user } = renderApp('/destinations', base({ 'POST /api/v1/destinations/test': () => ({ body: tested({ marker: 'missing', engineVersion: 'rclone v1.74.1' }) }) }));
    const form = await openAdd(user);
    await user.click(form.getByRole('radio', { name: /Backblaze B2/ }));
    await user.click(form.getByRole('radio', { name: /rclone \(plain copy of the files\)/ }));
    await fill(user, form.getByLabelText('Bucket'), 'bunkarr-media');
    await fill(user, form.getByLabelText('Application key ID'), '0012345abcdef0000000001');
    await fill(user, form.getByLabelText('Application key'), 'K001abcdefghijklmnopqrstuvwxyz');
    await user.click(form.getByRole('radio', { name: /Use my own \/ an existing crypt password/ }));
    await fill(user, form.getByLabelText('Encryption password'), 'a crypt password only');
    await user.click(form.getByRole('button', { name: 'Test' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations/test')).toHaveLength(1));
    expect((callsTo(calls, 'POST /api/v1/destinations/test')[0].body as EngineTestInput).encryption).toEqual({
      mode: 'crypt',
      generate: false,
      secret: 'a crypt password only',
    });

    // A password2 typed before switching to generated passwords is not sent.
    await fill(user, form.getByLabelText('Crypt password2 (salt)'), 'a leftover password2 value');
    await user.click(form.getByRole('radio', { name: /Generate the crypt passwords/ }));
    expect(form.queryByLabelText('Crypt password2 (salt)')).not.toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Test' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations/test')).toHaveLength(2));
    expect((callsTo(calls, 'POST /api/v1/destinations/test')[1].body as EngineTestInput).encryption).toEqual({ mode: 'crypt' });
  });

  it('drops a password2 typed for rclone when the engine becomes restic', async () => {
    const { calls, user } = renderApp('/destinations', base({ 'POST /api/v1/destinations/test': () => ({ body: tested({ repository: 'missing' }) }) }));
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'B2');
    await user.click(form.getByRole('radio', { name: /Backblaze B2/ }));
    await user.click(form.getByRole('radio', { name: /rclone \(plain copy of the files\)/ }));
    await fill(user, form.getByLabelText('Bucket'), 'bunkarr-media');
    await fill(user, form.getByLabelText('Application key ID'), '0012345abcdef0000000001');
    await fill(user, form.getByLabelText('Application key'), 'K001abcdefghijklmnopqrstuvwxyz');
    await user.click(form.getByRole('radio', { name: /Use my own \/ an existing crypt password/ }));
    await fill(user, form.getByLabelText('Crypt password2 (salt)'), 'short');

    await user.click(form.getByRole('radio', { name: /restic \(snapshots, deduplicated\)/ }));
    await user.click(form.getByRole('radio', { name: /Use my own \/ existing repository/ }));
    expect(form.queryByLabelText('Crypt password2 (salt)')).not.toBeInTheDocument();
    const secret = 'my own long repository password';
    await fill(user, form.getByLabelText('Encryption password'), secret);
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('No repository yet')).toBeInTheDocument();
    // The server refuses secret2 with restic (400).
    expect((callsTo(calls, 'POST /api/v1/destinations/test')[0].body as EngineTestInput).encryption).toEqual({ mode: 'restic', generate: false, secret });
    // The hidden rclone password2 does not block the restic create.
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Enter your Bunkarr password to confirm/)).toBeInTheDocument();
    expect(form.queryByText('The crypt password2 needs at least 16 characters.')).not.toBeInTheDocument();
  });

  it('confirms a crypt remote created with a password2 by the kit’s check code only', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'POST /api/v1/destinations/test': () => ({ body: tested({ marker: 'missing', engineVersion: 'rclone v1.74.1' }) }),
        'POST /api/v1/destinations': () => ({ status: 201, body: rcloneDestination({ encryption: { mode: 'crypt', origin: 'user', kitExportedAt: null, kitConfirmedAt: null } }) }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'Wasabi');
    await user.click(form.getByRole('radio', { name: /S3-compatible storage/ }));
    await user.click(form.getByRole('radio', { name: /rclone \(plain copy of the files\)/ }));
    await fill(user, form.getByLabelText('Bucket'), 'media-backup');
    await fill(user, form.getByLabelText('Access key ID'), 'AKIAEXAMPLE');
    await fill(user, form.getByLabelText('Secret access key'), 'wJalrXUtnFEMI/K7MDENG');
    await user.click(form.getByRole('radio', { name: /Use my own \/ an existing crypt password/ }));
    await fill(user, form.getByLabelText('Encryption password'), 'my own rclone crypt password');
    await fill(user, form.getByLabelText('Crypt password2 (salt)'), 'my own rclone crypt password2');
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('No marker yet')).toBeInTheDocument();
    await fill(user, form.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(form.getByRole('button', { name: 'Save' }));

    const kit = within(await screen.findByRole('dialog', { name: 'Recovery kit · Wasabi' }));
    expect(callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput).not.toHaveProperty('attach');
    // The server refuses the password alone: the kit holds both, so only its check code confirms it.
    expect(kit.queryByRole('region', { name: 'Type your password again' })).not.toBeInTheDocument();
    expect(kit.getByRole('region', { name: 'Type the check code from the kit' })).toBeInTheDocument();
    expect(kit.getByText(/until you download its recovery kit and type the check code/)).toBeInTheDocument();
  });
});

describe('Edit an rclone destination', () => {
  it('keeps the SFTP login hint off the stored S3 keys', async () => {
    const { user } = renderApp('/destinations', base({ 'GET /api/v1/destinations': () => ({ body: [rcloneDestination()] }) }));
    await user.click(await screen.findByRole('button', { name: 'Edit Wasabi' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit destination · Wasabi' }));
    expect(form.getAllByText(/Leave empty to keep it, type to replace it\./)).toHaveLength(2);
    expect(form.queryByText(/replaces the stored login/)).not.toBeInTheDocument();
  });
});

describe('Edit a restic destination', () => {
  it('keeps kind and location read-only, shows stored credentials, and asks for the password for new ones', async () => {
    const d = resticDestination({ encryption: { mode: 'restic', origin: 'generated', kitExportedAt: null, kitConfirmedAt: new Date().toISOString() }, blockedReason: '' });
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [d] }),
        'PUT /api/v1/destinations/9': () => ({ body: d }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit Offsite' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit destination · Offsite' }));
    expect(form.getByLabelText('Host')).toBeDisabled();
    expect(form.getByLabelText('Path')).toBeDisabled();
    expect(form.queryByRole('radio', { name: /SFTP server/ })).not.toBeInTheDocument();
    expect(form.getByText(/cannot change: create a new destination instead/)).toBeInTheDocument();
    const pw = form.getByLabelText(/^Password/);
    expect(pw).toHaveValue('');
    expect(pw).toHaveAttribute('placeholder', 'Stored. Leave empty to keep it; type to replace it.');
    // The server replaces the whole SFTP login when a key or password is sent.
    expect(form.getByText(/Typing a new private key or password replaces the stored login/)).toBeInTheDocument();
    expect(form.queryByLabelText('Your Bunkarr password')).not.toBeInTheDocument();

    // A change that sends nothing new needs no password and sends no credentials.
    await fill(user, form.getByLabelText('Keep deleted files'), '60');
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/destinations/9')).toHaveLength(1));
    const first = callsTo(calls, 'PUT /api/v1/destinations/9')[0].body as DestinationInput;
    expect(first.retention.deletedDays).toBe(60);
    expect(first).not.toHaveProperty('credentials');
    expect(first).not.toHaveProperty('remote');
    expect(first).not.toHaveProperty('currentPassword');
    expect(first).not.toHaveProperty('encryption');

    await user.click(await screen.findByRole('button', { name: 'Edit Offsite' }));
    const again = within(await screen.findByRole('dialog', { name: 'Edit destination · Offsite' }));
    await fill(user, again.getByLabelText(/^Password/), 'a brand new password');
    await fill(user, again.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(again.getByRole('checkbox', { name: /^TV/ }));
    await user.click(again.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/destinations/9')).toHaveLength(2));
    const second = callsTo(calls, 'PUT /api/v1/destinations/9')[1].body as DestinationInput;
    expect(second).toMatchObject({ credentials: { password: 'a brand new password' }, currentPassword: LOGIN, sourceIds: [1, 2] });
    expect(second).not.toHaveProperty('remote');
  });
});

describe('Re-pin the host keys of an SFTP destination', () => {
  it('sends the new keys with the stored location and asks for the password', async () => {
    const d = resticDestination({ encryption: { mode: 'restic', origin: 'generated', kitExportedAt: null, kitConfirmedAt: new Date().toISOString() }, blockedReason: '' });
    const rotated = [{ type: 'ssh-ed25519', fingerprint: 'SHA256:NewServerKeyAfterAReinstall000000000000000', key: 'AAAAC3NzaC1lZDI1NTE5AAAAINewKey' }];
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [d] }),
        'POST /api/v1/destinations/sftp/hostkeys': () => ({ body: rotated }),
        'PUT /api/v1/destinations/9': () => ({ body: d }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit Offsite' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit destination · Offsite' }));
    expect(within(form.getByRole('list', { name: 'Pinned host keys' })).getAllByRole('listitem')).toHaveLength(2);
    await user.click(form.getByRole('button', { name: 'Fetch host keys' }));
    const presented = within(await form.findByRole('group', { name: 'Presented host keys' }));
    await user.click(presented.getByRole('checkbox', { name: /These fingerprints are my server's/ }));
    await user.click(presented.getByRole('button', { name: 'Pin these keys' }));
    await fill(user, form.getByLabelText('Your Bunkarr password'), LOGIN);
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'PUT /api/v1/destinations/9')).toHaveLength(1));
    const body = callsTo(calls, 'PUT /api/v1/destinations/9')[0].body as DestinationInput;
    expect(body.remote).toEqual({ host: 'backup.example.net', port: 22, user: 'bunkarr', path: '/srv/bunkarr', hostKeys: [{ type: 'ssh-ed25519', key: 'AAAAC3NzaC1lZDI1NTE5AAAAINewKey' }] });
    expect(body.currentPassword).toBe(LOGIN);
    expect(body).not.toHaveProperty('credentials');
  });
});

describe('Bandwidth and window editor', () => {
  it('shows timetable overlaps per day and sends the window with its overrun choice', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'POST /api/v1/destinations/test': () => ({
          body: { ok: true, marker: 'missing', writable: true, fsType: 'nfs', local: false, capabilities: null, freeBytes: GiB, totalBytes: 2 * GiB, entries: 0, message: 'ok', warnings: [] },
        }),
        'POST /api/v1/destinations': () => ({ status: 201, body: destination() }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'UNAS');
    await fill(user, form.getByLabelText('Target'), '/backup');
    await user.click(form.getByRole('button', { name: 'Test' }));
    await form.findByText('None yet (written on create)');

    await user.click(form.getByRole('button', { name: 'Add a timetable line' }));
    await user.click(form.getByRole('button', { name: 'Add a timetable line' }));
    const line2 = within(form.getByRole('group', { name: 'Timetable line 2' }));
    // List semantics stay (the group is inside each item), and a day's label shows keyboard focus.
    const timetable = form.getByRole('list', { name: 'Timetable' });
    expect(within(timetable).getAllByRole('listitem').filter((li) => li.parentElement === timetable)).toHaveLength(2);
    expect(line2.getByRole('checkbox', { name: 'Mon' }).closest('label')).toHaveClass('has-[:focus-visible]:outline');
    expect(line2.getByText('Mon: overlaps line 1 (Mon 08:00–23:00).')).toBeInTheDocument();
    expect(line2.getByText('Fri: overlaps line 1 (Fri 08:00–23:00).')).toBeInTheDocument();
    expect(within(form.getByRole('group', { name: 'Timetable line 1' })).queryByText(/overlaps/)).not.toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText('Bandwidth: Timetable line 2: Mon: overlaps line 1 (Mon 08:00–23:00).')).toBeInTheDocument();

    // A night line on Friday runs into Saturday morning.
    for (const d of ['Mon', 'Tue', 'Wed', 'Thu']) await user.click(line2.getByRole('checkbox', { name: d }));
    await fill(user, line2.getByLabelText('Line 2 from'), '23:30');
    await fill(user, line2.getByLabelText('Line 2 to'), '07:00');
    await user.click(line2.getByRole('checkbox', { name: 'Fri' }));
    await user.click(line2.getByRole('checkbox', { name: 'Sat' }));
    expect(line2.queryByText(/overlaps/)).not.toBeInTheDocument();
    await fill(user, line2.getByLabelText('Line 2 upload limit'), '4096');
    await fill(user, line2.getByLabelText('Line 2 to'), '25:00');
    expect(line2.getByText('"25:00" is not a time HH:MM (00:00–23:59).')).toBeInTheDocument();
    await fill(user, line2.getByLabelText('Line 2 to'), '07:00');

    await user.click(form.getByRole('checkbox', { name: /Only transfer inside a window/ }));
    const win = within(form.getByRole('group', { name: 'Window' }));
    expect(win.getByText('Every day 01:00–07:00')).toBeInTheDocument();
    await user.click(win.getByRole('checkbox', { name: /Let a file larger than the window run past its end/ }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    const body = callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput;
    expect(body.bandwidth).toEqual({
      uploadKiBps: 0,
      downloadKiBps: 0,
      timetable: [
        { days: ['mon', 'tue', 'wed', 'thu', 'fri'], from: '08:00', to: '23:00', uploadKiBps: 1024, downloadKiBps: 0 },
        { days: ['sat'], from: '23:30', to: '07:00', uploadKiBps: 4096, downloadKiBps: 0 },
      ],
      window: { days: ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'], from: '01:00', to: '07:00', graceMinutes: 15, allowOverrun: true },
    });
  });

  it('keeps its controls inside a phone-wide screen', async () => {
    const { user } = renderApp('/destinations', base());
    const form = await openAdd(user);
    await user.click(form.getByRole('button', { name: 'Add a timetable line' }));
    const line = form.getByRole('group', { name: 'Timetable line 1' });
    // Every row of controls wraps instead of overflowing at 375 px (phase1.md §10).
    for (const row of line.querySelectorAll(':scope > div')) expect(row).toHaveClass('flex-wrap');
    expect(form.getByRole('radio', { name: /SFTP server/ }).closest('.grid')).toHaveClass('sm:grid-cols-2');
    expect(form.getByRole('radio', { name: /SFTP server/ }).closest('label')).toHaveClass('min-w-0');
  });
});

describe('Finish a create that did not finish', () => {
  // A create whose restic init ran but did not finish leaves a pending row with its generated
  // password (§4.5). A new test password cannot open that repository: the wizard sends no
  // secret, so the server finishes it with the kept one.
  const pending = resticDestination({
    id: 14,
    name: 'NAS restic',
    kind: 'local',
    target: '/backup/restic',
    fsType: 'nfs',
    remote: {},
    hasCredentials: null,
    pending: true,
    enabled: false,
    blockedReason: 'the create did not finish',
    encryption: { mode: 'restic', origin: 'generated', kitExportedAt: null, kitConfirmedAt: null },
  });
  const wrongPassword = tested({ ok: false, repository: 'wrong-password', message: 'wrong password' });

  it('finishes it from the Add wizard at the same location, without a secret or attach', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [pending] }),
        'POST /api/v1/destinations/test': () => ({ body: wrongPassword }),
        'POST /api/v1/destinations': () => ({ status: 201, body: { ...pending, pending: false, enabled: true, blockedReason: '' } }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'NAS restic');
    await user.click(form.getByRole('radio', { name: /restic repository/ }));
    await fill(user, form.getByLabelText('Target'), '/backup/restic/');
    expect(await form.findByText('Finishes the create of NAS restic')).toBeInTheDocument();
    expect(form.queryByRole('radio', { name: /Use my own \/ existing repository/ })).not.toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Test' }));
    expect(await form.findByText('Wrong password')).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    expect(form.queryByText(/A restic repository already exists here/)).not.toBeInTheDocument();
    const body = callsTo(calls, 'POST /api/v1/destinations')[0].body as DestinationInput;
    expect(body).toMatchObject({ kind: 'local', engine: 'restic', target: '/backup/restic/', encryption: { mode: 'restic' } });
    expect(body.encryption).not.toHaveProperty('secret');
    expect(body).not.toHaveProperty('attach');
    // Its kit is still unconfirmed: the recovery kit step follows.
    expect(await screen.findByRole('dialog', { name: 'Recovery kit · NAS restic' })).toBeInTheDocument();
  });

  it('opens the wizard filled in from the banner’s Finish create', async () => {
    const { calls, user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [pending] }),
        'POST /api/v1/destinations/test': () => ({ body: wrongPassword }),
        'POST /api/v1/destinations': () => ({ status: 201, body: { ...pending, pending: false, enabled: true, blockedReason: '' } }),
      }),
    );
    expect(await screen.findByText(/Creating NAS restic did not finish: finish it/)).toBeInTheDocument();
    // One Finish create: the banner's (the row offers its own only when there is no banner).
    await user.click(screen.getByRole('button', { name: 'Finish creating NAS restic' }));
    const form = within(await screen.findByRole('dialog', { name: 'Finish creating · NAS restic' }));
    expect(form.getByLabelText('Name')).toHaveValue('NAS restic');
    expect(form.getByLabelText('Target')).toHaveValue('/backup/restic');
    expect(form.getByText('Finishes the create of NAS restic')).toBeInTheDocument();
    // Enabled again: the pending row is stored disabled until the create finishes.
    expect(form.getByRole('checkbox', { name: /Run scheduled and manual syncs/ })).toBeChecked();
    await user.click(form.getByRole('button', { name: 'Save' }));
    expect(await form.findByText(/Test the connection first/)).toBeInTheDocument();
    await user.click(form.getByRole('button', { name: 'Test' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations/test')).toHaveLength(1));
    await user.click(form.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(calls, 'POST /api/v1/destinations')).toHaveLength(1));
    expect(callsTo(calls, 'POST /api/v1/destinations')[0].body).toMatchObject({ name: 'NAS restic', target: '/backup/restic', encryption: { mode: 'restic' } });
  });
});

describe('Warnings of a save', () => {
  it('keeps the dialog open to show an attach’s warnings', async () => {
    const warning = 'another Bunkarr may be writing to this repository';
    const { user } = renderApp(
      '/destinations',
      base({
        'POST /api/v1/destinations/test': () => ({ body: tested({ repository: 'exists', id: '4f2a9c' }) }),
        'POST /api/v1/destinations': () => ({
          status: 201,
          body: resticDestination({
            name: 'NAS restic',
            kind: 'local',
            target: '/backup/restic',
            warnings: [warning],
            encryption: { mode: 'restic', origin: 'user', kitExportedAt: null, kitConfirmedAt: new Date().toISOString() },
          }),
        }),
      }),
    );
    const form = await openAdd(user);
    await fill(user, form.getByLabelText('Name'), 'NAS restic');
    await user.click(form.getByRole('radio', { name: /restic repository/ }));
    await fill(user, form.getByLabelText('Target'), '/backup/restic');
    await user.click(form.getByRole('radio', { name: /Use my own \/ existing repository/ }));
    await fill(user, form.getByLabelText('Encryption password'), 'the existing repository password');
    await user.click(form.getByRole('button', { name: 'Test' }));
    await user.click(await form.findByRole('checkbox', { name: /Attach the existing repository/ }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    const done = within(await screen.findByRole('dialog', { name: 'Created · NAS restic' }));
    expect(done.getByText(warning)).toBeInTheDocument();
    await user.click(done.getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('shows the warnings of an update', async () => {
    const d = rcloneDestination({ kind: 'b2', name: 'B2 copy', blockedReason: '' });
    const warning = 'the B2 application key is not restricted to the bucket';
    const { user } = renderApp(
      '/destinations',
      base({
        'GET /api/v1/destinations': () => ({ body: [d] }),
        'PUT /api/v1/destinations/10': () => ({ body: { ...d, warnings: [warning] } }),
      }),
    );
    await user.click(await screen.findByRole('button', { name: 'Edit B2 copy' }));
    const form = within(await screen.findByRole('dialog', { name: 'Edit destination · B2 copy' }));
    await user.click(form.getByRole('button', { name: 'Save' }));
    const done = within(await screen.findByRole('dialog', { name: 'Saved · B2 copy' }));
    expect(done.getByText(warning)).toBeInTheDocument();
  });
});
