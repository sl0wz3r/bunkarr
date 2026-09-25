# 0003. Secrets at rest

- Status: accepted
- Date: 2026-09-24

## Context

API keys and destination credentials must be encrypted at rest with a key derived from a
generated master key in `/config`, never logged, and redacted in the UI and diagnostics.

## Decision

- `/config/bunkarr.key`: 32 random bytes, hex, mode 0600, created atomically (temp file + `link`)
  so a crash never leaves a truncated key and concurrent starts agree on one key.
- The settings key is derived with HKDF-SHA256 (`info = "bunkarr settings v1"`). Values are
  sealed with AES-256-GCM, random 96-bit nonce, and the setting's name as associated data, so a
  sealed value copied into another row does not decrypt. Format: `v1:` + base64url(nonce‖ct).
- A value that does not open (wrong key, tampering) fails loudly with a message pointing at
  `bunkarr.key`; Bunkarr never silently regenerates secrets.

## Consequences

- Threat model: protects a leaked database file, a diagnostics bundle or a database-only backup.
  It does not protect against someone who can read the whole `/config` (key and database
  together); nothing that must decrypt unattended can.
- **Operators must back up `bunkarr.key` with the database.** The README and compose file say so.
- The `v1:` prefix allows a later key rotation or algorithm change.
