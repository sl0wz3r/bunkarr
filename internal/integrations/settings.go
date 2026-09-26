package integrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
)

// validateCron checks the syntax of a cron expression, as the scheduler parses it.
var validateCron = jobqueue.ValidateCron

// AppName is the product name of an integration type, for messages ("Radarr", "Plex").
func (t Type) AppName() string {
	switch t {
	case TypePlex:
		return "Plex"
	case TypeSonarr:
		return "Sonarr"
	case TypeRadarr:
		return "Radarr"
	case TypeLidarr:
		return "Lidarr"
	case TypeTautulli:
		return "Tautulli"
	case TypeSeerr:
		return "Seerr"
	case TypeMaintainerr:
		return "Maintainerr"
	}
	return string(t)
}

// normalizeOtherSettings validates the settings document of a type other than Plex and returns
// its stored form: parsed (unknown fields dropped, missing fields defaulted) and encoded again.
// References to other integrations are checked by checkLinks, inside the write.
func normalizeOtherSettings(typ Type, raw json.RawMessage) (string, error) {
	var (
		v   any
		err error
	)
	switch {
	case typ.IsArr():
		var s ArrSettings
		if s, err = ParseArrSettings(raw); err == nil {
			err = s.Validate(typ.AppName())
		}
		v = s
	case typ == TypeTautulli:
		var s TautulliSettings
		if s, err = ParseTautulliSettings(raw); err == nil {
			err = s.Validate()
		}
		v = s
	case typ == TypeSeerr:
		var s SeerrSettings
		if s, err = ParseSeerrSettings(raw); err == nil {
			err = s.Validate()
		}
		v = s
	case typ == TypeMaintainerr:
		var s MaintainerrSettings
		if s, err = ParseMaintainerrSettings(raw); err == nil {
			err = s.Validate()
		}
		v = s
	default:
		return "", ValidationError(fmt.Sprintf("unknown integration type %q", typ))
	}
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode %s settings: %w", typ, err)
	}
	return string(b), nil
}

// linkedPlex returns the plexIntegrationId of stored Tautulli, Seerr or Maintainerr settings (0
// for other types or none).
func linkedPlex(typ Type, settings string) int64 {
	var s struct {
		PlexIntegrationID int64 `json:"plexIntegrationId"`
	}
	switch typ {
	case TypeTautulli, TypeSeerr, TypeMaintainerr:
		if json.Unmarshal([]byte(settings), &s) == nil {
			return s.PlexIntegrationID
		}
	}
	return 0
}

// checkLinks checks, inside a write, what a row's settings and key may refer to: the
// plexIntegrationId of Tautulli, Seerr and Maintainerr must name a Plex integration, and a
// Maintainerr integration has no API key (it has no API authentication, S8). hasKey is whether
// the row will have a key after the write.
func checkLinks(ctx context.Context, tx *sql.Tx, typ Type, settings string, hasKey bool) error {
	if typ == TypeMaintainerr && hasKey {
		return ValidationError("Maintainerr has no API authentication: leave the API key empty (clearApiKey removes a saved one)")
	}
	id := linkedPlex(typ, settings)
	if id == 0 {
		return nil
	}
	var linked string
	err := tx.QueryRowContext(ctx, `SELECT type FROM integrations WHERE id = ?`, id).Scan(&linked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ValidationError(fmt.Sprintf("plexIntegrationId: integration %d does not exist", id))
	case err != nil:
		return fmt.Errorf("check plexIntegrationId: %w", err)
	case Type(linked) != TypePlex:
		return ValidationError(fmt.Sprintf("plexIntegrationId: integration %d is a %s integration, not Plex", id, linked))
	}
	return nil
}

// CheckArrIntegration checks that id names a Sonarr, Radarr or Lidarr integration, for
// sources.arr_integration_id (design §4.1): ErrNotFound for an unknown id, a ValidationError for
// another type.
func (s *Store) CheckArrIntegration(ctx context.Context, id int64) (Type, error) {
	it, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if !it.Type.IsArr() {
		return it.Type, ValidationError(fmt.Sprintf("integration %q is a %s integration, not Sonarr, Radarr or Lidarr", it.Name, it.Type.AppName()))
	}
	return it.Type, nil
}
