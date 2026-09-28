package destinations

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Remote validation (phase4.md §4.2). A remote is decoded with DisallowUnknownFields and every
// field is validated; no field name or value ever becomes an rclone option by itself (§4.4: the
// engines build their environment from a closed table).

// S3 providers (§4.2).
var s3Providers = []string{"AWS", "Minio", "Wasabi", "Cloudflare", "Other"}

// s3StorageClasses are the storage classes each provider accepts ("" = the provider's default).
// Classes whose objects cannot be read without a restore request (GLACIER, DEEP_ARCHIVE) are not
// offered: restic reads its index and every job reads what it verifies.
var s3StorageClasses = map[string][]string{
	"AWS":        {"", "STANDARD", "STANDARD_IA", "ONEZONE_IA", "INTELLIGENT_TIERING", "GLACIER_IR", "REDUCED_REDUNDANCY"},
	"Minio":      {"", "STANDARD", "REDUCED_REDUNDANCY"},
	"Wasabi":     {"", "STANDARD"},
	"Cloudflare": {"", "STANDARD", "STANDARD_IA"},
	"Other":      {"", "STANDARD"},
}

var (
	regionPattern   = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	s3BucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	b2BucketPattern = regexp.MustCompile(`^[A-Za-z0-9-]{6,63}$`)
	hostLabel       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// b2S3Host is a Backblaze B2 S3-compatible endpoint: the same storage as a b2 destination.
	b2S3Host = regexp.MustCompile(`^s3\.[a-z0-9-]+\.backblazeb2\.com$`)
)

// maxHostKeys is how many host keys an sftp remote may pin.
const maxHostKeys = 16

// remoteCheck is what validating a remote may need besides the JSON: a name resolver for an http
// endpoint (nil: the default resolver).
type remoteCheck struct {
	ctx    context.Context
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// requireHostKeys refuses an sftp remote without hostKeys (Create; Test answers the presented
	// keys instead, §4.5).
	requireHostKeys bool
}

// decodeRemote decodes and validates raw as the remote of kind (strictly: unknown fields are a
// ValidationError) and returns it normalized, with warnings (an http endpoint). A local kind
// accepts only an empty remote (absent, null or {}).
func decodeRemote(raw json.RawMessage, kind engines.DestKind, c remoteCheck) (engines.Remote, []string, error) {
	empty := len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
	if kind == engines.Local {
		if !empty && string(bytes.TrimSpace(raw)) != "{}" {
			return engines.Remote{}, nil, ValidationError("remote: a local destination has no remote (use target)")
		}
		return engines.Remote{}, nil, nil
	}
	if empty {
		return engines.Remote{}, nil, ValidationError(fmt.Sprintf("remote: a %s destination needs its remote", kind))
	}
	strict := func(v any) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return ValidationError("remote: " + err.Error())
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return ValidationError("remote: unexpected data after the object")
		}
		return nil
	}
	switch kind {
	case engines.SFTP:
		var r engines.SFTPRemote
		if err := strict(&r); err != nil {
			return engines.Remote{}, nil, err
		}
		n, err := normalizeSFTP(r, c.requireHostKeys)
		return engines.Remote{SFTP: n}, nil, err
	case engines.S3:
		var r engines.S3Remote
		if err := strict(&r); err != nil {
			return engines.Remote{}, nil, err
		}
		n, warnings, err := normalizeS3(r, c)
		return engines.Remote{S3: n}, warnings, err
	case engines.B2:
		var r engines.B2Remote
		if err := strict(&r); err != nil {
			return engines.Remote{}, nil, err
		}
		n, err := normalizeB2(r)
		return engines.Remote{B2: n}, nil, err
	}
	return engines.Remote{}, nil, ValidationError(fmt.Sprintf("kind %q: use local, sftp, s3 or b2", kind))
}

// parseStoredRemote decodes a destination row's remote column (already validated when it was
// saved) for its kind.
func parseStoredRemote(raw string, kind engines.DestKind) (engines.Remote, error) {
	if kind == engines.Local || !kind.Valid() {
		return engines.Remote{}, nil
	}
	var r engines.Remote
	var err error
	switch kind {
	case engines.SFTP:
		r.SFTP = &engines.SFTPRemote{}
		err = json.Unmarshal([]byte(raw), r.SFTP)
	case engines.S3:
		r.S3 = &engines.S3Remote{}
		err = json.Unmarshal([]byte(raw), r.S3)
	case engines.B2:
		r.B2 = &engines.B2Remote{}
		err = json.Unmarshal([]byte(raw), r.B2)
	}
	if err != nil {
		return engines.Remote{}, fmt.Errorf("parse remote: %w", err)
	}
	return r, nil
}

