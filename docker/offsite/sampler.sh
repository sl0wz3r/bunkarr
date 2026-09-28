#!/bin/sh
# /proc sampler of the off-site acceptance suite (docs/design/phase4.md §14.6 item 4). The suite
# runs it as root inside the Bunkarr container (`docker exec -i -u 0 <c> sh -s <config dir>`, the
# script on stdin) for as long as the container runs. About every 100 ms it prints one sample:
#   T <unix time>
#   P <contents of /proc/<pid>/stat>              for every process ...
#   C <argv, each NUL replaced by \001>          ... followed by its command line
#   F <octal mode> <uid> <type> <path>            every entry under /dev/shm/bunkarr-run and
#                                                 <config dir>/run
#   E
# The Go side keeps the processes that descend from the Bunkarr server and checks their argv for
# every secret (clear, JSON-escaped, rclone-obscured with any IV), and checks the modes of the
# per-command secret directories (0700) and files (0600). The argv shims record every command as
# well; this sampler shows the processes while they run, the rclone children restic starts
# included.
config=${1:-/config}
while :; do
	echo "T $(date +%s)"
	for d in /proc/[0-9]*; do
		stat=$(cat "$d/stat" 2>/dev/null) || continue
		cmd=$(tr '\000' '\001' <"$d/cmdline" 2>/dev/null) || continue
		[ -n "$cmd" ] || continue
		printf 'P %s\nC %s\n' "$stat" "$cmd"
	done
	for dir in /dev/shm/bunkarr-run "$config/run"; do
		[ -d "$dir" ] || continue
		find "$dir" -mindepth 1 -exec stat -c 'F %a %u %F %n' {} + 2>/dev/null
	done
	echo E
	sleep 0.1
done
