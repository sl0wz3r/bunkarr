package destinations

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// Buckets the SDK endpoint rules of rclone 1.74.1 treat specially. The --op-s3 (Outposts alias)
// names put the hardware type at len-50, a 17-character outpost id at [len-49,len-32) and "beta"
// at [len-12,len-8), so the SDK dials <bucket>.op-<id>.<endpoint host> (type o) or
// <bucket>.ec2.<endpoint host> (type e), names engines.DialHosts does not list; measured with the
// real rclone (TestRealDialHostsRefusedBuckets).
const (
	outpostsOBucket = "ox-169-254-169-254ffffffffffffffffffffbetaa--op-s3"
	outpostsEBucket = "ex0123456789abcdefffffffffffffffffffffbetaa--op-s3"
)

// specialSuffixBuckets are accepted buckets with an SDK special suffix whose requests still go to
// <bucket>.<endpoint host> (measured with rclone 1.74.1).
var specialSuffixBuckets = []string{"bunkarr--usw2-az1--x-s3", "bunkarr--usw2-az1--xa-s3", "bunkarr--table-s3"}

func s3RemoteJSON(t *testing.T, provider, bucket string, forcePathStyle bool) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"provider": provider, "endpoint": "https://example.invalid", "bucket": bucket, "forcePathStyle": forcePathStyle})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestS3OutpostsAliasBucketRefused: a bucket with the Outposts alias suffix --op-s3 is refused
// for every provider, because the host the engine would dial for it is not one netguard checks
// (S25); the names pass the naming pattern, so the refusal is the suffix rule.
func TestS3OutpostsAliasBucketRefused(t *testing.T) {
	c := remoteCheck{ctx: context.Background()}
	for _, b := range []string{outpostsOBucket, outpostsEBucket} {
		if len(b) != 50 || b[len(b)-12:len(b)-8] != "beta" || !s3BucketPattern.MatchString(b) {
			t.Fatalf("%s is not the SDK's Outposts alias form", b)
		}
		for _, p := range []struct {
			provider string
			fps      bool
		}{{"Other", false}, {"Minio", true}, {"AWS", false}, {"Cloudflare", true}, {"Wasabi", false}} {
			_, _, err := decodeRemote(s3RemoteJSON(t, p.provider, b, p.fps), engines.S3, c)
			var ve ValidationError
			if !errors.As(err, &ve) || !strings.Contains(err.Error(), "reserved prefix or suffix") {
				t.Errorf("%s bucket %s: err = %v, want the reserved suffix refusal", p.provider, b, err)
			}
		}
	}
}

// TestS3SpecialSuffixBucketsChecked: the other SDK special suffixes are accepted, and the host the
// engine dials for them, <bucket>.<endpoint host>, is one of DialHosts.
func TestS3SpecialSuffixBucketsChecked(t *testing.T) {
	c := remoteCheck{ctx: context.Background()}
	for _, b := range specialSuffixBuckets {
		r, _, err := decodeRemote(s3RemoteJSON(t, "Other", b, false), engines.S3, c)
		if err != nil {
			t.Fatalf("bucket %s: %v", b, err)
		}
		if hosts := engines.DialHosts(engines.S3, r); !slices.Contains(hosts, b+".example.invalid") {
			t.Errorf("bucket %s: DialHosts = %v, want %s.example.invalid among them", b, hosts, b)
		}
	}
}
