package integrations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"strings"
)

// MaxPathMappings is the most path mappings a Plex integration may have.
const MaxPathMappings = 64

// PlexSettings is the settings object of a Plex integration (design §7).
type PlexSettings struct {
	// DataPath is the "Plex Media Server" data directory as Bunkarr sees it (mounted read-only),
	// e.g. /plex. Required for Plex DB backups; "" otherwise.
	DataPath string `json:"dataPath"`
	// PathMappings translate the library locations Plex reports into paths inside Bunkarr.
	PathMappings []PathMapping `json:"pathMappings"`
	// Backup configures the scheduled Plex DB backup.
	Backup PlexBackup `json:"backup"`
	// Index configures the Plex library index (design §6.3). Absent (nil) is the default:
	// disabled; IndexSettings returns it with the defaults applied.
	Index *PlexIndex `json:"index,omitempty"`
}

// Defaults of the Plex library index (design §4.2, §12.3).
const (
	// DefaultPlexIndexCron is the index refresh schedule of an enabled index without one.
	DefaultPlexIndexCron = "0 1 * * *"
	// DefaultPlexIndexStaleAfterHours is how long the index stays fresh after a complete refresh.
	DefaultPlexIndexStaleAfterHours = 72
)

// PlexIndex configures the Plex library index: sections, items and files, read by refresh jobs
// for the Tautulli and Maintainerr joins and the Plex-based tier facts.
type PlexIndex struct {
	Enabled bool `json:"enabled"`
	// Cron is a 5-field cron expression; an enabled index without one gets DefaultPlexIndexCron.
	Cron string `json:"cron"`
	// StaleAfterHours (1-720, default 72): the index's facts are unknown this long after its last
	// complete refresh (design D15).
	StaleAfterHours int `json:"staleAfterHours"`
}

// IndexSettings returns the library index settings with the defaults applied (disabled when
// absent).
func (s PlexSettings) IndexSettings() PlexIndex {
	if s.Index == nil {
		return PlexIndex{StaleAfterHours: DefaultPlexIndexStaleAfterHours}
	}
	return *s.Index
}

// PathMapping maps a path prefix as Plex sees it to the same directory as Bunkarr sees it.
type PathMapping struct {
	Plex  string `json:"plex"`
	Local string `json:"local"`
}

// PlexBackup is the Plex DB backup schedule of an integration.
type PlexBackup struct {
	// DestinationID is the destination the backups are written to (0 = none chosen).
	DestinationID int64 `json:"destinationId"`
	// Cron is a 5-field cron expression. Its syntax is validated by the caller
	// (jobqueue.ValidateCron); Validate only requires it when the backup is enabled.
	Cron    string `json:"cron"`
	Enabled bool   `json:"enabled"`
}

// ParsePlexSettings decodes a Plex settings document ("" and null are the zero settings) and
// normalizes it: surrounding spaces trimmed and paths cleaned (/data/movies/ → /data/movies).
// Unknown fields are ignored. It does not validate; call Validate.
func ParsePlexSettings(raw json.RawMessage) (PlexSettings, error) {
	var s PlexSettings
	if t := bytes.TrimSpace(raw); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		if err := json.Unmarshal(t, &s); err != nil {
			return PlexSettings{}, ValidationError("plex settings are not valid: " + jsonProblem(err))
		}
	}
	s.DataPath = cleanPath(s.DataPath)
	for i := range s.PathMappings {
		s.PathMappings[i].Plex = cleanPath(s.PathMappings[i].Plex)
		s.PathMappings[i].Local = cleanPath(s.PathMappings[i].Local)
	}
	if s.PathMappings == nil {
		s.PathMappings = []PathMapping{}
	}
	s.Backup.Cron = strings.TrimSpace(s.Backup.Cron)
	if s.Index != nil {
		s.Index.Cron = strings.TrimSpace(s.Index.Cron)
		if s.Index.Enabled && s.Index.Cron == "" {
			s.Index.Cron = DefaultPlexIndexCron
		}
		if s.Index.StaleAfterHours == 0 {
			s.Index.StaleAfterHours = DefaultPlexIndexStaleAfterHours
		}
	}
	return s, nil
}

