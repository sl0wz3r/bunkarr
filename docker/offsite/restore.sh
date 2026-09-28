#!/bin/sh
# Restore-without-Bunkarr template of the off-site acceptance suite (docs/design/phase4.md §5.3,
# §14.6 item 6, acceptance 6). The suite fills in the two marked blocks from a recovery kit's text
# and nothing else: the kit's shell block (between its BEGIN SHELL and END SHELL lines) and the
# listing and restore commands the kit prints under "Then list the backups and restore", with the
# kit's placeholders (<snapshot>, <source path>, <directory>) replaced by values the kit names (the
# snapshot id comes from the listing). It runs in a fresh golang:1.27-alpine container that has
# only restic and rclone added (apk) and no /config, on the network of the storage server, and
# prints the sha256 of the restored file last.
set -eu
work=$(mktemp -d)
cd "$work"
echo "== the kit's shell block"
# @KIT_SHELL@
echo "== the kit's listing and restore commands"
# @KIT_COMMANDS@
