//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kitFiles is the source of the recovery-kit test.
var kitFiles = map[string]int64{
	"kit/Movies/Nosferatu (1922)/Nosferatu (1922).mkv": 3 << 20,
	"kit/Movies/Nosferatu (1922)/Nosferatu (1922).srt": 12000,
	"kit/TV/The Show/Season 01/The Show - S01E01.mkv":  1 << 20,
}

// kitRestored is the file each restore without Bunkarr must reproduce.
const kitRestored = "Movies/Nosferatu (1922)/Nosferatu (1922).mkv"

// TestDockerOffsiteKit is acceptance 6 (docs/design/phase4.md §5.2, §5.3, §14.6 item 6): a
// remote destination created with the defaults is encrypted; until its recovery kit custody is
// confirmed, a sync (and verify and retention) answers 409 and only a dry run runs; the kit needs a
// UI session and the password; after the export and the check code, the sync runs; then, in a
// fresh golang:1.27-alpine container with only restic and rclone added and no /config, a script
// built only from the kit's text lists the backups and restores one file per engine (restic, and
// rclone crypt), whose sha256 equals the source's.
func TestDockerOffsiteKit(t *testing.T) {
	o := newOffsite(t)
	o.startMinIO()
	o.writeRandom(kitFiles)
	b := o.startBunkarr("bunkarr", bunkarrOpts{})
	src := b.createSource("Kit", "kit")

	kits := map[string]kitInfo{}
	for _, eng := range []string{"restic", "rclone"} {
		t.Logf("== %s", eng)
		var body map[string]any
		if eng == "restic" {
			body = b.resticS3("Kit restic", "kit-restic", []int64{src.ID}, nil)
		} else {
			body = b.cryptS3("Kit crypt", "kit-crypt", []int64{src.ID}, nil)
		}
		// No encryption field: the defaults (restic, rclone crypt).
		delete(body, "encryption")
		d := b.createDest(body)
		want := map[string]string{"restic": "restic", "rclone": "crypt"}[eng]
		if d.Encryption.Mode != want || d.Encryption.Origin != "generated" || d.Encryption.KitConfirmedAt != nil {
			t.Fatalf("destination created with the defaults: encryption %+v, want %s, generated, not confirmed", d.Encryption, want)
		}
		if !strings.Contains(d.Blocked, "recovery kit") {
			t.Fatalf("blockedReason %q, want the recovery kit", d.Blocked)
		}
		for _, job := range []string{"sync", "verify", "retention"} {
			code, resp := b.api.status(viaKey, "POST", fmt.Sprintf("/destinations/%d/%s", d.ID, job), nil)
			if code != 409 || !strings.Contains(string(resp), "recovery kit") {
				t.Fatalf("%s before the kit was confirmed: HTTP %d %s, want 409 naming the recovery kit", job, code, resp)
			}
		}
		// A dry run is allowed (S21).
		requireStatus(t, b.sync(d.ID, map[string]any{"dryRun": true}), "completed")
		// The API key cannot export the kit; a wrong password is refused.
		path := fmt.Sprintf("/destinations/%d/recovery-kit", d.ID)
		if code, resp := b.api.status(viaKey, "POST", path, map[string]any{"currentPassword": e2ePassword}); code != 403 {
			t.Fatalf("kit export with the API key: HTTP %d %s, want 403", code, resp)
		}
		if code, resp := b.api.status(viaSession, "POST", path, map[string]any{"currentPassword": "not-the-password"}); code != 400 {
			t.Fatalf("kit export with a wrong password: HTTP %d %s, want 400", code, resp)
		}
		k := b.confirmKit(d.ID)
		if k.Engine != eng || len(k.Sources) != 1 || k.Sources[0].Path != offsiteMedia+"/kit" {
			t.Fatalf("kit: engine %q, sources %+v", k.Engine, k.Sources)
		}
		j := o.untouchedTree("kit", func() apiJob { return b.sync(d.ID, nil) })
		requireStatus(t, j, "completed")
		if st := decodeStats[engineSync](t, j); st.FilesCopied != int64(len(kitFiles)) || st.FilesFailed != 0 {
			t.Fatalf("first sync: %s", j.Stats)
		}
		kits[eng] = k
	}

	// A fresh container: golang:1.27-alpine plus restic and rclone, no /config, on the network.
	r := o.d.run("restore", "--network", o.net, "--entrypoint", "sleep", offImage("BUNKARR_E2E_GOLANG_IMAGE", defaultGolangImage), "3600")
	o.d.docker("exec", r, "apk", "add", "--no-cache", "restic", "rclone")
	if o.d.execOK(r, "test", "-e", "/config") {
		t.Fatal("the restore container has a /config")
	}
	want := o.sha256Of("kit/" + kitRestored)
	for _, eng := range []string{"restic", "rclone"} {
		script := o.restoreScript(kits[eng], kitRestored)
		out := o.execStdin(r, nil, script, "sh", "-s")
		got := map[string]string{}
		for _, l := range strings.Split(out, "\n") {
			// sha256sum: "<64 hex>  <path>" (the path may hold spaces)
			if len(l) > 66 && l[64] == ' ' && l[65] == ' ' && isHex(l[:64]) {
				got[l[66:]] = l[:64]
			}
		}
		if want := map[string]int{"restic": 2, "rclone": 1}[eng]; len(got) != want {
			t.Fatalf("%s restore printed %d sha256 lines, want %d:\n%s", eng, len(got), want, out)
		}
		for p, sum := range got {
			if sum != want {
				t.Fatalf("%s restore without Bunkarr: %s has sha256 %s, the source's is %s\n%s", eng, p, sum, want, out)
			}
		}
		if eng == "restic" && !strings.Contains(out, "bunkarr-dest:"+kits[eng].EngineTag) {
			t.Fatalf("restic snapshots listing without the destination's tag:\n%s", out)
		}
		t.Logf("%s: %d restored copies match the source's sha256 %.12s", eng, len(got), want)
	}
	o.auditAll()
}

