package notify

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// configKeyRE is the accepted Apprise configuration key (Apprise itself allows up to 128).
var configKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// urlStartRE finds where an Apprise URL starts: a scheme and "://" at the start of the list or
// after a separator (whitespace or commas). Like Apprise's own parser, a comma that is not
// followed by a new scheme belongs to the current URL (e.g. mailto://…?to=a@x,b@y).
var urlStartRE = regexp.MustCompile(`(?:^|[\s,]+)[A-Za-z0-9][A-Za-z0-9+.-]{0,31}://`)

// endpoint is the validated delivery part of an Input.
type endpoint struct {
	apiURL    string
	configKey string
	// urls is "" in stateful mode, or when the stored URLs are kept.
	urls string
}

// values is a validated Input.
type values struct {
	endpoint
	name                                     string
	enabled, onFailure, onWarning, onSuccess bool
}

// normalize validates in. cur is the stored notification for an update (nil on create): its
// flags are the defaults for nil booleans and its stored URLs satisfy stateless mode.
func normalize(in Input, cur *Notification) (values, error) {
	name, err := validName(in.Name)
	if err != nil {
		return values{}, err
	}
	ep, err := validEndpoint(in, cur)
	if err != nil {
		return values{}, err
	}
	v := values{endpoint: ep, name: name, enabled: true, onFailure: true, onWarning: true}
	if cur != nil {
		v.enabled, v.onFailure, v.onWarning, v.onSuccess = cur.Enabled, cur.OnFailure, cur.OnWarning, cur.OnSuccess
	}
	for _, f := range []struct {
		in  *bool
		out *bool
	}{{in.Enabled, &v.enabled}, {in.OnFailure, &v.onFailure}, {in.OnWarning, &v.onWarning}, {in.OnSuccess, &v.onSuccess}} {
		if f.in != nil {
			*f.out = *f.in
		}
	}
	return v, nil
}

func validName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		return "", ValidationError("name is required")
	case !utf8.ValidString(name):
		return "", ValidationError("name must be valid UTF-8")
	case utf8.RuneCountInString(name) > MaxNameLen:
		return "", ValidationError(fmt.Sprintf("name must be at most %d characters", MaxNameLen))
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", ValidationError("name must not contain control characters")
	}
	return name, nil
}

// validEndpoint checks the kind, API URL and mode of in. cur is the stored notification (nil for
// a new one): in stateless mode an empty in.URLs keeps its stored URLs, but only with its stored
// API URL. The URLs are bound to the Apprise API they were saved with, so a caller who does not
// know them cannot have them posted to another host (S8).
func validEndpoint(in Input, cur *Notification) (endpoint, error) {
	if in.Kind != "" && in.Kind != KindApprise {
		return endpoint{}, ValidationError(fmt.Sprintf("kind must be %q", KindApprise))
	}
	apiURL, err := validAPIURL(in.APIURL)
	if err != nil {
		return endpoint{}, err
	}
	key := strings.TrimSpace(in.ConfigKey)
	urls := strings.TrimSpace(in.URLs)
	if key != "" {
		if !configKeyRE.MatchString(key) {
			return endpoint{}, ValidationError("configKey must be 1-64 letters, digits, '_' or '-'")
		}
		if urls != "" {
			return endpoint{}, ValidationError("set either urls (stateless mode) or configKey (stateful mode), not both")
		}
		return endpoint{apiURL: apiURL, configKey: key}, nil
	}
	if urls == "" {
		switch {
		case cur == nil || !cur.HasURLs:
			return endpoint{}, ValidationError("urls are required unless configKey is set")
		case apiURL != cur.APIURL:
			return endpoint{}, ValidationError("urls are required when apiUrl changes: the saved Apprise URLs are only sent to " +
				"the Apprise API they were saved with, so enter them again")
		}
		return endpoint{apiURL: apiURL}, nil
	}
	if err := validURLs(urls); err != nil {
		return endpoint{}, err
	}
	return endpoint{apiURL: apiURL, urls: urls}, nil
}

// validAPIURL accepts an absolute http(s) URL without credentials, query or fragment and returns
// it without trailing slashes. Messages never echo the input (it may hold a password).
func validAPIURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ValidationError("apiUrl is required")
	}
	if len(s) > MaxAPIURLLen {
		return "", ValidationError(fmt.Sprintf("apiUrl must be at most %d bytes", MaxAPIURLLen))
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", ValidationError("apiUrl is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", ValidationError("apiUrl must start with http:// or https://")
	case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return "", ValidationError("apiUrl must be an absolute URL with a host, e.g. http://apprise:8000")
	case u.User != nil:
		return "", ValidationError("apiUrl must not contain a user name or password")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(s, "?#"):
		return "", ValidationError("apiUrl must not contain a query or fragment")
	}
	return strings.TrimRight(s, "/"), nil
}

// validURLs checks an Apprise URL list without echoing it.
func validURLs(urls string) error {
	switch {
	case len(urls) > MaxURLsLen:
		return ValidationError(fmt.Sprintf("urls must be at most %d bytes", MaxURLsLen))
	case !utf8.ValidString(urls):
		return ValidationError("urls must be valid UTF-8")
	case strings.ContainsFunc(urls, func(r rune) bool {
		return unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'
	}):
		return ValidationError("urls must not contain control characters")
	}
	loc := urlStartRE.FindStringIndex(urls)
	if loc == nil {
		return ValidationError("urls must contain at least one Apprise URL (scheme://...)")
	}
	if loc[0] != 0 {
		return ValidationError("urls must be Apprise URLs (scheme://...) separated by commas or spaces")
	}
	return nil
}

// splitURLs returns the individual URLs of an Apprise URL list, split the way Apprise splits
// them. Text before the first URL is ignored.
func splitURLs(urls string) []string {
	locs := urlStartRE.FindAllStringIndex(urls, -1)
	out := make([]string, 0, len(locs))
	for i, loc := range locs {
		end := len(urls)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if u := strings.Trim(urls[loc[0]:end], " \t\r\n,"); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// redactURLs returns s with the Apprise URL list urls, and each URL in it, redacted, without
// registering them (for URLs that are only held for one request: the Test form's).
func redactURLs(s, urls string) string {
	return logging.RedactValues(s, append(splitURLs(urls), strings.TrimSpace(urls))...)
}

// holdURLs makes the Apprise URL list notification id stores ("" for none), and each URL in it,
// the secret values its row holds in the redaction registry, so they are redacted wherever they
// would appear in logs or messages, and the URLs it held before are released. URLs that are not
// stored are not held (the registry is process-wide): see redactURLs.
func holdURLs(id int64, urls string) {
	urls = strings.TrimSpace(urls)
	if urls == "" {
		logging.SetSecrets(secretOwner(id))
		return
	}
	logging.SetSecrets(secretOwner(id), append(splitURLs(urls), urls)...)
}