// jsonProblem describes a decoding error without quoting the input.
func jsonProblem(err error) string {
	switch e := err.(type) {
	case *json.SyntaxError:
		return "malformed JSON"
	case *json.UnmarshalTypeError:
		if e.Field != "" && e.Type != nil {
			return fmt.Sprintf("%s must be %s", e.Field, kindWord(e.Type.Kind()))
		}
		return "settings must be a JSON object"
	}
	return "malformed JSON"
}

// kindWord names a JSON value kind for a user-facing message.
func kindWord(k reflect.Kind) string {
	switch k {
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Struct, reflect.Map:
		return "an object"
	case reflect.Bool:
		return "true or false"
	case reflect.String:
		return "a string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	}
	return "a different type"
}

// cleanPath trims spaces and cleans a non-empty path; relative paths stay relative (Validate
// rejects them).
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

// validPath reports whether p is absolute and clean.
func validPath(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && !strings.ContainsRune(p, 0)
}

// Validate checks the settings: DataPath and every mapping are absolute, clean paths; mappings are
// unique by their Plex prefix; an enabled backup has a data path, a destination and a cron
// expression.
func (s PlexSettings) Validate() error {
	if s.DataPath != "" && !validPath(s.DataPath) {
		return ValidationError("dataPath must be an absolute path such as /plex")
	}
	if len(s.PathMappings) > MaxPathMappings {
		return ValidationError(fmt.Sprintf("at most %d path mappings are allowed", MaxPathMappings))
	}
	seen := make(map[string]bool, len(s.PathMappings))
	for i, m := range s.PathMappings {
		if !validPath(m.Plex) {
			return ValidationError(fmt.Sprintf("path mapping %d: the Plex path must be an absolute path such as /data/movies", i+1))
		}
		if !validPath(m.Local) {
			return ValidationError(fmt.Sprintf("path mapping %d: the local path must be an absolute path such as /media/movies", i+1))
		}
		if seen[m.Plex] {
			return ValidationError(fmt.Sprintf("path mapping %d: the Plex path %s is mapped twice", i+1, m.Plex))
		}
		seen[m.Plex] = true
	}
	if ix := s.Index; ix != nil {
		if ix.StaleAfterHours < MinStaleAfterHours || ix.StaleAfterHours > MaxStaleAfterHours {
			return ValidationError(fmt.Sprintf("index.staleAfterHours must be %d to %d", MinStaleAfterHours, MaxStaleAfterHours))
		}
		if ix.Enabled {
			if err := validateCron(ix.Cron); err != nil {
				return ValidationError("index.cron is not a valid schedule: " + err.Error())
			}
		}
	}
	if s.Backup.DestinationID < 0 {
		return ValidationError("backup.destinationId must be a destination id")
	}
	if s.Backup.Enabled {
		switch {
		case s.DataPath == "":
			return ValidationError("scheduled Plex DB backups need the Plex data path (dataPath)")
		case s.Backup.DestinationID == 0:
			return ValidationError("scheduled Plex DB backups need a destination")
		case s.Backup.Cron == "":
			return ValidationError("scheduled Plex DB backups need a schedule (cron)")
		}
	}
	return nil
}

// MapPath translates a path as Plex sees it into the path inside Bunkarr, using the mapping with
// the longest Plex prefix that matches on a path-segment boundary (/data matches /data and
// /data/movies, never /database). The result is cleaned. ok is false when no mapping applies or
// plexPath is not absolute.
func (s PlexSettings) MapPath(plexPath string) (local string, ok bool) {
	p := strings.TrimSpace(plexPath)
	if !path.IsAbs(p) {
		return "", false
	}
	p = path.Clean(p)
	var bestPrefix, bestLocal string
	found := false
	for _, m := range s.PathMappings {
		prefix, loc := cleanPath(m.Plex), cleanPath(m.Local)
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

// hasPathPrefix reports whether prefix is p or one of its ancestor directories.
func hasPathPrefix(p, prefix string) bool {
	if prefix == "/" {
		return true
	}
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}
