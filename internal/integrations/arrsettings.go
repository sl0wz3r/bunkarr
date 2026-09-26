package integrations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Defaults of the *arr settings (design §4.2, §12.3).
const (
	// DefaultArrRefreshCron is the full refresh schedule of an *arr integration.
	DefaultArrRefreshCron = "15 */6 * * *"
	// DefaultArrStaleAfterHours is how long an *arr index stays fresh after a full refresh.
	DefaultArrStaleAfterHours = 24
	// DefaultArrBackupCron is the *arr backup schedule of an enabled backup without one (weekly).
	DefaultArrBackupCron = "30 6 * * 0"
	// DefaultMaxScheduledAgeDays is the age up to which the *arr's own scheduled backup is copied
	// instead of creating a new one (the *arrs' default backup interval).
	DefaultMaxScheduledAgeDays = 7
	// MaxMaxScheduledAgeDays bounds backup.maxScheduledAgeDays.
	MaxMaxScheduledAgeDays = 90
)

// IsArr reports whether t is Sonarr, Radarr or Lidarr.
func (t Type) IsArr() bool {
	return t == TypeSonarr || t == TypeRadarr || t == TypeLidarr
}

// ArrSettings is the settings object of a Sonarr, Radarr or Lidarr integration (design §4.2).
type ArrSettings struct {
	// PathMappings translate the paths the *arr reports into paths inside Bunkarr.
	PathMappings []ArrPathMapping `json:"pathMappings"`
	// BackupFolder is the *arr's Backups directory as Bunkarr sees it (mounted read-only), e.g.
	// /arr/radarr-backups; "" fetches backups over HTTP (design D3).
	BackupFolder string `json:"backupFolder"`
	// Backup configures the scheduled *arr backup (§10).
	Backup ArrBackup `json:"backup"`
	// Refresh configures the scheduled full refresh of the *arr index (§6.1).
	Refresh RefreshSettings `json:"refresh"`
}

// ArrPathMapping maps a path prefix as the *arr sees it to the same directory as Bunkarr sees it.
type ArrPathMapping struct {
	Arr   string `json:"arr"`
	Local string `json:"local"`
}

// MappingPaths implements Mapping.
func (m ArrPathMapping) MappingPaths() (remote, local string) { return m.Arr, m.Local }

// ArrBackup is the *arr backup schedule of an integration.
type ArrBackup struct {
	// DestinationID is the destination the backups are written to (0 = none chosen).
	DestinationID int64 `json:"destinationId"`
	// Cron is a 5-field cron expression; an enabled backup without one gets DefaultArrBackupCron.
	Cron    string `json:"cron"`
	Enabled bool   `json:"enabled"`
	// MaxScheduledAgeDays (1-90, default 7): a scheduled backup of the *arr younger than this is
	// copied, and no Backup command is sent.
	MaxScheduledAgeDays int `json:"maxScheduledAgeDays"`
	// AcceptInsecureModes allows backups to a destination that does not enforce file modes (SMB
	// without POSIX extensions): the zips hold the *arr's secrets (S17).
	AcceptInsecureModes bool `json:"acceptInsecureModes"`
}

// RefreshSettings configures the scheduled refresh of an integration's metadata cache.
type RefreshSettings struct {
	// Cron is a 5-field cron expression ("" takes the type's default).
	Cron    string `json:"cron"`
	Enabled bool   `json:"enabled"`
	// StaleAfterHours (1-720): the cache's facts are unknown this long after its last complete
	// refresh (design D15).
	StaleAfterHours int `json:"staleAfterHours"`
}

// Limits of RefreshSettings.StaleAfterHours.
const (
	MinStaleAfterHours = 1
	MaxStaleAfterHours = 720
)

// defaultArrSettings are the settings of an *arr integration whose settings are empty.
func defaultArrSettings() ArrSettings {
	return ArrSettings{
		PathMappings: []ArrPathMapping{},
		Backup:       ArrBackup{MaxScheduledAgeDays: DefaultMaxScheduledAgeDays},
		Refresh:      RefreshSettings{Cron: DefaultArrRefreshCron, Enabled: true, StaleAfterHours: DefaultArrStaleAfterHours},
	}
}

