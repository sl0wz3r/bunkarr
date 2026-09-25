#!/bin/sh
# Bunkarr container entrypoint (runs under tini as PID 1).
#
# Started as root (Unraid, docker compose, docker run: the normal case):
#   1. validate PUID / PGID / UMASK / TZ and apply the umask
#   2. create or reuse a group with PGID and a user with PUID
#   3. give /config itself (NOT recursively) and the files Bunkarr owns inside it to PUID:PGID.
#      Nothing else is ever chowned: never source media, never backup destinations.
#   4. drop privileges with su-exec and exec Bunkarr, which takes /config/bunkarr.lock so a second
#      container on the same /config refuses to start
#
# Started as a non-root user (docker run --user, Kubernetes runAsUser): steps 2-3 are skipped and
# Bunkarr runs as that user; make /config writable for it yourself.
#
# Arguments:
#   (none) or -flags ...                 /app/bunkarr serve [flags]            (as PUID:PGID)
#   serve|version|healthcheck|reset-auth /app/bunkarr <command> [flags]        (as PUID:PGID)
#   anything else                        executed as-is, as root (debugging, e.g. "sh")
set -eu

APP=/app/bunkarr
CONFIG_DIR=${BUNKARR_CONFIG_DIR:-/config}
USER_NAME=bunkarr

log() { printf '[entrypoint] %s\n' "$*"; }
warn() { printf '[entrypoint] WARNING: %s\n' "$*" >&2; }
die() {
	printf '[entrypoint] ERROR: %s\n' "$*" >&2
	exit 1
}

# normalize_id NAME VALUE: VALUE as a plain decimal id, or exit with a clear message.
normalize_id() {
	case $2 in
	'' | *[!0-9]*) die "$1 must be a numeric id (0-2147483647), got '$2'" ;;
	esac
	[ "${#2}" -le 10 ] || die "$1 is out of range: '$2'"
	_id=$(printf '%s' "$2" | sed 's/^0*//')
	[ -n "$_id" ] || _id=0
	[ "$_id" -le 2147483647 ] || die "$1 is out of range: '$2'"
	printf '%s' "$_id"
}

check_umask() {
	case $UMASK in
	[0-7] | [0-7][0-7] | [0-7][0-7][0-7] | [0-7][0-7][0-7][0-7]) ;;
	*) die "UMASK must be 1-4 octal digits such as 002 or 022, got '$UMASK'" ;;
	esac
	_mask=000$UMASK
	_mask=${_mask#"${_mask%???}"}
	case $_mask in
	0??) ;;
	*) warn "UMASK=$UMASK removes permissions from the file owner; Bunkarr may be unable to read its own files. Use 002 or 022." ;;
	esac
}

check_tz() {
	[ -n "${TZ:-}" ] || return 0
	case $TZ in
	*..*) warn "TZ='$TZ' is not a valid time zone name; times will be shown in UTC." ;;
	*) [ -f "/usr/share/zoneinfo/${TZ#:}" ] || warn "Unknown time zone TZ='$TZ' (expected e.g. America/New_York); times will be shown in UTC." ;;
	esac
}

# setup_identity: passwd/group entries for PUID/PGID so logs and `id` show names. su-exec accepts
# numeric ids anyway, so failures here are not fatal.
setup_identity() {
	_entry=$(getent passwd "$USER_NAME" || true)
	if [ -n "$_entry" ]; then
		if [ "$(printf '%s' "$_entry" | cut -d: -f3)" != "$PUID" ] || [ "$(printf '%s' "$_entry" | cut -d: -f4)" != "$PGID" ]; then
			deluser "$USER_NAME" >/dev/null 2>&1 || return 1
		fi
	fi
	_entry=$(getent group "$USER_NAME" || true)
	if [ -n "$_entry" ] && [ "$(printf '%s' "$_entry" | cut -d: -f3)" != "$PGID" ]; then
		delgroup "$USER_NAME" >/dev/null 2>&1 || return 1
	fi
	GROUP_NAME=$(getent group "$PGID" | cut -d: -f1 || true)
	if [ -z "$GROUP_NAME" ]; then
		addgroup -g "$PGID" "$USER_NAME" >/dev/null 2>&1 || return 1
		GROUP_NAME=$USER_NAME
	fi
	RUN_USER=$(getent passwd "$PUID" | cut -d: -f1 || true)
	if [ -z "$RUN_USER" ]; then
		adduser -D -H -h "$CONFIG_DIR" -s /sbin/nologin -G "$GROUP_NAME" -u "$PUID" "$USER_NAME" >/dev/null 2>&1 || return 1
		RUN_USER=$USER_NAME
	fi
	return 0
}

