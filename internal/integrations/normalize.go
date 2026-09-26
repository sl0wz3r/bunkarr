package integrations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// StartupReport is what NormalizeStored did.
type StartupReport struct {
	// Normalized lists the rows whose settings were rewritten: parsed again, unknown fields
	// dropped, missing fields defaulted.
	Normalized []int64
	// Invalid lists the rows whose settings or key are not valid for their type. Each is
	// disabled (enabled = 0) and keeps its settings as they were, for the user to correct.
	Invalid []InvalidIntegration
	// WebhookKeys lists the *arr rows that got a webhook key.
	WebhookKeys []int64
}

// InvalidIntegration is a row NormalizeStored found invalid.
type InvalidIntegration struct {
	ID   int64
	Name string
	Type Type
	// Reason is the validation message.
	Reason string
	// Disabled is true when this start-up disabled the row (false: it was disabled already).
	Disabled bool
}

// PointNormalizeBeforeCommit is where NormalizeStored has written every change of its one
// transaction and not committed yet.
const PointNormalizeBeforeCommit = "integrations.normalizeBeforeCommit"

// NormalizeStored brings rows written before migration 0003 to the Phase 2 rules (design §4.1).
// Phase 1 accepted Sonarr, Radarr, Lidarr, Tautulli, Seerr and Maintainerr rows with any settings
// object. Each such row is parsed again: a valid one gets its normalized settings (unknown fields
// dropped, defaults applied); an invalid one (a Tautulli without plexIntegrationId, a Maintainerr
// with a key, a relative path mapping, ...) is disabled and keeps its settings. Every *arr row
// without a webhook key gets one. Everything is written in one transaction, so a crash leaves the
// rows as they were, and a second run changes nothing. Plex rows are left alone (Phase 1
// validated them).
//
// Run it at start-up before RegisterSecrets; the webhook keys it creates are held in the
// redaction registry and the key map when it returns. The default refresh schedules of the valid
// rows are the caller's (they are schedules rows, mirrored from the settings).
func (s *Store) NormalizeStored(ctx context.Context) (StartupReport, error) {
	type row struct {
		id       int64
		typ      Type
		name     string
		settings string
		hasKey   bool
		enabled  bool
		hasHook  bool
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	var (
		rep  StartupReport
		keys = map[int64]string{}
		typs = map[int64]Type{}
	)
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		rep = StartupReport{}
		clear(keys)
		rs, err := tx.QueryContext(ctx, `SELECT id, type, name, settings, api_key <> '', enabled, webhook_key <> ''
			FROM integrations WHERE type <> 'plex' ORDER BY id`)
		if err != nil {
			return fmt.Errorf("read integrations: %w", err)
		}
		var list []row
		for rs.Next() {
			var r row
			var typ string
			if err := rs.Scan(&r.id, &typ, &r.name, &r.settings, &r.hasKey, &r.enabled, &r.hasHook); err != nil {
				_ = rs.Close()
				return fmt.Errorf("read integrations: %w", err)
			}
			r.typ = Type(typ)
			list = append(list, r)
		}
		if err := rs.Err(); err != nil {
			_ = rs.Close()
			return fmt.Errorf("read integrations: %w", err)
		}
		if err := rs.Close(); err != nil {
			return fmt.Errorf("read integrations: %w", err)
		}
		now := db.FormatTime(s.now())
		for _, r := range list {
			norm, err := normalizeOtherSettings(r.typ, []byte(r.settings))
			if err == nil {
				err = checkLinks(ctx, tx, r.typ, norm, r.hasKey)
			}
			switch verr := err.(type) {
			case nil:
				if norm != r.settings {
					if _, err := tx.ExecContext(ctx, `UPDATE integrations SET settings = ?, updated_at = ? WHERE id = ?`, norm, now, r.id); err != nil {
						return fmt.Errorf("normalize integration %d: %w", r.id, err)
					}
					rep.Normalized = append(rep.Normalized, r.id)
				}
			case ValidationError:
				rep.Invalid = append(rep.Invalid, InvalidIntegration{ID: r.id, Name: r.name, Type: r.typ, Reason: string(verr), Disabled: r.enabled})
				if r.enabled {
					if _, err := tx.ExecContext(ctx, `UPDATE integrations SET enabled = 0, updated_at = ? WHERE id = ?`, now, r.id); err != nil {
						return fmt.Errorf("disable integration %d: %w", r.id, err)
					}
				}
			default:
				return fmt.Errorf("check integration %d: %w", r.id, err)
			}
			if r.typ.IsArr() && !r.hasHook {
				key, err := s.storeNewWebhookKey(ctx, tx, r.id)
				if err != nil {
					return err
				}
				keys[r.id], typs[r.id] = key, r.typ
				rep.WebhookKeys = append(rep.WebhookKeys, r.id)
			}
		}
		faultinject.Point(PointNormalizeBeforeCommit)
		return nil
	})
	if err != nil {
		return StartupReport{}, fmt.Errorf("normalize integrations: %w", err)
	}
	faultinject.Point(pointBeforeHold)
	for id, key := range keys {
		s.holdWebhookKey(id, typs[id], key)
	}
	return rep, nil
}
