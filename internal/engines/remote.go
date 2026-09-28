package engines

import (
	"encoding/json"
	"net"
	"net/url"
	"strings"
)

// HostKey is one pinned SFTP host key (§4.6): its type ("ssh-ed25519", "ecdsa-sha2-nistp256",
// "rsa-sha2-512" keys report "ssh-rsa") and the key in base64 SSH wire format. destinations stores
// it only after ssh.ParsePublicKey succeeded and the type matched.
type HostKey struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// SFTPRemote is the non-secret location of an sftp destination (§4.2).
type SFTPRemote struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	// Path is absolute, or relative to the login directory.
	Path     string    `json:"path"`
	HostKeys []HostKey `json:"hostKeys"`
}

// S3Remote is the non-secret location of an s3 destination (§4.2).
type S3Remote struct {
	// Provider is AWS, Minio, Wasabi, Cloudflare or Other.
	Provider string `json:"provider"`
	// Endpoint is an https URL ("" for AWS: the region's endpoint).
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	StorageClass   string `json:"storageClass"`
	ForcePathStyle bool   `json:"forcePathStyle"`
	// CACert is one PEM certificate for a self-signed endpoint ("" = the system roots).
	CACert string `json:"caCert"`
}

// B2Remote is the non-secret location of a b2 destination (§4.2).
type B2Remote struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
}

// Remote is a destination's `remote` column: the member of its kind is set, the others are nil
// (all nil for local). Decoding and validation belong to internal/destinations.
type Remote struct {
	SFTP *SFTPRemote
	S3   *S3Remote
	B2   *B2Remote
}

// MarshalJSON writes the set member's object ({} when none is set), the normalized form stored
// in destinations.remote.
func (r Remote) MarshalJSON() ([]byte, error) {
	switch {
	case r.SFTP != nil:
		v := *r.SFTP
		if v.HostKeys == nil {
			v.HostKeys = []HostKey{}
		}
		return json.Marshal(v)
	case r.S3 != nil:
		return json.Marshal(r.S3)
	case r.B2 != nil:
		return json.Marshal(r.B2)
	default:
		return []byte("{}"), nil
	}
}

// B2APIHost is the host a B2 remote first dials (b2_authorize_account); the API host it returns
// is checked by the caller.
const B2APIHost = "api.backblazeb2.com"

// DialHosts returns the host names the engine dials for a remote of kind: the names netguard
// checks when the destination is saved and at each job start (S25). s3: the endpoint host and,
// unless it is an IP literal, <bucket>.<endpoint host>, whatever forcePathStyle says, because
// rclone decides the style itself (1.74.1: path style for a bucket with a dot or an IP endpoint,
// virtual-hosted style forced for AWS and Wasabi even with forcePathStyle); for AWS without an
// endpoint s3.<region>.amazonaws.com and <bucket>.s3.<region>.amazonaws.com (region us-east-1
// when empty); b2: api.backblazeb2.com; sftp: the host. The Outposts forms the SDK builds for a
// bucket ending in --op-s3 (<bucket>.op-<id>.<host>, <bucket>.ec2.<host>) are not listed:
// destinations refuses that suffix. A local kind, a missing member or an endpoint that does not
// parse gives nil. Checking a name the engine does not dial is harmless: a name that does not
// resolve passes netguard.
func DialHosts(kind DestKind, r Remote) []string {
	switch kind {
	case SFTP:
		if r.SFTP == nil || r.SFTP.Host == "" {
			return nil
		}
		return []string{strings.Trim(r.SFTP.Host, "[]")}
	case B2:
		if r.B2 == nil {
			return nil
		}
		return []string{B2APIHost}
	case S3:
		if r.S3 == nil {
			return nil
		}
		s := r.S3
		if s.Endpoint == "" {
			if s.Provider != "AWS" {
				return nil
			}
			region := s.Region
			if region == "" {
				region = "us-east-1"
			}
			host := "s3." + region + ".amazonaws.com"
			return []string{host, s.Bucket + "." + host}
		}
		u, err := url.Parse(s.Endpoint)
		if err != nil || u.Hostname() == "" {
			return nil
		}
		host := u.Hostname()
		if net.ParseIP(host) != nil || s.Bucket == "" {
			return []string{host}
		}
		return []string{host, s.Bucket + "." + host}
	}
	return nil
}
