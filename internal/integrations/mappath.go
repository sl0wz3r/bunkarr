package integrations

import (
	"fmt"
	"path"
	"strings"
)

// Mapping is a path mapping: a path prefix as another application reports it (Plex's
// {plex, local} pairs, the *arrs' {arr, local} pairs) and the same directory inside Bunkarr.
type Mapping interface {
	// MappingPaths returns the prefix as the application sees it and as Bunkarr sees it.
	MappingPaths() (remote, local string)
}

// MappingPaths implements Mapping.
func (m PathMapping) MappingPaths() (remote, local string) { return m.Plex, m.Local }

// MapPath translates a path as another application reports it into the path inside Bunkarr,
// using the mapping with the longest prefix that matches on a path-segment boundary (/data
// matches /data and /data/movies, never /database). The result is cleaned. ok is false when no
// mapping applies or p is not absolute (design S18: such a path is unmapped, never an error).
func MapPath[M Mapping](mappings []M, p string) (local string, ok bool) {
	p = strings.TrimSpace(p)
	if !path.IsAbs(p) {
		return "", false
	}
	p = path.Clean(p)
	var bestPrefix, bestLocal string
	found := false
	for _, m := range mappings {
		remote, loc := m.MappingPaths()
		prefix, loc := cleanPath(remote), cleanPath(loc)
		if !path.IsAbs(prefix) || !path.IsAbs(loc) || !hasPathPrefix(p, prefix) {
			continue
		}
		if !found || len(prefix) > len(bestPrefix) {
			bestPrefix, bestLocal, found = prefix, loc, true
		}
	}
	if !found {
		return "", false
	}
	return path.Join(bestLocal, strings.TrimPrefix(p, bestPrefix)), true
}

// validateMappings checks a mapping list: at most MaxPathMappings pairs, both sides absolute clean
// paths, and each application-side prefix used once. app names the application side in messages
// ("Plex", "Radarr").
func validateMappings[M Mapping](mappings []M, app string) error {
	if len(mappings) > MaxPathMappings {
		return ValidationError(fmt.Sprintf("at most %d path mappings are allowed", MaxPathMappings))
	}
	seen := make(map[string]bool, len(mappings))
	for i, m := range mappings {
		remote, local := m.MappingPaths()
		if !validPath(remote) {
			return ValidationError(fmt.Sprintf("path mapping %d: the %s path must be an absolute path such as /data/movies", i+1, app))
		}
		if !validPath(local) {
			return ValidationError(fmt.Sprintf("path mapping %d: the local path must be an absolute path such as /media/movies", i+1))
		}
		if seen[remote] {
			return ValidationError(fmt.Sprintf("path mapping %d: the %s path %s is mapped twice", i+1, app, remote))
		}
		seen[remote] = true
	}
	return nil
}