# fix_config_ownership: /config itself plus Bunkarr's own files in it. Symlinks are changed
# themselves (-h), never followed; other filesystems are not crossed (-xdev); hard-linked files
# are skipped so a link placed in /config cannot hand over a file from elsewhere.
fix_config_ownership() {
	_rc=0
	if [ "$(stat -c '%u:%g' "$CONFIG_DIR")" != "$PUID:$PGID" ]; then
		chown "$PUID:$PGID" "$CONFIG_DIR" || _rc=1
	fi
	for _path in "$CONFIG_DIR"/bunkarr.db "$CONFIG_DIR"/bunkarr.db-* "$CONFIG_DIR"/bunkarr.key \
		"$CONFIG_DIR"/bunkarr.lock "$CONFIG_DIR"/logs; do
		[ -e "$_path" ] || [ -L "$_path" ] || continue
		if [ -L "$_path" ]; then
			warn "Not changing ownership through symlink $_path"
			continue
		fi
		find "$_path" -xdev \( -type d -o -links 1 \) ! -user "$PUID" -exec chown -h "$PUID:$PGID" {} + || _rc=1
		find "$_path" -xdev \( -type d -o -links 1 \) ! -group "$PGID" -exec chgrp -h "$PGID" {} + || _rc=1
	done
	return "$_rc"
}

can_write() {
	# shellcheck disable=SC2016 # $1 is expanded by the inner shell
	su-exec "$1" sh -c 'test -d "$1" && _f=$(mktemp -p "$1" .bunkarr-write-test.XXXXXX 2>/dev/null) && rm -f "$_f"' sh "$2"
}

case "${1:-}" in
'' | -*) set -- "$APP" serve "$@" ;;
serve | version | healthcheck | reset-auth) set -- "$APP" "$@" ;;
"$APP" | bunkarr)
	shift
	set -- "$APP" "$@"
	;;
*) exec "$@" ;;
esac

UMASK=${UMASK:-002}
check_umask
umask "$UMASK"
check_tz

if [ "$(id -u)" != 0 ]; then
	log "Running as non-root $(id -u):$(id -g) (umask $UMASK); PUID/PGID are ignored."
	exec "$@"
fi

PUID=$(normalize_id PUID "${PUID:-1000}")
PGID=$(normalize_id PGID "${PGID:-1000}")
[ "$PUID" != 0 ] || warn "PUID=0: Bunkarr will run as root. Use the id that owns your appdata (Unraid: 99)."

GROUP_NAME=$PGID
RUN_USER=$PUID
setup_identity || warn "Could not create a user/group entry for $PUID:$PGID; continuing with numeric ids."
[ -n "$RUN_USER" ] || RUN_USER=$PUID
[ -n "$GROUP_NAME" ] || GROUP_NAME=$PGID

mkdir -p "$CONFIG_DIR"
fix_config_ownership || warn "Could not give everything Bunkarr owns in $CONFIG_DIR to $PUID:$PGID."
can_write "$PUID:$PGID" "$CONFIG_DIR" ||
	die "$CONFIG_DIR is not writable for $PUID:$PGID. Check the host folder mapped to $CONFIG_DIR (a local disk, not NFS/SMB) and PUID/PGID."

[ "${2:-}" = serve ] && log "Starting Bunkarr as $RUN_USER:$GROUP_NAME ($PUID:$PGID), umask $UMASK, TZ=${TZ:-UTC}"
exec su-exec "$PUID:$PGID" "$@"