// ParseArrSettings decodes an *arr settings document ("" and null are the defaults) and
// normalizes it: fields that are missing take their defaults (refresh enabled every 6 hours,
// stale after 24 h; backup disabled, weekly when enabled without a cron expression; scheduled
// backups reused up to 7 days old), surrounding spaces are trimmed and paths cleaned. Unknown
// fields are dropped. It does not validate; call Validate.
func ParseArrSettings(raw json.RawMessage) (ArrSettings, error) {
	s := defaultArrSettings()
	if t := bytes.TrimSpace(raw); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		if err := json.Unmarshal(t, &s); err != nil {
			return ArrSettings{}, ValidationError("*arr settings are not valid: " + jsonProblem(err))
		}
	}
	if s.PathMappings == nil {
		s.PathMappings = []ArrPathMapping{}
	}
	for i := range s.PathMappings {
		s.PathMappings[i].Arr = cleanPath(s.PathMappings[i].Arr)
		s.PathMappings[i].Local = cleanPath(s.PathMappings[i].Local)
	}
	s.BackupFolder = cleanPath(s.BackupFolder)
	s.Backup.Cron = strings.TrimSpace(s.Backup.Cron)
	if s.Backup.Enabled && s.Backup.Cron == "" {
		s.Backup.Cron = DefaultArrBackupCron
	}
	s.Refresh.normalize(DefaultArrRefreshCron)
	return s, nil
}

// normalize trims the cron expression and gives an empty one the type's default.
func (r *RefreshSettings) normalize(defaultCron string) {
	r.Cron = strings.TrimSpace(r.Cron)
	if r.Cron == "" {
		r.Cron = defaultCron
	}
}

// validate checks the refresh settings: a valid cron expression and 1-720 hours.
func (r RefreshSettings) validate() error {
	if r.StaleAfterHours < MinStaleAfterHours || r.StaleAfterHours > MaxStaleAfterHours {
		return ValidationError(fmt.Sprintf("refresh.staleAfterHours must be %d to %d", MinStaleAfterHours, MaxStaleAfterHours))
	}
	if err := validateCron(r.Cron); err != nil {
		return ValidationError("refresh.cron: " + err.Error())
	}
	return nil
}

// Validate checks the settings: the mappings (as for Plex: at most 64, absolute clean paths, each
// *arr prefix once), an absolute backupFolder, the backup (an enabled one needs a destination;
// maxScheduledAgeDays 1-90; a valid cron expression) and the refresh settings. app names the
// application in messages ("Radarr"). That the destination exists and enforces file modes is
// checked where destinations are known (§10).
func (s ArrSettings) Validate(app string) error {
	if app == "" {
		app = "*arr"
	}
	if err := validateMappings(s.PathMappings, app); err != nil {
		return err
	}
	if s.BackupFolder != "" && !validPath(s.BackupFolder) {
		return ValidationError(fmt.Sprintf("backupFolder must be an absolute path such as /arr/%s-backups", strings.ToLower(app)))
	}
	b := s.Backup
	switch {
	case b.DestinationID < 0:
		return ValidationError("backup.destinationId must be a destination id")
	case b.MaxScheduledAgeDays < 1 || b.MaxScheduledAgeDays > MaxMaxScheduledAgeDays:
		return ValidationError(fmt.Sprintf("backup.maxScheduledAgeDays must be 1 to %d", MaxMaxScheduledAgeDays))
	case b.Enabled && b.DestinationID == 0:
		return ValidationError(fmt.Sprintf("scheduled %s backups need a destination", app))
	case b.Enabled && b.Cron == "":
		return ValidationError(fmt.Sprintf("scheduled %s backups need a schedule (cron)", app))
	}
	if b.Cron != "" {
		if err := validateCron(b.Cron); err != nil {
			return ValidationError("backup.cron: " + err.Error())
		}
	}
	return s.Refresh.validate()
}

// MapPath translates a path as the *arr reports it into the path inside Bunkarr (see MapPath).
func (s ArrSettings) MapPath(arrPath string) (local string, ok bool) {
	return MapPath(s.PathMappings, arrPath)
}

// ArrSettings decodes the settings of a Sonarr, Radarr or Lidarr integration.
func (i Integration) ArrSettings() (ArrSettings, error) {
	if !i.Type.IsArr() {
		return ArrSettings{}, fmt.Errorf("integration %d is a %s integration, not an *arr", i.ID, i.Type)
	}
	return ParseArrSettings(i.Settings)
}