// hasControl reports whether s contains a Unicode control character (\n, \r, \t, NUL, ...).
func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

// validHost reports whether h is an RFC 1123 host name or an IP literal (without brackets), and
// returns it normalized (lower case; the canonical form of an IP address).
func validHost(h string) (string, bool) {
	if h == "" || len(h) > 253 || strings.ContainsFunc(h, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("@,*?[]!/\\#%|'\"`$;&<>(){}", r)
	}) {
		return "", false
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if ip.Zone() != "" {
			return "", false
		}
		return ip.String(), true
	}
	h = strings.ToLower(h)
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return "", false
		}
	}
	// A name whose last label is all digits would be read as an IPv4 address by some resolvers.
	if _, err := strconv.Atoi(labels[len(labels)-1]); err == nil {
		return "", false
	}
	return h, true
}

// validPrefix checks a bucket prefix or an sftp path segment list: "" or a clean relative path
// (jobs.ValidTargetPath) without control characters.
func validPrefix(field, p string) error {
	if p == "" {
		return nil
	}
	if hasControl(p) || !jobs.ValidTargetPath(p) {
		return ValidationError(fmt.Sprintf("remote.%s %q: use a clean relative path without control characters (or nothing)", field, p))
	}
	return nil
}

// normalizeSFTP validates an sftp remote (§4.2) and returns it normalized: the host lower-cased,
// port 22 when 0, each host key parsed, checked against its type and re-encoded.
func normalizeSFTP(r engines.SFTPRemote, requireKeys bool) (*engines.SFTPRemote, error) {
	host, ok := validHost(r.Host)
	if !ok {
		return nil, ValidationError(fmt.Sprintf("remote.host %q: use a host name or an IP address (no user, port or spaces)", r.Host))
	}
	r.Host = host
	if r.Port == 0 {
		r.Port = 22
	}
	if r.Port < 1 || r.Port > 65535 {
		return nil, ValidationError(fmt.Sprintf("remote.port %d is out of range (1-65535)", r.Port))
	}
	if r.User == "" || strings.ContainsFunc(r.User, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) {
		return nil, ValidationError("remote.user: a user name without spaces or control characters is required")
	}
	p := r.Path
	switch {
	case p == "" || p == "." || p == "/":
		return nil, ValidationError("remote.path: a directory is required (absolute, or relative to the login directory)")
	case hasControl(p) || strings.ContainsRune(p, 0):
		return nil, ValidationError("remote.path contains control characters")
	case path.Clean(p) != p:
		return nil, ValidationError(fmt.Sprintf("remote.path %q is not clean (%q)", p, path.Clean(p)))
	case p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/.."):
		return nil, ValidationError(fmt.Sprintf("remote.path %q may not contain ..", p))
	}
	keys, err := normalizeHostKeys(r.HostKeys)
	if err != nil {
		return nil, err
	}
	if requireKeys && len(keys) == 0 {
		return nil, ValidationError("remote.hostKeys: pin the server's host keys first (POST /destinations/sftp/hostkeys, then confirm the fingerprints)")
	}
	r.HostKeys = keys
	return &r, nil
}

