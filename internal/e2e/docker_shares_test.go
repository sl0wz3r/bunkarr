//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// alpineImage is the base of the Samba server image: the Bunkarr image's own runtime base
// (Dockerfile), pinned by digest.
const alpineImage = "alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"

// The Samba account of the share test.
const (
	shareUser     = "unas"
	sharePassword = "unas-password-not-secret"
)

// Mount options of Unraid's Unassigned Devices plugin for remote shares (include/lib.php,
// get_mount_params), which is how Bunkarr's target shares are mounted on Unraid: SMB without Unix
// extensions and with client-side inode numbers, the SMB version set to 3.1.1; NFS with the
// version negotiated.
const (
	udCIFSOptions = "rw,hard,relatime,noserverino,nounix,iocharset=utf8,file_mode=0777,dir_mode=0777,uid=99,gid=100," +
		"retrans=3,actimeo=10,rsize=1048576,wsize=1048576,closetimeo=30,vers=3.1.1"
	udNFSOptions = "rw,hard,timeo=50,retrans=5,relatime,rsize=1048576,wsize=1048576"
)

// sambaScript runs a standalone Samba server that shares the volume at /share as [unas] with one
// user.
const sambaScript = `set -eu
adduser -D -H -u 1000 "$SHARE_USER"
printf '%s\n%s\n' "$SHARE_PASSWORD" "$SHARE_PASSWORD" | smbpasswd -a -s "$SHARE_USER"
chown "$SHARE_USER" /share
cat > /etc/samba/smb.conf <<EOF
[global]
  server role = standalone server
  disable netbios = yes
  smb ports = 445
  map to guest = never
  load printers = no
  printing = bsd
  printcap name = /dev/null
  disable spoolss = yes
[unas]
  path = /share
  read only = no
  valid users = $SHARE_USER
EOF
exec smbd --foreground --no-process-group --debug-stdout`

// nfsScript runs the kernel NFS server, NFSv4 only (no rpcbind), exporting the volume at /share
// (a volume: overlayfs cannot be exported). The grace period after the start, during which no file
// can be created, is cut from 90 s to the minimum, 10 s.
const nfsScript = `set -eu
mount -t nfsd nfsd /proc/fs/nfsd
echo "/share *(rw,sync,no_subtree_check,no_root_squash,insecure,fsid=0)" > /etc/exports
exportfs -r
rpc.nfsd -N 3 -V 4 -G 10 -L 10 8
exec rpc.mountd -F -N 3 -V 4`

// shareClientScript mounts the share at /mnt/share (retrying while the server starts), creates a
// file (which waits out an NFS server's grace period) and runs the e2e test binary as Unraid's
// nobody:users (99:100) with every destination on the share.
const shareClientScript = `set -eu
mkdir -p /mnt/share
i=0
until mount -t "$SHARE_FSTYPE" "$SHARE_REMOTE" /mnt/share -o "$SHARE_OPTIONS"; do
  i=$((i + 1))
  if [ "$i" -ge 30 ]; then echo "cannot mount $SHARE_REMOTE"; exit 1; fi
  sleep 1
done
grep ' /mnt/share ' /proc/mounts
touch /mnt/share/.e2e-ready && rm /mnt/share/.e2e-ready
if [ "$SHARE_FSTYPE" != cifs ]; then chown 99:100 /mnt/share; fi
exec su-exec 99:100 /e2e.test -test.v -test.count=1 -test.timeout=25m -test.run "$E2E_RUN"`

// shareTests are the tests the share test runs with their destinations on the share.
const shareTests = `^(TestSyncLifecycle|TestKillResume)$`

