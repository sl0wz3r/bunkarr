package integrations

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Defaults of the Tautulli, Seerr and Maintainerr refreshes (design §4.2, §12.3).
const (
	DefaultTautulliRefreshCron        = "0 2 * * *"
	DefaultTautulliStaleAfterHours    = 72
	DefaultSeerrRefreshCron           = "30 2 * * *"
	DefaultSeerrStaleAfterHours       = 72
	DefaultMaintainerrRefreshCron     = "45 */6 * * *"
	DefaultMaintainerrStaleAfterHours = 24
)

// TautulliSettings is the settings object of a Tautulli integration.
type TautulliSettings struct {
	// PlexIntegrationID is the Plex integration whose server Tautulli watches (required).
	PlexIntegrationID int64           `json:"plexIntegrationId"`
	Refresh           RefreshSettings `json:"refresh"`
}

// SeerrSettings is the settings object of a Seerr integration.
type SeerrSettings struct {
	// PlexIntegrationID is optional: it enables the rating-key fallback of the requests (§8.2).
	PlexIntegrationID int64           `json:"plexIntegrationId"`
	Refresh           RefreshSettings `json:"refresh"`
}

// MaintainerrSettings is the settings object of a Maintainerr integration. Maintainerr has no
// API authentication, so its integration never has an API key (S8).
type MaintainerrSettings struct {
	// PlexIntegrationID is the Plex integration whose server Maintainerr manages (required).
	PlexIntegrationID int64           `json:"plexIntegrationId"`
	Refresh           RefreshSettings `json:"refresh"`
}

// parseLinked decodes the settings shared by Tautulli, Seerr and Maintainerr into dst (which
// holds the defaults) and normalizes the refresh cron expression.
func parseLinked(raw json.RawMessage, what string, dst any, refresh *RefreshSettings, defaultCron string) error {
	if t := bytes.TrimSpace(raw); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		if err := json.Unmarshal(t, dst); err != nil {
			return ValidationError(what + " settings are not valid: " + jsonProblem(err))
		}
	}
	refresh.normalize(defaultCron)
	return nil
}

// ParseTautulliSettings decodes and normalizes a Tautulli settings document (missing fields take
// the defaults: refresh enabled at 02:00, stale after 72 h; unknown fields are dropped). It does
// not validate; call Validate.
func ParseTautulliSettings(raw json.RawMessage) (TautulliSettings, error) {
	s := TautulliSettings{Refresh: RefreshSettings{Cron: DefaultTautulliRefreshCron, Enabled: true, StaleAfterHours: DefaultTautulliStaleAfterHours}}
	if err := parseLinked(raw, "Tautulli", &s, &s.Refresh, DefaultTautulliRefreshCron); err != nil {
		return TautulliSettings{}, err
	}
	return s, nil
}

// Validate checks the settings: a Plex integration id (whether it names a Plex integration is
// checked by the store) and the refresh settings.
func (s TautulliSettings) Validate() error {
	if s.PlexIntegrationID <= 0 {
		return ValidationError("Tautulli needs the Plex server it watches (plexIntegrationId)")
	}
	return s.Refresh.validate()
}

// ParseSeerrSettings decodes and normalizes a Seerr settings document (refresh enabled at 02:30,
// stale after 72 h by default). It does not validate; call Validate.
func ParseSeerrSettings(raw json.RawMessage) (SeerrSettings, error) {
	s := SeerrSettings{Refresh: RefreshSettings{Cron: DefaultSeerrRefreshCron, Enabled: true, StaleAfterHours: DefaultSeerrStaleAfterHours}}
	if err := parseLinked(raw, "Seerr", &s, &s.Refresh, DefaultSeerrRefreshCron); err != nil {
		return SeerrSettings{}, err
	}
	return s, nil
}

// Validate checks the settings: an optional Plex integration id and the refresh settings.
func (s SeerrSettings) Validate() error {
	if s.PlexIntegrationID < 0 {
		return ValidationError("plexIntegrationId must be a Plex integration id")
	}
	return s.Refresh.validate()
}

// ParseMaintainerrSettings decodes and normalizes a Maintainerr settings document (refresh
// enabled every 6 hours at :45, stale after 24 h by default). It does not validate; call Validate.
func ParseMaintainerrSettings(raw json.RawMessage) (MaintainerrSettings, error) {
	s := MaintainerrSettings{Refresh: RefreshSettings{Cron: DefaultMaintainerrRefreshCron, Enabled: true, StaleAfterHours: DefaultMaintainerrStaleAfterHours}}
	if err := parseLinked(raw, "Maintainerr", &s, &s.Refresh, DefaultMaintainerrRefreshCron); err != nil {
		return MaintainerrSettings{}, err
	}
	return s, nil
}

// Validate checks the settings: a Plex integration id and the refresh settings.
func (s MaintainerrSettings) Validate() error {
	if s.PlexIntegrationID <= 0 {
		return ValidationError("Maintainerr needs the Plex server it manages (plexIntegrationId)")
	}
	return s.Refresh.validate()
}

// TautulliSettings decodes the settings of a Tautulli integration.
func (i Integration) TautulliSettings() (TautulliSettings, error) {
	if i.Type != TypeTautulli {
		return TautulliSettings{}, fmt.Errorf("integration %d is a %s integration, not tautulli", i.ID, i.Type)
	}
	return ParseTautulliSettings(i.Settings)
}

// SeerrSettings decodes the settings of a Seerr integration.
func (i Integration) SeerrSettings() (SeerrSettings, error) {
	if i.Type != TypeSeerr {
		return SeerrSettings{}, fmt.Errorf("integration %d is a %s integration, not seerr", i.ID, i.Type)
	}
	return ParseSeerrSettings(i.Settings)
}

// MaintainerrSettings decodes the settings of a Maintainerr integration.
func (i Integration) MaintainerrSettings() (MaintainerrSettings, error) {
	if i.Type != TypeMaintainerr {
		return MaintainerrSettings{}, fmt.Errorf("integration %d is a %s integration, not maintainerr", i.ID, i.Type)
	}
	return ParseMaintainerrSettings(i.Settings)
}