// restoreScript fills docker/offsite/restore.sh with a kit's shell block and the listing and
// restore commands it prints; rel is the file (relative to the kit's only source) to restore and
// hash.
func (o *offsite) restoreScript(k kitInfo, rel string) string {
	o.t.Helper()
	tmpl, err := os.ReadFile(filepath.Join(o.root, "docker", "offsite", "restore.sh"))
	if err != nil {
		o.t.Fatal(err)
	}
	shell := k.shellBlock()
	cmds := k.commands()
	if shell == "" || len(cmds) < 2 || len(k.Sources) != 1 {
		o.t.Fatalf("the kit has no shell block, commands or single source:\n%s", k.Text)
	}
	var run []string
	switch k.Engine {
	case "restic":
		// restic snapshots --tag '<tag>' / restic restore <snapshot>:<source path> --target
		// <directory> / restic dump <snapshot> <file path> > <file>
		tag := ""
		for _, c := range cmds {
			if strings.HasPrefix(c, "restic snapshots ") {
				run = append(run, c)
				_, tag, _ = strings.Cut(c, "restic snapshots ")
			}
		}
		if !strings.HasPrefix(tag, "--tag ") {
			o.t.Fatalf("the kit's snapshot listing has no tag filter: %q", cmds)
		}
		for _, c := range cmds {
			switch {
			case strings.HasPrefix(c, "restic restore "):
				c = strings.NewReplacer("<snapshot>", "latest", "<source path>", shq(k.Sources[0].Path), "<directory>", "./restored").Replace(c)
				run = append(run, c+" "+tag)
			case strings.HasPrefix(c, "restic dump "):
				c = strings.NewReplacer("<snapshot>", "latest", "<file path>", shq(k.Sources[0].Path+"/"+rel), "<file>", "./dumped.bin").Replace(c)
				run = append(run, strings.Replace(c, "restic dump ", "restic dump "+tag+" ", 1))
			}
		}
		run = append(run, "sha256sum ./restored/"+shq(rel)+" ./dumped.bin")
	case "rclone":
		// rclone lsd '<root>' / rclone copy '<root><destFolder>' <directory>
		for _, c := range cmds {
			switch {
			case strings.HasPrefix(c, "rclone lsd "):
				run = append(run, c)
			case strings.HasPrefix(c, "rclone copy "):
				run = append(run, strings.Replace(c, "<directory>", "./restored", 1))
			}
		}
		run = append(run, "sha256sum ./restored/"+shq(rel))
	}
	if len(run) < 3 {
		o.t.Fatalf("could not build the restore commands from the kit's lines %q", cmds)
	}
	s := strings.Replace(string(tmpl), "# @KIT_SHELL@", shell, 1)
	return strings.Replace(s, "# @KIT_COMMANDS@", strings.Join(run, "\n"), 1)
}

// isHex reports whether s is lower-case hex.
func isHex(s string) bool {
	return strings.Trim(s, "0123456789abcdef") == ""
}
