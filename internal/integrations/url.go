package integrations

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// NormalizeURL checks an integration base URL and returns its stored form: an absolute http or
// https URL with a host, no user name or password, no query and no fragment (credentials go in
// the API key, never in the URL: design S8), and no trailing slash. A path is allowed (a service
// behind a reverse proxy at https://host/plex). Error messages do not repeat the URL.
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return "", ValidationError("url is required")
	case len(s) > MaxURLLen:
		return "", ValidationError(fmt.Sprintf("url must be at most %d characters", MaxURLLen))
	case strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return "", ValidationError("url must not contain spaces or control characters")
	case strings.ContainsAny(s, "?#"):
		return "", ValidationError("url must not contain a query (?) or fragment (#); put the API key in the API key field")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", ValidationError("url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", ValidationError("url must start with http:// or https://")
	}
	if u.User != nil {
		return "", ValidationError("url must not contain a user name or password")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", ValidationError("url must include a host name, e.g. http://plex:32400")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	if u.RawPath != "" && u.RawPath == u.Path {
		u.RawPath = ""
	}
	return u.String(), nil
}
