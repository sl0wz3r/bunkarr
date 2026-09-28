//go:build enginebin

package engines_test

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

// lookupRE finds the name in a Go resolver error ("dial tcp: lookup <name> on ...: no such host").
var lookupRE = regexp.MustCompile(`lookup ([^\s:]+)`)

// TestRealDialHosts: the host the real rclone dials for an S3 remote is one of DialHosts, so
// netguard checks it (S25, SE-12). Each endpoint is under .invalid, so the dial fails at the name
// lookup and the error names the host rclone chose, whatever style it picked. rclone 1.74.1 picks
// path style for a bucket with a dot although forcePathStyle is false, and virtual-hosted style
// for AWS and Wasabi although forcePathStyle is true; restic reaches S3 through rclone too.
func TestRealDialHosts(t *testing.T) {
	e := enginetest.RealEnv(t)
	tests := []struct {
		name   string
		remote engines.S3Remote
		want   string // measured with rclone 1.74.1
	}{
		{"bucket with a dot", engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid", Bucket: "a.b"}, "example.invalid"},
		{"bucket with a dot over http", engines.S3Remote{Provider: "Other", Endpoint: "http://example.invalid:9000", Bucket: "a.b"}, "example.invalid"},
		{"AWS endpoint, forcePathStyle", engines.S3Remote{Provider: "AWS", Endpoint: "https://example.invalid", Bucket: "abc", ForcePathStyle: true}, "abc.example.invalid"},
		{"Wasabi, forcePathStyle", engines.S3Remote{Provider: "Wasabi", Endpoint: "https://example.invalid", Bucket: "abc", ForcePathStyle: true}, "abc.example.invalid"},
		{"Minio, forcePathStyle", engines.S3Remote{Provider: "Minio", Endpoint: "https://example.invalid", Bucket: "abc", ForcePathStyle: true}, "example.invalid"},
		{"Other, virtual-hosted", engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid", Bucket: "abc"}, "abc.example.invalid"},
		{"Cloudflare, forcePathStyle", engines.S3Remote{Provider: "Cloudflare", Endpoint: "https://example.invalid", Bucket: "abc", ForcePathStyle: true}, "example.invalid"},
		// SDK special suffixes that still dial <bucket>.<endpoint host>; the Outposts alias suffix
		// --op-s3 does not and is refused by destinations (TestRealDialHostsRefusedBuckets there).
		{"S3 Express suffix", engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid", Bucket: "bunkarr--usw2-az1--x-s3"}, "bunkarr--usw2-az1--x-s3.example.invalid"},
		{"S3 Express local zone suffix", engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid", Bucket: "bunkarr--usw2-az1--xa-s3"}, "bunkarr--usw2-az1--xa-s3.example.invalid"},
		{"table suffix", engines.S3Remote{Provider: "Other", Endpoint: "https://example.invalid", Bucket: "bunkarr--table-s3"}, "bunkarr--table-s3.example.invalid"},
	}
	secrets := engines.Secrets{Credentials: engines.Credentials{AccessKeyID: "dialhosts", SecretAccessKey: "dialhosts-not-a-secret"}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.remote
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
			m := lookupRE.FindStringSubmatch(string(out))
			if m == nil {
				t.Fatalf("no name lookup in rclone's output:\n%s", out)
			}
			dialed := strings.TrimSuffix(m[1], ".")
			hosts := engines.DialHosts(engines.S3, d.Remote)
			if !slices.Contains(hosts, dialed) {
				t.Errorf("rclone dialed %s; DialHosts = %v", dialed, hosts)
			}
			if dialed != tc.want {
				t.Logf("rclone dialed %s, not %s as 1.74.1 did (still checked)", dialed, tc.want)
			}
		})
	}
}
