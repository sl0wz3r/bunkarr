#!/bin/sh
# SFTP server setup of the off-site acceptance suite (docs/design/phase4.md §14.6). The suite
# starts atmoz/sftp:alpine (pinned by digest, as the engine spike and make test-engines do) with
# the user bunkarr (uid 1001, writable directory upload) and runs this script inside it as root
# (`docker exec -i -e SFTP_KEY_PASSPHRASE <sftp> sh -s bunkarr`, the script on stdin; the
# passphrase comes through the environment, never argv). It makes a passphrase-protected ed25519
# client key, authorizes it for the user (sshd's StrictModes: the chroot home stays root's, .ssh is
# the user's), and prints the private key. The server's own host keys (ed25519 and rsa, generated
# by the image at its first start) stay in /etc/ssh/ssh_host_*_key.pub, which the suite compares
# with what POST /destinations/sftp/hostkeys presents before it pins them.
set -eu
user=${1:?usage: sftp-setup.sh USER}
: "${SFTP_KEY_PASSPHRASE:?SFTP_KEY_PASSPHRASE is not set}"
dir=$(mktemp -d)
ssh-keygen -q -t ed25519 -N "$SFTP_KEY_PASSPHRASE" -C bunkarr-offsite -f "$dir/id_ed25519"
mkdir -p "/home/$user/.ssh"
cat "$dir/id_ed25519.pub" >>"/home/$user/.ssh/authorized_keys"
chown "$user" "/home/$user/.ssh" "/home/$user/.ssh/authorized_keys"
chmod 700 "/home/$user/.ssh"
chmod 600 "/home/$user/.ssh/authorized_keys"
cat "$dir/id_ed25519"
rm -rf "$dir"
