import { describe, expect, it } from 'vitest';
import type { CredentialsInput, S3Remote, SftpRemote } from '@/api/types';
import { resticDestination, rcloneDestination, destination } from '@/test/fixtures';
import {
  EMPTY_B2,
  EMPTY_S3,
  EMPTY_SFTP,
  createNeedsPassword,
  credentialProblem,
  engineChoices,
  engineSettingsDefaults,
  pendingAt,
  fillEndpoint,
  isEncrypted,
  kitState,
  locationProblem,
  presetOf,
  remoteFor,
  repositoryBytes,
  sameHostKeys,
  secretProblem,
  snapshotCount,
  typedCredentials,
  updateNeedsPassword,
} from './destinationKinds';

const GiB = 1024 ** 3;

describe('choices', () => {
  it('offers filecopy first locally and restic first off-site', () => {
    expect(engineChoices('local').map((c) => c.value)).toEqual(['filecopy', 'restic']);
    for (const k of ['sftp', 's3', 'b2'] as const) expect(engineChoices(k).map((c) => c.value)).toEqual(['restic', 'rclone']);
    for (const c of engineChoices('s3')) expect(c.help.length).toBeGreaterThan(40);
  });

  it('fills the provider endpoint patterns', () => {
    expect(fillEndpoint(presetOf('Wasabi'), 'eu-central-2')).toBe('https://s3.eu-central-2.wasabisys.com');
    expect(fillEndpoint(presetOf('Wasabi'), '')).toBe('https://s3.us-east-1.wasabisys.com');
    expect(fillEndpoint(presetOf('AWS'), 'eu-west-1')).toBe('');
    expect(fillEndpoint(presetOf('Cloudflare'), 'auto')).toBe('https://{account}.r2.cloudflarestorage.com');
    expect(presetOf('nonsense').provider).toBe('Other');
  });
});

describe('credentials', () => {
  it('sends only typed fields of the kind', () => {
    const typed: CredentialsInput = { password: 'long enough', privateKey: '', accessKeyId: 'not sftp' };
    expect(typedCredentials('sftp', typed)).toEqual({ password: 'long enough' });
    expect(typedCredentials('sftp', { password: '' })).toBeUndefined();
    expect(typedCredentials('local', { password: 'x' })).toBeUndefined();
  });

  it.each([
    ['s3', { accessKeyId: 'a' }, /access key ID and the secret/],
    ['b2', { keyId: 'k' }, /application key ID and the application key/],
    ['sftp', {}, /private key or a password/],
    ['sftp', { password: 'long enough', privateKeyPassphrase: 'passphrase' }, /needs the private key/],
    ['sftp', { password: 'short' }, /at least 8 characters/],
  ] as const)('refuses incomplete %s credentials', (kind, c, msg) => {
    expect(credentialProblem(kind, c)).toMatch(msg);
  });

  it('accepts complete credentials', () => {
    expect(credentialProblem('sftp', { privateKey: '-----BEGIN OPENSSH PRIVATE KEY-----' })).toBeNull();
    expect(credentialProblem('s3', { accessKeyId: 'a', secretAccessKey: 'b' })).toBeNull();
  });

  it('checks a chosen encryption password as the server does', () => {
    expect(secretProblem('0123456789abcdef')).toBeNull();
    expect(secretProblem('0123456789abcde')).toMatch(/16 characters/);
    expect(secretProblem(' 0123456789abcdef')).toMatch(/space/);
    expect(secretProblem('0123456789abcdef\n')).toMatch(/space|control/);
    expect(secretProblem('0123456789\u0007abcdef')).toMatch(/control/);
  });
});

describe('locations', () => {
  it('requires the pinned host keys, the endpoint and the bucket', () => {
    const r = { sftp: { ...EMPTY_SFTP, host: 'h', user: 'u' }, s3: EMPTY_S3, b2: EMPTY_B2 };
    expect(locationProblem('sftp', r)).toMatch(/host keys/);
    expect(locationProblem('sftp', { ...r, sftp: { ...r.sftp, hostKeys: [{ type: 'ssh-ed25519', key: 'AAAA' }] } })).toBeNull();
    expect(locationProblem('s3', { ...r, s3: { ...EMPTY_S3, provider: 'Minio', endpoint: 'https://{account}.x', bucket: 'b' } })).toMatch(/endpoint/);
    expect(locationProblem('s3', { ...r, s3: { ...EMPTY_S3, bucket: 'b' } })).toBeNull();
    expect(locationProblem('b2', r)).toMatch(/bucket/);
    expect(remoteFor('b2', { ...r, b2: { bucket: ' media ', prefix: ' a/b ' } })).toEqual({ bucket: 'media', prefix: 'a/b' });
    expect(remoteFor('local', r)).toBeUndefined();
  });
});

