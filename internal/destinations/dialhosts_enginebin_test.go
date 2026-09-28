//go:build enginebin

package destinations

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/enginetest"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// dialLookupRE finds the name in a Go resolver error ("dial tcp: lookup <name> on ...").
var dialLookupRE = regexp.MustCompile(`lookup ([^\s:]+)`)

// TestRealDialHostsRefusedBuckets: every S3 bucket Bunkarr accepts makes the real rclone dial one
// of DialHosts, so netguard checks the name (S25). The Outposts alias buckets (--op-s3) make it
// dial <bucket>.op-<id>.<host> or <bucket>.ec2.<host>, whatever the provider and forcePathStyle,
// so they must be refused; the other SDK special suffixes dial <bucket>.<host> and are accepted.
// Each endpoint is under .invalid, so the dial fails at the name lookup and the error names the
// host rclone chose.
func TestRealDialHostsRefusedBuckets(t *testing.T) {
	e := enginetest.RealEnv(t)
	providers := []struct {
		provider string
		fps      bool
	}{{"Other", false}, {"Minio", true}, {"AWS", false}, {"Cloudflare", true}}
	buckets := append([]string{outpostsOBucket, outpostsEBucket}, specialSuffixBuckets...)
	secrets := engines.Secrets{Credentials: engines.Credentials{AccessKeyID: "dialhosts", SecretAccessKey: "dialhosts-not-a-secret"}}
	c := remoteCheck{ctx: context.Background()}
	for _, b := range buckets {
		for _, p := range providers {
			t.Run(p.provider+"/"+b, func(t *testing.T) {
				r := engines.S3Remote{Provider: p.provider, Endpoint: "https://example.invalid", Bucket: b, ForcePathStyle: p.fps}
				d := engines.Destination{Engine: engines.Rclone, Kind: engines.S3, Remote: engines.Remote{S3: &r}}
				env, err := rclone.Env(rclone.EnvInput{Dest: d, Secrets: secrets})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, e.RclonePath, "lsf", "--retries", "1", "--low-level-retries", "1",
					"--contimeout", "5s", "--timeout", "10s", rclone.StorageRoot(d))
				cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
				for k, v := range env {
					cmd.Env = append(cmd.Env, k+"="+v)
				}
				out, err := cmd.CombinedOutput()
				if err == nil {
					t.Fatalf("rclone lsf against %s succeeded", r.Endpoint)
				}
				m := dialLookupRE.FindStringSubmatch(string(out))
				if m == nil {
					t.Fatalf("no name lookup in rclone's output:\n%s", out)
				}
				dialed := strings.TrimSuffix(m[1], ".")
				hosts := engines.DialHosts(engines.S3, d.Remote)
				_, _, verr := decodeRemote(s3RemoteJSON(t, p.provider, b, p.fps), engines.S3, c)
				outposts := strings.HasSuffix(b, "--op-s3")
				switch {
				case verr == nil && !slices.Contains(hosts, dialed):
					t.Errorf("bucket accepted, but rclone dialed %s; DialHosts = %v", dialed, hosts)
				case outposts && verr == nil:
					t.Errorf("Outposts alias bucket accepted")
				case !outposts && verr != nil:
					t.Errorf("bucket refused: %v", verr)
				case outposts && slices.Contains(hosts, dialed):
					t.Logf("rclone dialed %s, one of DialHosts, not the Outposts form 1.74.1 dialed (still refused)", dialed)
				}
			})
		}
	}
}
