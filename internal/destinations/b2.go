package destinations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// b2AuthorizeURL is Backblaze B2's b2_authorize_account (API v2, which reports allowed.bucketId).
const b2AuthorizeURL = "https://" + engines.B2APIHost + "/b2api/v2/b2_authorize_account"

// b2Timeout bounds the authorization call.
const b2Timeout = 30 * time.Second

// b2Authorization is the part of b2_authorize_account's answer Bunkarr reads. The authorization
// token it also carries is never decoded.
type b2Authorization struct {
	APIURL  string `json:"apiUrl"`
	Allowed struct {
		BucketID   *string `json:"bucketId"`
		BucketName *string `json:"bucketName"`
	} `json:"allowed"`
}

// authorizeB2 checks a B2 application key with b2_authorize_account over HTTPS through the
// outbound guard, the key only in the Authorization header (§4.5). It refuses a key that B2
// rejects or that is restricted to another bucket, checks the API host B2 answers with, and warns
// when the key is not restricted to the bucket: whoever obtains Bunkarr's /config could then
// erase every bucket of the account (§16).
func (s *Store) authorizeB2(ctx context.Context, bucket string, c engines.Credentials) ([]string, error) {
	if err := s.checkHost(ctx, engines.B2APIHost); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, b2Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.b2AuthURL, nil)
	if err != nil {
		return nil, fmt.Errorf("b2 authorization: %w", err)
	}
	req.SetBasicAuth(c.KeyID, c.ApplicationKey)
	req.Header.Set("User-Agent", version.UserAgent())
	client := s.opts.HTTPClient
	if client == nil {
		client = &http.Client{Transport: netguard.NewTransport(), Timeout: b2Timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL holds no secret; the error text names the host only.
		return nil, ValidationError(fmt.Sprintf("could not reach Backblaze B2 to check the application key: %v", redactErr(err, []string{c.ApplicationKey})))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("b2 authorization: read the answer: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ValidationError("Backblaze B2 rejected the application key (check keyId and applicationKey)")
	case resp.StatusCode != http.StatusOK:
		return nil, ValidationError(fmt.Sprintf("Backblaze B2 refused the key check: HTTP %d", resp.StatusCode))
	}
	var a b2Authorization
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, ValidationError("Backblaze B2 answered the key check with something that is not its JSON")
	}
	if u, err := url.Parse(a.APIURL); err == nil && u.Hostname() != "" {
		if err := s.checkHost(ctx, u.Hostname()); err != nil {
			return nil, err
		}
	}
	if a.Allowed.BucketName != nil && *a.Allowed.BucketName != bucket {
		return nil, ValidationError(fmt.Sprintf("the B2 application key is restricted to bucket %q, not %q", *a.Allowed.BucketName, bucket))
	}
	if a.Allowed.BucketID == nil {
		return []string{fmt.Sprintf("the B2 application key is not restricted to bucket %q: whoever obtains Bunkarr's /config could erase every bucket of the account; create a key for this bucket only", bucket)}, nil
	}
	return nil, nil
}