describe('S29 (the password in the same dialog)', () => {
  it('asks when creating an off-site destination or accepting no encryption', () => {
    expect(createNeedsPassword('local', false)).toBe(false);
    expect(createNeedsPassword('b2', false)).toBe(true);
    expect(createNeedsPassword('local', true)).toBe(true);
  });

  it('asks when an update changes credentials, host keys, the CA certificate or links a source off-site', () => {
    const d = resticDestination();
    const keys = d.remote.hostKeys!;
    expect(updateNeedsPassword(d, { sourceIds: [1], hostKeys: keys })).toBe(false);
    expect(updateNeedsPassword(d, { sourceIds: [1], hostKeys: [...keys].reverse() })).toBe(false);
    expect(updateNeedsPassword(d, { sourceIds: [1], hostKeys: keys.slice(1) })).toBe(true);
    expect(updateNeedsPassword(d, { sourceIds: [1], credentials: { password: 'new password' } })).toBe(true);
    expect(updateNeedsPassword(d, { sourceIds: [1, 2] })).toBe(true);
    expect(updateNeedsPassword(d, { sourceIds: [] })).toBe(false);
    const s3 = rcloneDestination({ remote: { ...rcloneDestination().remote, caCert: '-----BEGIN CERTIFICATE-----\nX\n' } });
    expect(updateNeedsPassword(s3, { sourceIds: [1], caCert: '-----BEGIN CERTIFICATE-----\nX' })).toBe(false);
    expect(updateNeedsPassword(s3, { sourceIds: [1], caCert: '' })).toBe(true);
    // A local destination never asks for linking a source.
    expect(updateNeedsPassword(destination(), { sourceIds: [1, 2] })).toBe(false);
    expect(sameHostKeys(undefined, [])).toBe(true);
  });
});

describe('custody and engine state', () => {
  it('reads the kit state and encryption', () => {
    expect(kitState(destination())).toBe('none');
    expect(kitState(resticDestination())).toBe('unconfirmed');
    expect(kitState(rcloneDestination())).toBe('confirmed');
    expect(isEncrypted(destination())).toBe(false);
    expect(isEncrypted(rcloneDestination())).toBe(true);
  });

  it('reads the repository figures when the engine state has them', () => {
    const st = { destinationId: 9, engineVersion: '', lastPruneAt: null, lastCheckAt: null, readSubsetNext: 1, lastCleanupAt: null, throughputBps: null, updatedAt: '' };
    expect(snapshotCount({ ...st, stats: { snapshotCount: 3 } })).toBe(3);
    expect(repositoryBytes({ ...st, stats: { repositoryBytes: 5 } })).toBe(5);
    expect(repositoryBytes({ ...st, stats: { bytes: 5, totalSize: 6 } })).toBeNull();
    expect(snapshotCount({ ...st, stats: {} })).toBeNull();
    expect(snapshotCount(null)).toBeNull();
  });

  it('fills the engine settings per engine and never adds them to filecopy', () => {
    const base = destination().settings;
    expect(engineSettingsDefaults(base, 'filecopy', 'local')).toEqual(base);
    const restic = engineSettingsDefaults(base, 'restic', 'local');
    expect(restic.restic?.packSizeMiB).toBe(16);
    expect(restic.verify.sampleMaxBytes).toBe(4 * GiB);
    expect(engineSettingsDefaults(base, 'restic', 'b2').restic?.packSizeMiB).toBe(64);
    const rclone = engineSettingsDefaults(base, 'rclone', 's3');
    expect(rclone.rclone).toEqual({ batchFiles: 1000, batchBytes: 64 * GiB });
    expect(rclone.verify.sampleMaxBytes).toBe(16 * GiB);
    expect(rclone).not.toHaveProperty('restic');
  });
});

describe('pendingAt', () => {
  const sftp = resticDestination({ pending: true });
  const b2 = rcloneDestination({ id: 12, kind: 'b2', pending: true, remote: { bucket: 'Media', prefix: 'bunkarr' } });
  const s3 = rcloneDestination({ id: 13, pending: true, remote: { endpoint: '', region: '', bucket: 'media', prefix: 'x' } });
  const list = [sftp, b2, s3, resticDestination({ id: 15, kind: 'local', target: '/backup/r', remote: {}, pending: false })];
  const sftpAt = (over: Partial<SftpRemote>) => ({ host: 'Backup.example.net', port: 22, user: 'other', path: '/srv/bunkarr', hostKeys: [], ...over });

  it('matches a pending destination of the same kind, engine and location only', () => {
    expect(pendingAt(list, 'sftp', 'restic', '', sftpAt({}))).toBe(sftp);
    expect(pendingAt(list, 'sftp', 'rclone', '', sftpAt({}))).toBeNull();
    expect(pendingAt(list, 'sftp', 'restic', '', sftpAt({ path: '/srv/other' }))).toBeNull();
    expect(pendingAt(list, 'sftp', 'restic', '', sftpAt({ port: 2222 }))).toBeNull();
    expect(pendingAt(list, 'b2', 'rclone', '', { bucket: 'media', prefix: 'bunkarr' })).toBe(b2);
    expect(pendingAt(list, 'b2', 'rclone', '', { bucket: 'media', prefix: '' })).toBeNull();
    const s3At = { provider: 'aws', endpoint: '', region: 'us-east-1', bucket: 'media', prefix: 'x', caCert: '' } as unknown as S3Remote;
    expect(pendingAt(list, 's3', 'rclone', '', s3At)).toBe(s3);
    expect(pendingAt(list, 's3', 'rclone', '', { ...s3At, region: 'eu-west-1' })).toBeNull();
    // Not pending: a create there is refused by the overlap rule, not resumed.
    expect(pendingAt(list, 'local', 'restic', '/backup/r', undefined)).toBeNull();
    expect(pendingAt(undefined, 'local', 'restic', '/backup/r', undefined)).toBeNull();
  });
});
