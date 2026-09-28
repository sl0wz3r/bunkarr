#!/bin/sh
# Argv-recording shim of the off-site acceptance suite (docs/design/phase4.md §14.6 item 4,
# acceptance 4). The suite's test image installs this file, root-owned and not writable by the
# Bunkarr UID, as /opt/bunkarr-e2e/bin/restic and /opt/bunkarr-e2e/bin/rclone, and points
# BUNKARR_RESTIC_PATH and BUNKARR_RCLONE_PATH at them. Each run appends one record with its
# arguments to /argvlog/argv.log and then executes the real binary (/usr/bin/<name>) with the same
# arguments, so every command Bunkarr starts is recorded, short-lived ones (rcat, cat config)
# included, and restic's `-o rclone.program=<this shim>` records `rclone serve restic` too. The
# environment is never recorded: it is where the engines' secrets are expected (S22).
#
# A record is one write of:
#   ---- <UTC time> pid=<pid> ppid=<ppid> <name>
#   arg: <argument>          (one line per argument, in order)
#   ---- end
name=${0##*/}
case $name in
restic | rclone) ;;
*)
	echo "argv-shim: unknown program name $name" >&2
	exit 127
	;;
esac
rec="---- $(date -u +%Y-%m-%dT%H:%M:%SZ) pid=$$ ppid=$PPID $name"
for a in "$@"; do
	rec="$rec
arg: $a"
done
rec="$rec
---- end"
printf '%s\n' "$rec" >>/argvlog/argv.log
exec "/usr/bin/$name" "$@"
