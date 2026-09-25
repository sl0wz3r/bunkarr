# 0001. Stack and container base

- Status: accepted
- Date: 2026-09-24

## Context

The spec fixes Go, SQLite via `modernc.org/sqlite`, `chi`, React + TypeScript + Vite, Tailwind,
and restic/rclone as backup engines, and allows "distroless or alpine" as the image base. The
container must honour `PUID`/`PGID`/`UMASK`/`TZ` like the linuxserver/hotio images Unraid users
expect.

## Decision

- **Alpine** (digest-pinned) with `tini` and `su-exec`. A shell entrypoint creates the user and
  group, fixes ownership of Bunkarr's own files in `/config` (never recursively, never source
  media), and drops privileges. Distroless has no shell, `adduser` or package manager, so PUID/PGID
  handling would move into the Go binary running as root, and restic/rclone would have to be
  copied in by hand. Alpine packages restic and rclone and keeps them patched with `apk upgrade`.
- **Two SQLite pools**: one writer connection (`BEGIN IMMEDIATE`) and a small `query_only` read
  pool, WAL mode. Writes are serialized in-process, so no `SQLITE_BUSY` on lock upgrades.
- **Router**: `chi` as specified; the standard library's `http.CrossOriginProtection` for CSRF
  and `slog.NewMultiHandler` for the dual log output (both Go 1.25+/1.26+), so no extra packages.
- **Password hashing**: bcrypt from `golang.org/x/crypto`, cost 12, at most 4 hashes at a time.

## Consequences

- Image ≈ 230 MB, mostly restic and rclone. Acceptable for the target (NAS appdata).
- The entrypoint is shell and is covered by shellcheck and `docker/test-image.sh`.