// normalizeHostKeys parses each pinned key (ssh.ParsePublicKey), checks its type and re-encodes it;
// a duplicate key is dropped. A value with whitespace or a control character is refused before it
// is decoded (base64 decoding would skip a line break).
func normalizeHostKeys(in []engines.HostKey) ([]engines.HostKey, error) {
	if len(in) > maxHostKeys {
		return nil, ValidationError(fmt.Sprintf("remote.hostKeys: at most %d keys", maxHostKeys))
	}
	out := []engines.HostKey{}
	for i, k := range in {
		bad := func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }
		if strings.ContainsFunc(k.Type, bad) || strings.ContainsFunc(k.Key, bad) || k.Type == "" || k.Key == "" {
			return nil, ValidationError(fmt.Sprintf("remote.hostKeys[%d]: a type and a base64 key without spaces or line breaks are required", i))
		}
		pk, err := rclone.ParseHostKey(k)
		if err != nil {
			return nil, ValidationError(fmt.Sprintf("remote.hostKeys[%d]: %v", i, err))
		}
		n := engines.HostKey{Type: pk.Type(), Key: base64.StdEncoding.EncodeToString(pk.Marshal())}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// normalizeS3 validates an s3 remote (§4.2) and returns it normalized (the endpoint as
// scheme://host[:port], the CA certificate re-encoded), with a warning for an http endpoint.
func normalizeS3(r engines.S3Remote, c remoteCheck) (*engines.S3Remote, []string, error) {
	var warnings []string
	if !slices.Contains(s3Providers, r.Provider) {
		return nil, nil, ValidationError(fmt.Sprintf("remote.provider %q: use %s", r.Provider, strings.Join(s3Providers, ", ")))
	}
	if r.Region != "" && !regionPattern.MatchString(r.Region) {
		return nil, nil, ValidationError(fmt.Sprintf("remote.region %q: lower-case letters, digits and hyphens only", r.Region))
	}
	if r.Endpoint == "" {
		if r.Provider != "AWS" {
			return nil, nil, ValidationError(fmt.Sprintf("remote.endpoint: a %s destination needs its endpoint URL", r.Provider))
		}
	} else {
		ep, warning, err := normalizeEndpoint(r.Endpoint, c)
		if err != nil {
			return nil, nil, err
		}
		r.Endpoint = ep
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if err := checkS3Bucket(r.Bucket); err != nil {
		return nil, nil, err
	}
	if err := validPrefix("prefix", r.Prefix); err != nil {
		return nil, nil, err
	}
	if !slices.Contains(s3StorageClasses[r.Provider], r.StorageClass) {
		return nil, nil, ValidationError(fmt.Sprintf("remote.storageClass %q is not offered for %s (use %s)", r.StorageClass, r.Provider,
			strings.Join(slices.DeleteFunc(slices.Clone(s3StorageClasses[r.Provider]), func(s string) bool { return s == "" }), ", ")))
	}
	if r.CACert != "" {
		cert, err := normalizeCACert(r.CACert)
		if err != nil {
			return nil, nil, err
		}
		r.CACert = cert
	}
	return &r, warnings, nil
}

// normalizeEndpoint checks an S3 endpoint URL: https without userinfo, query, fragment or path
// (http only for a loopback or private address, with a warning).
func normalizeEndpoint(raw string, c remoteCheck) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || hasControl(raw) {
		return "", "", ValidationError(fmt.Sprintf("remote.endpoint %q is not a URL", raw))
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", "", ValidationError("remote.endpoint: no user, password, query or fragment (credentials go in credentials)")
	}
	if u.Path != "" && u.Path != "/" {
		return "", "", ValidationError("remote.endpoint: no path (the bucket and prefix are fields of their own)")
	}
	host, ok := validHost(u.Hostname())
	if !ok {
		return "", "", ValidationError(fmt.Sprintf("remote.endpoint: %q is not a host name or IP address", u.Hostname()))
	}
	hostport := host
	if strings.Contains(host, ":") {
		hostport = "[" + host + "]"
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", "", ValidationError(fmt.Sprintf("remote.endpoint: port %q is out of range", p))
		}
		hostport += ":" + strconv.Itoa(n)
	}
	switch u.Scheme {
	case "https":
		return "https://" + hostport, "", nil
	case "http":
		if !privateHost(c, host) {
			return "", "", ValidationError("remote.endpoint: use https (http is accepted only for a loopback or private address)")
		}
		return "http://" + hostport, fmt.Sprintf("the endpoint %s is not encrypted (http); use it only on a private network", host), nil
	}
	return "", "", ValidationError(fmt.Sprintf("remote.endpoint: scheme %q: use https", u.Scheme))
}

// privateHost reports whether host is, or resolves only to, loopback or private addresses.
func privateHost(c remoteCheck, host string) bool {
	private := func(a netip.Addr) bool { a = a.Unmap(); return a.IsLoopback() || a.IsPrivate() }
	if ip, err := netip.ParseAddr(host); err == nil {
		return private(ip)
	}
	lookup := c.lookup
	if lookup == nil {
		lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
		}
	}
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	addrs, err := lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !private(a) {
			return false
		}
	}
	return true
}

// checkS3Bucket applies the S3 bucket naming rules. The Outposts alias suffix --op-s3 is refused
// as well: the SDK endpoint rules rclone 1.74.1 uses send such a bucket to
// <bucket>.op-<outpost id>.<endpoint host> or <bucket>.ec2.<endpoint host> (or an s3-outposts
// host), names engines.DialHosts does not list, so netguard would not check them (S25).
func checkS3Bucket(b string) error {
	bad := func(why string) error { return ValidationError(fmt.Sprintf("remote.bucket %q: %s", b, why)) }
	switch {
	case !s3BucketPattern.MatchString(b):
		return bad("3-63 lower-case letters, digits, dots and hyphens, starting and ending with a letter or digit")
	case strings.Contains(b, "..") || strings.Contains(b, ".-") || strings.Contains(b, "-."):
		return bad("no two adjacent dots, and no dot next to a hyphen")
	case strings.HasPrefix(b, "xn--") || strings.HasPrefix(b, "sthree-") || strings.HasSuffix(b, "-s3alias") ||
		strings.HasSuffix(b, "--ol-s3") || strings.HasSuffix(b, "--op-s3"):
		return bad("a reserved prefix or suffix")
	}
	if _, err := netip.ParseAddr(b); err == nil {
		return bad("not an IP address")
	}
	return nil
}

