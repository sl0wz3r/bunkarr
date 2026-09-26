package destinations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// VerifyMode says how much a verify job re-reads (design §4.5).
type VerifyMode string

// Verify modes.
const (
	VerifyOff    VerifyMode = "off"
	VerifySample VerifyMode = "sample"
	VerifyFull   VerifyMode = "full"
)

// HardlinkMode says what a sync does with the other names of a hardlinked file (design §4.2).
type HardlinkMode string

// Hardlink modes.
const (
	// HardlinksRecreate makes a hardlink at the destination when it supports them.
	HardlinksRecreate HardlinkMode = "recreate"
	// HardlinksCopy never links: the content is stored once, the other names are only recorded.
	HardlinksCopy HardlinkMode = "copy"
)

// AdoptMode says when an unrecorded destination file is adopted instead of displaced (§4.4).
type AdoptMode string

// Adoption modes.
const (
	AdoptSizeMtime AdoptMode = "size+mtime"
	AdoptSizeHash  AdoptMode = "size+hash"
	AdoptOff       AdoptMode = "off"
)

// Verify configures verification.
type Verify struct {
	Mode VerifyMode `json:"mode"`
	// SamplePercent is the share of files a sample verify re-reads (0-100; 0 means the default).
	SamplePercent int `json:"samplePercent"`
}

// Settings are a destination's sync settings (design §7 Destination.settings).
type Settings struct {
	Verify        Verify       `json:"verify"`
	Hardlinks     HardlinkMode `json:"hardlinks"`
	AdoptExisting AdoptMode    `json:"adoptExisting"`
	// MtimeWindowSec is the tolerated mtime difference for adoption (0-3600, like rsync
	// --modify-window).
	MtimeWindowSec int `json:"mtimeWindowSec"`
	// MaxChangePercent and MaxChangeFiles are the mass-change guard limits (S10b).
	MaxChangePercent int `json:"maxChangePercent"`
	MaxChangeFiles   int `json:"maxChangeFiles"`
}

// Retention are a destination's retention periods (design S5, §5).
type Retention struct {
	// DeletedDays is how long files deleted or replaced at the source are kept (1-3650).
	DeletedDays int `json:"deletedDays"`
	// PlexDBDaily is how many daily Plex DB versions are kept (1-365).
	PlexDBDaily int `json:"plexDbDaily"`
	// PlexDBWeekly is how many ISO weeks keep their newest Plex DB version (1-520).
	PlexDBWeekly int `json:"plexDbWeekly"`
	// ArrDaily is how many daily *arr backup versions are kept per integration (1-365; design
	// phase2-3.md §10 step 8).
	ArrDaily int `json:"arrDaily"`
	// ArrWeekly is how many ISO weeks keep their newest *arr backup version (1-520).
	ArrWeekly int `json:"arrWeekly"`
	// ManifestDays is how many days keep their newest manifest version (1-3650; design
	// phase2-3.md §11.2 step 6). internal/manifest reads it from the stored JSON.
	ManifestDays int `json:"manifestDays"`
	// ManifestWeeks is how many ISO weeks keep their newest manifest version (0-520). Unlike
	// the other periods 0 is a value, no weekly versions: only a missing manifestWeeks takes the
	// default (UnmarshalJSON records whether it was present; a Go literal's zero is missing).
	ManifestWeeks int `json:"manifestWeeks"`
	// manifestWeeksSet says ManifestWeeks is a value, not missing: it was in the decoded JSON,
	// or the retention is normalized (so normalizing it again changes nothing).
	manifestWeeksSet bool
}

// Defaults (design §4, §5, S5, S10b).
const (
	DefaultSamplePercent    = 5
	DefaultMaxChangePercent = 10
	DefaultMaxChangeFiles   = 1000
	DefaultDeletedDays      = 30
	DefaultPlexDBDaily      = 14
	DefaultPlexDBWeekly     = 8
	DefaultArrDaily         = 14
	DefaultArrWeekly        = 8
	DefaultManifestDays     = 30
	DefaultManifestWeeks    = 12
)