// TestDockerShares runs TestSyncLifecycle and TestKillResume (this package's test binary,
// cross-compiled for the Docker server) with every destination target on a real network share
// instead of a local directory, as Bunkarr is deployed against a NAS: a Samba server mounted over
// CIFS with Unraid Unassigned Devices' options (nounix, noserverino, SMB 3.1.1) and the kernel NFS
// server mounted over NFSv4. The client is a privileged container of the Bunkarr image plus
// cifs-utils and nfs-utils; the binary under test is the image's own. BUNKARR_E2E_SHARES lists
// the shares to test (smb, nfs; see docker/test-shares.sh).
func TestDockerShares(t *testing.T) {
	kinds := os.Getenv("BUNKARR_E2E_SHARES")
	if kinds == "" {
		t.Skip("BUNKARR_E2E_SHARES is not set (run docker/test-shares.sh)")
	}
	testBin := linuxTestBinary(t, newDockerEnv(t))
	for _, kind := range strings.Split(kinds, ",") {
		switch kind = strings.TrimSpace(kind); kind {
		case "smb":
			t.Run("smb", func(t *testing.T) {
				d := newDockerEnv(t)
				netName, _ := d.network("net")
				samba := d.build("samba", "FROM "+alpineImage+"\nRUN apk add --no-cache samba\n")
				share := d.volume("share")
				srv := d.run("smb", "--network", netName, "-v", share+":/share",
					"-e", "SHARE_USER="+shareUser, "-e", "SHARE_PASSWORD="+sharePassword, "--entrypoint", "sh", samba, "-c", sambaScript)
				runOnShare(t, d, shareClientImage(d), testBin, netName, share, "cifs", "//"+d.ipOn(srv, netName)+"/unas",
					udCIFSOptions+",username="+shareUser+",password="+sharePassword)
			})
		case "nfs":
			t.Run("nfs", func(t *testing.T) {
				d := newDockerEnv(t)
				netName, _ := d.network("net")
				client := shareClientImage(d)
				share := d.volume("share")
				srv := d.run("nfs", "--privileged", "--network", netName, "-v", share+":/share",
					"--entrypoint", "sh", client, "-c", nfsScript)
				runOnShare(t, d, client, testBin, netName, share, "nfs", d.ipOn(srv, netName)+":/", udNFSOptions)
			})
		default:
			t.Fatalf("BUNKARR_E2E_SHARES: unknown share %q (want smb, nfs)", kind)
		}
	}
}

// shareClientImage builds the client image (also the NFS server's): the Bunkarr image plus
// cifs-utils and nfs-utils.
func shareClientImage(d *dockerEnv) string {
	d.t.Helper()
	return d.build("client", "FROM "+d.image+"\nRUN apk add --no-cache cifs-utils nfs-utils\n")
}

// runOnShare mounts remote (filesystem type fstype, mount options opts), whose directory on the
// server is the volume share, in a privileged container of the client image on network netName
// and runs shareTests from testBin there, failing when they fail. The volume is also mounted
// read-only at /mnt/share-server, where the tests compare the server's inode numbers.
func runOnShare(t *testing.T, d *dockerEnv, client, testBin, netName, share, fstype, remote, opts string) {
	t.Helper()
	c := d.name("client")
	d.containers = append(d.containers, c)
	d.docker("create", "--name", c, "--privileged", "--network", netName, "-v", share+":/mnt/share-server:ro",
		"-e", "SHARE_FSTYPE="+fstype, "-e", "SHARE_REMOTE="+remote, "-e", "SHARE_OPTIONS="+opts, "-e", "E2E_RUN="+shareTests,
		"-e", "BUNKARR_E2E_BINARY=/app/bunkarr", "-e", "BUNKARR_E2E_TARGET_ROOT=/mnt/share",
		"-e", "BUNKARR_E2E_TARGET_BACKING=/mnt/share-server", "-e", "TMPDIR=/tmp",
		"--entrypoint", "sh", client, "-c", shareClientScript)
	d.docker("cp", testBin, c+":/e2e.test")
	out, err := exec.Command("docker", "start", "-a", c).CombinedOutput()
	t.Logf("---- %s share %s ----\n%s", fstype, remote, out)
	if err != nil {
		t.Fatalf("%s on a %s share: %v", shareTests, fstype, err)
	}
}

// linuxTestBinary cross-compiles this package's test binary for the Docker server's platform.
func linuxTestBinary(t *testing.T, d *dockerEnv) string {
	t.Helper()
	arch := d.docker("version", "--format", "{{.Server.Arch}}")
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "e2e.test")
	cmd := exec.Command("go", "test", "-c", "-tags", "e2e", "-o", out, "./internal/e2e")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test -c for linux/%s: %v\n%s", arch, err, b)
	}
	return out
}