// normalizeCACert checks that pemText holds exactly one certificate and re-encodes it.
func normalizeCACert(pemText string) (string, error) {
	block, rest := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return "", ValidationError("remote.caCert: one PEM certificate (-----BEGIN CERTIFICATE-----) is required")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", ValidationError(fmt.Sprintf("remote.caCert: %v", err))
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})), nil
}

// normalizeB2 validates a b2 remote (§4.2).
func normalizeB2(r engines.B2Remote) (*engines.B2Remote, error) {
	if !b2BucketPattern.MatchString(r.Bucket) || strings.HasPrefix(strings.ToLower(r.Bucket), "b2-") {
		return nil, ValidationError(fmt.Sprintf("remote.bucket %q: 6-63 letters, digits and hyphens, not starting with b2-", r.Bucket))
	}
	if err := validPrefix("prefix", r.Prefix); err != nil {
		return nil, err
	}
	return &r, nil
}

// displayTarget is a remote's display location, stored as destinations.target (§4.2):
// sftp://user@host:port/path, s3:<endpoint>/<bucket>/<prefix>, b2:<bucket>/<prefix>.
func displayTarget(kind engines.DestKind, r engines.Remote) string {
	join := func(parts ...string) string {
		return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "/")
	}
	switch {
	case kind == engines.SFTP && r.SFTP != nil:
		host := r.SFTP.Host
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		p := r.SFTP.Path
		if !strings.HasPrefix(p, "/") {
			p = "/~/" + p
		}
		return fmt.Sprintf("sftp://%s@%s:%d%s", r.SFTP.User, host, r.SFTP.Port, p)
	case kind == engines.S3 && r.S3 != nil:
		ep := r.S3.Endpoint
		if ep == "" {
			ep = "https://" + awsHost(r.S3.Region)
		}
		return "s3:" + join(ep, r.S3.Bucket, r.S3.Prefix)
	case kind == engines.B2 && r.B2 != nil:
		return "b2:" + join(r.B2.Bucket, r.B2.Prefix)
	}
	return ""
}

// awsHost is the endpoint host of AWS S3 in region ("us-east-1" when empty).
func awsHost(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return "s3." + region + ".amazonaws.com"
}

// location is a remote's storage location for the overlap rule (§4.2, the S4 rule for remotes):
// the store (an S3 endpoint host, "b2" for Backblaze B2 whichever way it is reached, or an SFTP
// host and port), the bucket, and the prefix or path inside it.
type location struct {
	store, bucket, prefix string
}

// remoteLocation returns the storage location of a remote of kind (ok false for a local kind).
func remoteLocation(kind engines.DestKind, r engines.Remote) (location, bool) {
	switch {
	case kind == engines.SFTP && r.SFTP != nil:
		return location{store: "sftp:" + net.JoinHostPort(r.SFTP.Host, strconv.Itoa(r.SFTP.Port)), prefix: strings.Trim(r.SFTP.Path, "/"),
			bucket: map[bool]string{true: "/", false: "~"}[strings.HasPrefix(r.SFTP.Path, "/")]}, true
	case kind == engines.B2 && r.B2 != nil:
		return location{store: "b2", bucket: strings.ToLower(r.B2.Bucket), prefix: r.B2.Prefix}, true
	case kind == engines.S3 && r.S3 != nil:
		host := awsHost(r.S3.Region)
		if r.S3.Endpoint != "" {
			if u, err := url.Parse(r.S3.Endpoint); err == nil {
				host = u.Hostname()
				if p := u.Port(); p != "" && !(u.Scheme == "https" && p == "443") && !(u.Scheme == "http" && p == "80") {
					host += ":" + p
				}
			}
		}
		if b2S3Host.MatchString(host) {
			return location{store: "b2", bucket: strings.ToLower(r.S3.Bucket), prefix: r.S3.Prefix}, true
		}
		return location{store: "s3:" + host, bucket: r.S3.Bucket, prefix: r.S3.Prefix}, true
	}
	return location{}, false
}

// overlapsWith reports whether two storage locations overlap: the same store and bucket, with one
// prefix equal to or inside the other ("" holds every prefix).
func (l location) overlapsWith(o location) bool {
	if l.store != o.store || l.bucket != o.bucket {
		return false
	}
	within := func(inner, outer string) bool { return outer == "" || strings.HasPrefix(inner, outer+"/") }
	return l.prefix == o.prefix || within(l.prefix, o.prefix) || within(o.prefix, l.prefix)
}