// DefaultSettings returns the documented default settings.
func DefaultSettings() Settings {
	return Settings{
		Verify:           Verify{Mode: VerifySample, SamplePercent: DefaultSamplePercent},
		Hardlinks:        HardlinksRecreate,
		AdoptExisting:    AdoptSizeMtime,
		MtimeWindowSec:   0,
		MaxChangePercent: DefaultMaxChangePercent,
		MaxChangeFiles:   DefaultMaxChangeFiles,
	}
}

// DefaultRetention returns the documented default retention.
func DefaultRetention() Retention {
	return Retention{DeletedDays: DefaultDeletedDays, PlexDBDaily: DefaultPlexDBDaily, PlexDBWeekly: DefaultPlexDBWeekly,
		ArrDaily: DefaultArrDaily, ArrWeekly: DefaultArrWeekly, ManifestDays: DefaultManifestDays,
		ManifestWeeks: DefaultManifestWeeks, manifestWeeksSet: true}
}

// retentionFields is Retention without its UnmarshalJSON method.
type retentionFields Retention

// decodeRetention decodes retention JSON and records whether manifestWeeks was present. strict
// refuses unknown keys (the API decodes its bodies that way).
func decodeRetention(b []byte, strict bool) (Retention, error) {
	var aux struct {
		retentionFields
		// ManifestWeeks shadows the embedded field, so a present 0 is told from a missing key.
		ManifestWeeks *int `json:"manifestWeeks"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(&aux); err != nil {
		return Retention{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Retention{}, errors.New("invalid character after the retention object")
	}
	r := Retention(aux.retentionFields)
	r.manifestWeeksSet = aux.ManifestWeeks != nil
	if aux.ManifestWeeks != nil {
		r.ManifestWeeks = *aux.ManifestWeeks
	}
	return r, nil
}

// UnmarshalJSON decodes a retention object, refusing unknown keys as the API does; a present
// manifestWeeks keeps its value, 0 included (see ManifestWeeks).
func (r *Retention) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	got, err := decodeRetention(b, true)
	if err != nil {
		return err
	}
	*r = got
	return nil
}

// Normalize fills missing (empty or zero) values with the defaults and validates the rest. An
// invalid value returns a ValidationError.
func (s Settings) Normalize() (Settings, error) {
	d := DefaultSettings()
	switch s.Verify.Mode {
	case "":
		s.Verify.Mode = d.Verify.Mode
	case VerifyOff, VerifySample, VerifyFull:
	default:
		return Settings{}, ValidationError(fmt.Sprintf("verify mode %q: use off, sample or full", s.Verify.Mode))
	}
	if s.Verify.SamplePercent == 0 {
		s.Verify.SamplePercent = d.Verify.SamplePercent
	}
	if s.Verify.SamplePercent < 0 || s.Verify.SamplePercent > 100 {
		return Settings{}, ValidationError(fmt.Sprintf("verify sample percent %d is out of range (0-100)", s.Verify.SamplePercent))
	}
	switch s.Hardlinks {
	case "":
		s.Hardlinks = d.Hardlinks
	case HardlinksRecreate, HardlinksCopy:
	default:
		return Settings{}, ValidationError(fmt.Sprintf("hardlinks %q: use recreate or copy", s.Hardlinks))
	}
	switch s.AdoptExisting {
	case "":
		s.AdoptExisting = d.AdoptExisting
	case AdoptSizeMtime, AdoptSizeHash, AdoptOff:
	default:
		return Settings{}, ValidationError(fmt.Sprintf("adoptExisting %q: use size+mtime, size+hash or off", s.AdoptExisting))
	}
	if s.MtimeWindowSec < 0 || s.MtimeWindowSec > 3600 {
		return Settings{}, ValidationError(fmt.Sprintf("mtime window %d s is out of range (0-3600)", s.MtimeWindowSec))
	}
	if s.MaxChangePercent == 0 {
		s.MaxChangePercent = d.MaxChangePercent
	}
	if s.MaxChangePercent < 1 || s.MaxChangePercent > 100 {
		return Settings{}, ValidationError(fmt.Sprintf("max change percent %d is out of range (1-100)", s.MaxChangePercent))
	}
	if s.MaxChangeFiles == 0 {
		s.MaxChangeFiles = d.MaxChangeFiles
	}
	if s.MaxChangeFiles < 1 {
		return Settings{}, ValidationError(fmt.Sprintf("max change files %d must be at least 1", s.MaxChangeFiles))
	}
	return s, nil
}

// Normalize fills missing (zero) values with the defaults — a retention period is never zero —
// and validates the ranges. An invalid value returns a ValidationError.
func (r Retention) Normalize() (Retention, error) {
	d := DefaultRetention()
	if r.DeletedDays == 0 {
		r.DeletedDays = d.DeletedDays
	}
	if r.PlexDBDaily == 0 {
		r.PlexDBDaily = d.PlexDBDaily
	}
	if r.PlexDBWeekly == 0 {
		r.PlexDBWeekly = d.PlexDBWeekly
	}
	if r.ArrDaily == 0 {
		r.ArrDaily = d.ArrDaily
	}
	if r.ArrWeekly == 0 {
		r.ArrWeekly = d.ArrWeekly
	}
	if r.ManifestDays == 0 {
		r.ManifestDays = d.ManifestDays
	}
	if r.ManifestWeeks == 0 && !r.manifestWeeksSet {
		r.ManifestWeeks = d.ManifestWeeks
	}
	r.manifestWeeksSet = true
	if r.ManifestDays < 1 || r.ManifestDays > 3650 {
		return Retention{}, ValidationError(fmt.Sprintf("manifest daily versions %d is out of range (1-3650)", r.ManifestDays))
	}
	if r.ManifestWeeks < 0 || r.ManifestWeeks > 520 {
		return Retention{}, ValidationError(fmt.Sprintf("manifest weekly versions %d is out of range (0-520; 0 keeps none)", r.ManifestWeeks))
	}
	if r.ArrDaily < 1 || r.ArrDaily > 365 {
		return Retention{}, ValidationError(fmt.Sprintf("*arr backup daily versions %d is out of range (1-365)", r.ArrDaily))
	}
	if r.ArrWeekly < 1 || r.ArrWeekly > 520 {
		return Retention{}, ValidationError(fmt.Sprintf("*arr backup weekly versions %d is out of range (1-520)", r.ArrWeekly))
	}
	if r.DeletedDays < 1 || r.DeletedDays > 3650 {
		return Retention{}, ValidationError(fmt.Sprintf("deleted-file retention %d days is out of range (1-3650)", r.DeletedDays))
	}
	if r.PlexDBDaily < 1 || r.PlexDBDaily > 365 {
		return Retention{}, ValidationError(fmt.Sprintf("Plex DB daily versions %d is out of range (1-365)", r.PlexDBDaily))
	}
	if r.PlexDBWeekly < 0 || r.PlexDBWeekly > 520 {
		return Retention{}, ValidationError(fmt.Sprintf("Plex DB weekly versions %d is out of range (0-520; 0 means the default)", r.PlexDBWeekly))
	}
	return r, nil
}

// ParseSettings reads settings JSON as stored in destinations.settings: missing keys and zero
// values take the defaults ("{}" and "" are the defaults).
func ParseSettings(raw string) (Settings, error) {
	var s Settings
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return Settings{}, fmt.Errorf("parse settings: %w", err)
		}
	}
	return s.Normalize()
}

// ParseRetention reads retention JSON as stored in destinations.retention: missing keys and zero
// values take the defaults ("{}" and "" are the defaults), except a stored manifestWeeks 0.
// Unknown keys are ignored.
func ParseRetention(raw string) (Retention, error) {
	var r Retention
	if raw != "" {
		var err error
		if r, err = decodeRetention([]byte(raw), false); err != nil {
			return Retention{}, fmt.Errorf("parse retention: %w", err)
		}
	}
	return r.Normalize()
}
